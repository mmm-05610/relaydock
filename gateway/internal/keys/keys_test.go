package keys

import (
	"testing"
	"time"
)

// TestRotateInheritsConstraints 轮换必须继承全部约束（模型白名单/过期/已用额度）。
func TestRotateInheritsConstraints(t *testing.T) {
	st := NewMemStoreForTest()
	m := NewManager(st)
	raw, err := m.CreateKeyWithModels("old", "owner", "agent", 10, "model-a,model-b", *newTestTime(2026, 12, 31))
	if err != nil {
		t.Fatal(err)
	}
	hash := SHA256Hash(raw)
	if err := m.AddUsage(hash, 3.5); err != nil {
		t.Fatal(err)
	}

	newRaw, err := m.Rotate(hash)
	if err != nil {
		t.Fatal(err)
	}
	nk, _ := m.GetKey(SHA256Hash(newRaw))
	if nk == nil {
		t.Fatal("new key not found")
	}
	if nk.AllowedModels != "model-a,model-b" {
		t.Fatalf("AllowedModels lost: %q", nk.AllowedModels)
	}
	if nk.ExpiresAt == nil {
		t.Fatal("ExpiresAt lost")
	}
	if nk.QuotaUsed != 3.5 {
		t.Fatalf("QuotaUsed reset: %v", nk.QuotaUsed)
	}
	if nk.QuotaLimit != 10 {
		t.Fatalf("QuotaLimit lost: %v", nk.QuotaLimit)
	}
	if old, _ := m.GetKey(hash); old == nil || old.Enabled {
		t.Fatal("old key should be revoked")
	}
}

// memTestStore keys.Store 的最小内存实现（测试用）。
type memTestStore struct {
	keys map[string]*Key
	seq  int64
}

func NewMemStoreForTest() Store {
	return &memTestStore{keys: map[string]*Key{}}
}

func (m *memTestStore) CreateKey(k Key) error {
	m.seq++
	k.ID = m.seq
	cp := k
	m.keys[k.KeyHash] = &cp
	return nil
}

func (m *memTestStore) GetKeyByHash(hash string) (*Key, error) { return m.keys[hash], nil }
func (m *memTestStore) ListKeys() ([]Key, error) {
	out := make([]Key, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, *k)
	}
	return out, nil
}
func (m *memTestStore) RevokeKey(hash string) error {
	if k, ok := m.keys[hash]; ok {
		k.Enabled = false
	}
	return nil
}
func (m *memTestStore) AddQuotaUsed(hash string, amount float64) error {
	if k, ok := m.keys[hash]; ok {
		k.QuotaUsed += amount
	}
	return nil
}
func (m *memTestStore) UpdateQuota(hash string, limit float64) error {
	if k, ok := m.keys[hash]; ok {
		k.QuotaLimit = limit
	}
	return nil
}
func (m *memTestStore) UpdateKey(k Key) error {
	if old, ok := m.keys[k.KeyHash]; ok {
		old.Name, old.Owner, old.AgentType = k.Name, k.Owner, k.AgentType
		old.QuotaLimit, old.AllowedModels = k.QuotaLimit, k.AllowedModels
		old.ExpiresAt = k.ExpiresAt
	}
	return nil
}

func newTestTime(y int, mo int, d int) *time.Time {
	return newTestTimePtr(y, mo, d)
}

func newTestTimePtr(y, mo, d int) *time.Time {
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	return &t
}
