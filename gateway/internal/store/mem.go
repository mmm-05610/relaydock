package store

import (
	"fmt"
	"sort"
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
	settings   map[string]string
	bodies     []LogBody
	bodiesSeq  int64
	nextID     int64
	accountSeq int64
}

func NewMemStore() *MemStore {
	return &MemStore{
		keys:     make(map[string]*keys.Key),
		upstream: make(map[string][]byte),
		settings: make(map[string]string),
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
		old.ExpiresAt = k.ExpiresAt
	}
	return nil
}

// TouchLastUsed 记录虚拟 key 最后使用时间。
func (s *MemStore) TouchLastUsed(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[hash]; ok {
		now := time.Now()
		k.LastUsedAt = &now
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
	} // 非零（seed 指定历史时间）保留
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

// GetUsageOverview 从内存日志聚合（<=2 天小时粒度，其余天粒度）。
func (s *MemStore) GetUsageOverview(days int) (*UsageOverview, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().AddDate(0, 0, -days)
	gran := 24 * time.Hour
	if days <= 2 {
		gran = time.Hour
	}
	ov := &UsageOverview{Series: []UsageOverviewPoint{}}
	buckets := map[string]*UsageOverviewPoint{}
	byModel := map[string]*UsageDistribution{}
	byKey := map[string]*UsageDistribution{}

	for _, l := range s.logs {
		if !l.CreatedAt.IsZero() && l.CreatedAt.Before(cutoff) {
			continue
		}
		ov.Summary.Requests++
		tok := l.InputTokens + l.OutputTokens + l.CacheReadTokens + l.CacheWriteTokens
		ov.Summary.InputTokens += l.InputTokens
		ov.Summary.OutputTokens += l.OutputTokens
		ov.Summary.CacheReadTokens += l.CacheReadTokens
		ov.Summary.CacheWriteTokens += l.CacheWriteTokens
		ov.Summary.Tokens += tok
		ov.Summary.Cost += l.Cost
		if l.Status >= 400 {
			ov.Summary.Errors++
		}
		// 时间桶
		trunc := l.CreatedAt.Truncate(gran)
		key := trunc.Format("2006-01-02T15:04:05Z07:00")
		b := buckets[key]
		if b == nil {
			b = &UsageOverviewPoint{Date: key}
			buckets[key] = b
		}
		b.Requests++
		b.Tokens += tok
		b.Cost += l.Cost
		if l.Status >= 400 {
			b.Errors++
		}
		// 分布
		md := byModel[l.Model]
		if md == nil {
			md = &UsageDistribution{Group: l.Model}
			byModel[l.Model] = md
		}
		md.Requests++
		md.Tokens += tok
		md.Cost += l.Cost
		kd := byKey[fmt.Sprint(l.KeyID)]
		if kd == nil {
			kd = &UsageDistribution{Group: fmt.Sprint(l.KeyID)}
			byKey[fmt.Sprint(l.KeyID)] = kd
		}
		kd.Requests++
		kd.Tokens += tok
		kd.Cost += l.Cost
	}
	if ov.Summary.Requests > 0 {
		ov.Summary.SuccessRate = float64(ov.Summary.Requests-ov.Summary.Errors) / float64(ov.Summary.Requests)
	}
	for _, b := range buckets {
		ov.Series = append(ov.Series, *b)
	}
	sort.Slice(ov.Series, func(i, j int) bool { return ov.Series[i].Date < ov.Series[j].Date })
	ov.ByModel = topDistributions(byModel)
	ov.ByKey = topDistributions(byKey)
	return ov, nil
}

// topDistributions Top5 + Other 归并（按请求数排序）。
func topDistributions(m map[string]*UsageDistribution) []UsageDistribution {
	all := make([]UsageDistribution, 0, len(m))
	for _, d := range m {
		all = append(all, *d)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Requests > all[j].Requests })
	if len(all) <= 6 {
		return all
	}
	top := all[:5]
	other := UsageDistribution{Group: "其他"}
	for _, d := range all[5:] {
		other.Requests += d.Requests
		other.Tokens += d.Tokens
		other.Cost += d.Cost
	}
	return append(top, other)
}

func (s *MemStore) QueryLogs(filter LogFilter) ([]UsageLog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 返回最新的 filter.Limit 条（简易实现，开发/测试用）
	n := len(s.logs)
	if filter.Limit > 0 && filter.Limit < n {
		n = filter.Limit
	}
	var filtered []UsageLog
	for _, l := range s.logs {
		if filter.KeyID > 0 && l.KeyID != filter.KeyID {
			continue
		}
		if filter.Model != "" && l.Model != filter.Model {
			continue
		}
		if filter.Status > 0 && l.Status != filter.Status {
			continue
		}
		if filter.FailedOnly && l.Status < 400 {
			continue
		}
		if filter.RequestID != "" && l.RequestID != filter.RequestID {
			continue
		}
		if filter.Days > 0 && !l.CreatedAt.IsZero() && l.CreatedAt.Before(time.Now().AddDate(0, 0, -filter.Days)) {
			continue
		}
		filtered = append(filtered, l)
	}
	s.logs = filtered // 无意义赋值防未用？——不，保留原 logs
	_ = filtered
	total := len(s.logs)
	_ = total
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

// --- 设置与全文日志（内存上限 500 条，超出丢最旧） ---

func (s *MemStore) GetSetting(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings[key], nil
}

func (s *MemStore) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[key] = value
	return nil
}

func (s *MemStore) InsertLogBody(b *LogBody) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodiesSeq++
	b.ID = s.bodiesSeq
	b.CreatedAt = time.Now()
	s.bodies = append(s.bodies, *b)
	if len(s.bodies) > 500 {
		s.bodies = s.bodies[len(s.bodies)-500:]
	}
	return nil
}

func (s *MemStore) GetLogBodyByRequestID(usageRequestID string) (*LogBody, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.bodies) - 1; i >= 0; i-- {
		if s.bodies[i].UsageRequestID == usageRequestID {
			b := s.bodies[i]
			return &b, nil
		}
	}
	return nil, nil
}

func (s *MemStore) CleanupLogBodies(olderThan time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []LogBody
	var removed int64
	for _, b := range s.bodies {
		if b.CreatedAt.Before(olderThan) {
			removed++
			continue
		}
		kept = append(kept, b)
	}
	s.bodies = kept
	return removed, nil
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
