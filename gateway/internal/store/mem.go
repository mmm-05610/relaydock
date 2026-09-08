package store

import (
	"fmt"
	"sync"
	"time"

	"gateway/internal/config"
	"gateway/internal/keys"
)

// MemStore 内存实现（开发/测试用）：与 PgStore 同等的管理面能力，
// 本地无 DATABASE_URL 时渠道/模型/账号 CRUD 全功能可用，重启清零。
type MemStore struct {
	mu         sync.RWMutex
	keys       map[string]*keys.Key
	upstream   map[string][]byte
	logs       []UsageLog
	channels   []config.Channel
	accounts   []UpstreamAccount
	nextID     int64
	accountSeq int64
}

func NewMemStore() *MemStore {
	return &MemStore{
		keys:     make(map[string]*keys.Key),
		upstream: make(map[string][]byte),
	}
}

func (s *MemStore) CreateKey(k keys.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	k.ID = s.nextID
	cp := k
	s.keys[k.KeyHash] = &cp
	return nil
}

func (s *MemStore) GetKeyByHash(hash string) (*keys.Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys[hash], nil
}

func (s *MemStore) ListKeys() ([]keys.Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]keys.Key, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, *k)
	}
	return out, nil
}

func (s *MemStore) RevokeKey(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[hash]; ok {
		k.Enabled = false
	}
	return nil
}

func (s *MemStore) AddQuotaUsed(hash string, amount float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[hash]; ok {
		k.QuotaUsed += amount
	}
	return nil
}

func (s *MemStore) UpdateQuota(hash string, limit float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[hash]; ok {
		k.QuotaLimit = limit
	}
	return nil
}

func (s *MemStore) UpdateKey(k keys.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.keys[k.KeyHash]; ok {
		old.Name = k.Name
		old.Owner = k.Owner
		old.AgentType = k.AgentType
		old.QuotaLimit = k.QuotaLimit
		old.AllowedModels = k.AllowedModels
	}
	return nil
}

func (s *MemStore) SetUpstreamKey(provider string, encrypted []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstream[provider] = encrypted
	return nil
}

func (s *MemStore) GetUpstreamKey(provider string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.upstream[provider], nil
}

func (s *MemStore) InsertUsageLog(log UsageLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}
	s.logs = append(s.logs, log)
	return nil
}

func (s *MemStore) GetUsageStats() (UsageStats, error) {
	return UsageStats{}, nil // 内存模式无数据
}

func (s *MemStore) GetDashboard() (DashboardStats, error) {
	return DashboardStats{}, nil
}

func (s *MemStore) GetTimeseries(r TimeRange) ([]TimeseriesPoint, error) {
	return nil, nil
}

func (s *MemStore) GetGrouped(by string, r TimeRange) ([]GroupedUsage, error) {
	return nil, nil
}

func (s *MemStore) QueryLogs(filter LogFilter) ([]UsageLog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 返回最新的 filter.Limit 条（简易实现，开发/测试用）
	n := len(s.logs)
	if filter.Limit > 0 && filter.Limit < n {
		n = filter.Limit
	}
	out := make([]UsageLog, n)
	copy(out, s.logs[len(s.logs)-n:])
	// 倒序（最新在前，与 PG 查询语义一致）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// --- ChannelStore 实现 ---

func (s *MemStore) LoadChannels() ([]config.Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]config.Channel, len(s.channels))
	copy(out, s.channels)
	return out, nil
}

func (s *MemStore) CreateChannel(ch config.Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.channels {
		if c.Provider == ch.Provider {
			return fmt.Errorf("channel %q already exists", ch.Provider)
		}
	}
	s.nextID++
	ch.ID = s.nextID
	for i := range ch.Models {
		ch.Models[i].Provider = ch.Provider
	}
	s.channels = append(s.channels, ch)
	return nil
}

func (s *MemStore) UpdateChannel(ch config.Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.channels {
		if s.channels[i].Provider == ch.Provider {
			ch.ID = s.channels[i].ID
			for j := range ch.Models {
				ch.Models[j].Provider = ch.Provider
			}
			s.channels[i] = ch
			return nil
		}
	}
	return fmt.Errorf("channel %q not found", ch.Provider)
}

