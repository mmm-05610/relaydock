package keys

import (
	"fmt"
	"strings"
)

// Key 虚拟 key 记录。
type Key struct {
	ID            int64
	KeyHash       string
	Name          string
	Owner         string // 谁拥有（多用户归属）
	AgentType     string
	QuotaLimit    float64 // USD，0 = 不限
	QuotaUsed     float64
	Enabled       bool
	AllowedModels string // 允许访问的模型（逗号分隔，空=不限）
}

// CanAccessModel 检查该 key 是否允许访问指定模型。
func (k *Key) CanAccessModel(model string) bool {
	if k.AllowedModels == "" {
		return true
	}
	for _, m := range strings.Split(k.AllowedModels, ",") {
		if strings.TrimSpace(m) == model {
			return true
		}
	}
	return false
}

// Store key 存储接口（内存实现先，部署切 PG）。
type Store interface {
	CreateKey(k Key) error
	GetKeyByHash(hash string) (*Key, error)
	ListKeys() ([]Key, error)
	RevokeKey(hash string) error                    // 吊销（enabled=false）
	AddQuotaUsed(hash string, amount float64) error // 累加额度（post-call）
	UpdateQuota(hash string, limit float64) error   // 编辑额度上限
	UpdateKey(k Key) error                          // 编辑 key 元数据（name/owner/agent/额度/模型权限）
}

// Manager 虚拟 key 管理器。
type Manager struct {
	store Store
}

func NewManager(store Store) *Manager {
	return &Manager{store: store}
}

// CreateKey 签发虚拟 key，返回明文（仅此一次，之后只存哈希）。
func (m *Manager) CreateKey(name, owner, agentType string, quota float64) (string, error) {
	return m.CreateKeyWithModels(name, owner, agentType, quota, "")
}

// CreateKeyWithModels 签发虚拟 key（带允许的模型列表）。
func (m *Manager) CreateKeyWithModels(name, owner, agentType string, quota float64, allowedModels string) (string, error) {
	raw, err := GenerateKey()
	if err != nil {
		return "", err
	}
	k := Key{
		KeyHash:       SHA256Hash(raw),
		Name:          name,
		Owner:         owner,
		AgentType:     agentType,
		QuotaLimit:    quota,
		Enabled:       true,
		AllowedModels: allowedModels,
	}
	if err := m.store.CreateKey(k); err != nil {
		return "", err
	}
	return raw, nil
}

// Authenticate 校验 bearer key，含额度硬挡（pre-call）。
func (m *Manager) Authenticate(raw string) (*Key, error) {
	k, err := m.store.GetKeyByHash(SHA256Hash(raw))
	if err != nil {
		return nil, err
	}
	if k == nil || !k.Enabled {
		return nil, fmt.Errorf("unauthorized")
	}
	if k.QuotaLimit > 0 && k.QuotaUsed >= k.QuotaLimit {
		return nil, fmt.Errorf("quota exceeded")
	}
	return k, nil
}

// RevokeByHash 吊销虚拟 key（enabled=false，保留历史）。
func (m *Manager) RevokeByHash(hash string) error {
	return m.store.RevokeKey(hash)
}

// ListKeys 列出所有虚拟 key。
func (m *Manager) ListKeys() ([]Key, error) {
	return m.store.ListKeys()
}

// GetKey 按 hash 查单个 key。
func (m *Manager) GetKey(hash string) (*Key, error) {
	return m.store.GetKeyByHash(hash)
}

// Rotate 轮换 key：生成新 key（保留 name/owner/agent/quota），禁用旧 key，返回新明文。
func (m *Manager) Rotate(hash string) (string, error) {
	old, err := m.store.GetKeyByHash(hash)
	if err != nil {
		return "", err
	}
	if old == nil {
		return "", fmt.Errorf("key not found")
	}
	raw, err := GenerateKey()
	if err != nil {
		return "", err
	}
	newKey := Key{
		KeyHash:    SHA256Hash(raw),
		Name:       old.Name,
		Owner:      old.Owner,
		AgentType:  old.AgentType,
		QuotaLimit: old.QuotaLimit,
		Enabled:    true,
	}
	if err := m.store.CreateKey(newKey); err != nil {
		return "", err
	}
	if err := m.store.RevokeKey(hash); err != nil {
		return "", err
	}
	return raw, nil
}

// UpdateQuota 编辑额度上限。
func (m *Manager) UpdateQuota(hash string, limit float64) error {
	return m.store.UpdateQuota(hash, limit)
}

// UpdateKey 编辑 key 元数据。
func (m *Manager) UpdateKey(k Key) error {
	return m.store.UpdateKey(k)
}

// AddUsage 累加额度（计量落库时调用）。
func (m *Manager) AddUsage(hash string, cost float64) error {
	return m.store.AddQuotaUsed(hash, cost)
}
