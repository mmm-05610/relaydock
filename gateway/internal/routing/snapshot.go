package routing

import (
	"sync"
	"sync/atomic"

	"gateway/internal/config"
	"gateway/internal/pool"
)

// Snapshot 一次发布的完整路由视图：渠道配置 + 上游凭据 + 显式账号池。
// 发布后不可变。每个数据面请求在开始时 Load 一次，
// 整个请求生命周期只用该版本（热更新期间的在途请求不受影响）。
type Snapshot struct {
	Version uint64
	Config  *config.Config // 渠道 + 模型路由
	// UpstreamKeys provider -> 明文 key（仅进程内，不序列化、不落日志）。
	// 只服务「没有显式账号」的渠道（隐式单 key 兼容路径）。
	UpstreamKeys map[string]string
	// Accounts channel_id -> 该渠道的显式账号（发布时经 Pool.Reconcile 关联运行态）。
	// 空 = 走 UpstreamKeys 隐式路径。
	Accounts map[int64][]*pool.AccountRef
}

// AccountsFor 取渠道的显式账号列表（可能为空）。
func (s *Snapshot) AccountsFor(ch *config.Channel) []*pool.AccountRef {
	return s.Accounts[ch.ID]
}

// FindModel 按客户端 model 名查配置（first-match，只认启用渠道 + 启用模型）。
func (s *Snapshot) FindModel(name string) *config.Model {
	return s.Config.FindModel(name)
}

// FindChannel 按 provider 查渠道。
func (s *Snapshot) FindChannel(provider string) *config.Channel {
	return s.Config.FindChannel(provider)
}

// UpstreamKey 取该渠道的上游明文 key，未配置返回空串。
func (s *Snapshot) UpstreamKey(provider string) string {
	return s.UpstreamKeys[provider]
}

// WithConfig 保留当前凭据与账号、替换渠道配置（热更新 reload 用）。
func (s *Snapshot) WithConfig(cfg *config.Config) *Snapshot {
	return &Snapshot{Config: cfg, UpstreamKeys: s.UpstreamKeys, Accounts: s.Accounts}
}

// WithUpstreamKey 克隆凭据表并更新一个 provider 的 key（管理面录入 key 用）。
func (s *Snapshot) WithUpstreamKey(provider, key string) *Snapshot {
	keys := make(map[string]string, len(s.UpstreamKeys)+1)
	for k, v := range s.UpstreamKeys {
		keys[k] = v
	}
	keys[provider] = key
	return &Snapshot{Config: s.Config, UpstreamKeys: keys, Accounts: s.Accounts}
}

// WithAccounts 替换账号池视图（配置变更/录入账号后重建快照用）。
func (s *Snapshot) WithAccounts(accounts map[int64][]*pool.AccountRef) *Snapshot {
	return &Snapshot{Config: s.Config, UpstreamKeys: s.UpstreamKeys, Accounts: accounts}
}

// Store Snapshot 的原子发布器。数据面 Load 无锁；控制面 Update 串行化，
// 避免并发管理请求基于同一旧版本各发一份、互相覆盖。
type Store struct {
	mu      sync.Mutex
	current atomic.Pointer[Snapshot]
}

// NewStore 发布初始快照。
func NewStore(cfg *config.Config, upstreamKeys map[string]string) *Store {
	s := &Store{}
	s.current.Store(&Snapshot{Version: 1, Config: cfg, UpstreamKeys: upstreamKeys})
	return s
}

// Load 取当前快照（请求生命周期内只调用一次并复用返回值）。
func (s *Store) Load() *Snapshot {
	return s.current.Load()
}

// Update 基于当前快照构建并原子发布新版本；build 返回的快照 Version 由 Update 统一递增。
func (s *Store) Update(build func(cur *Snapshot) *Snapshot) *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.current.Load()
	next := build(cur)
	if next == nil {
		return cur
	}
	next.Version = cur.Version + 1
	s.current.Store(next)
	return next
}