func (s *MemStore) DeleteChannel(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.channels {
		if s.channels[i].Provider == provider {
			s.channels = append(s.channels[:i], s.channels[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("channel %q not found", provider)
}

func (s *MemStore) findChannelLocked(provider string) *config.Channel {
	for i := range s.channels {
		if s.channels[i].Provider == provider {
			return &s.channels[i]
		}
	}
	return nil
}

func (s *MemStore) CreateModel(provider string, m config.Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.findChannelLocked(provider)
	if ch == nil {
		return fmt.Errorf("channel %q not found", provider)
	}
	for i := range ch.Models {
		if ch.Models[i].Name == m.Name {
			return fmt.Errorf("model %q already exists", m.Name)
		}
	}
	m.Provider = provider
	ch.Models = append(ch.Models, m)
	return nil
}

func (s *MemStore) UpdateModel(provider string, m config.Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.findChannelLocked(provider)
	if ch == nil {
		return fmt.Errorf("channel %q not found", provider)
	}
	for i := range ch.Models {
		if ch.Models[i].Name == m.Name {
			m.Provider = provider
			ch.Models[i] = m
			return nil
		}
	}
	return fmt.Errorf("model %q not found", m.Name)
}

func (s *MemStore) DeleteModel(provider, modelName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.findChannelLocked(provider)
	if ch == nil {
		return fmt.Errorf("channel %q not found", provider)
	}
	for i := range ch.Models {
		if ch.Models[i].Name == modelName {
			ch.Models = append(ch.Models[:i], ch.Models[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("model %q not found", modelName)
}

// --- AccountStore 实现 ---

func (s *MemStore) ListUpstreamAccounts() ([]UpstreamAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]UpstreamAccount, len(s.accounts))
	copy(out, s.accounts)
	return out, nil
}

func (s *MemStore) CreateUpstreamAccount(a *UpstreamAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountSeq++
	a.ID = s.accountSeq
	s.accounts = append(s.accounts, *a)
	return nil
}

func (s *MemStore) UpdateUpstreamAccount(a UpstreamAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == a.ID {
			s.accounts[i].Name = a.Name
			s.accounts[i].EncryptedKey = a.EncryptedKey
			s.accounts[i].KeyFingerprint = a.KeyFingerprint
			s.accounts[i].MaxConcurrency = a.MaxConcurrency
			s.accounts[i].Enabled = a.Enabled
			return nil
		}
	}
	return fmt.Errorf("account %d not found", a.ID)
}

func (s *MemStore) DeleteUpstreamAccount(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("account %d not found", id)
}

func (s *MemStore) SetUpstreamToken(id int64, encryptedToken []byte, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			s.accounts[i].EncryptedToken = encryptedToken
			s.accounts[i].TokenExpiresAt = expiresAt
			s.accounts[i].LastRefreshAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("account %d not found", id)
}

// AccountUsageStats 从内存 usage 日志聚合（days 窗口）。
func (s *MemStore) AccountUsageStats(days int) ([]AccountUsage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().AddDate(0, 0, -days)
	byAccount := map[int64]*AccountUsage{}
	for _, l := range s.logs {
		if l.AccountID <= 0 || l.CreatedAt.Before(cutoff) {
			continue
		}
		st, ok := byAccount[l.AccountID]
		if !ok {
			st = &AccountUsage{AccountID: l.AccountID}
			byAccount[l.AccountID] = st
		}
		st.Requests++
		st.Cost += l.Cost
		st.Tokens += l.InputTokens + l.OutputTokens + l.CacheReadTokens + l.CacheWriteTokens
		st.AvgAttempts += float64(l.Attempts)
		if l.Status >= 400 {
			st.Errors++
		}
	}
	out := make([]AccountUsage, 0, len(byAccount))
	for _, st := range byAccount {
		st.AvgAttempts /= float64(st.Requests)
		out = append(out, *st)
	}
	return out, nil
}
