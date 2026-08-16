package store

import (
	"sync"

	"gateway/internal/config"
	"gateway/internal/keys"
)

// MemStore 内存实现（开发用，部署切 PG）。
type MemStore struct {
	mu       sync.RWMutex
	keys     map[string]*keys.Key
	upstream map[string][]byte
	nextID   int64
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
	return nil // 内存模式不落库（开发用）
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
	return nil, nil
}

func (s *MemStore) LoadChannels() ([]config.Channel, error) {
	return nil, nil
}
func (s *MemStore) CreateChannel(ch config.Channel) error { return nil }
func (s *MemStore) UpdateChannel(ch config.Channel) error { return nil }
func (s *MemStore) DeleteChannel(provider string) error    { return nil }
func (s *MemStore) CreateModel(provider string, m config.Model) error { return nil }
func (s *MemStore) UpdateModel(provider string, m config.Model) error { return nil }
func (s *MemStore) DeleteModel(provider, modelName string) error       { return nil }
