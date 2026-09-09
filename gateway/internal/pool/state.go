// Package pool 上游账号池：每账号独立凭据与并发容量，原子占用、幂等释放、
// 429 冷却与保守 failover 分类。第一版不做权重/优先级/sticky/RPM/TPM。
//
// 配置态（AccountSpec）随快照发布整体重建、发布后不可变；
// 运行态（AccountState）以 account_id 稳定关联，热更新时同 ID 复用，
// 在途请求持有的旧 AccountRef/Lease 在其结束后正常归还。
package pool

import (
	"sync"
	"time"
)

// AccountSpec 账号配置（不可变快照的一部分）。
type AccountSpec struct {
	ID             int64
	ChannelID      int64
	Name           string
	Credential     string // 进程内明文，不序列化、不打日志（oauth 型 = 快照预热时的 access token fallback）
	MaxConcurrency int64  // 0 = 不限
	Enabled        bool
	CredentialType string // api_key(空值等同) | oauth（凭据经 credentialFor 动态解析）
	OAuthProfile   string
}

// AccountState 账号运行状态（进程内，重启清零——运行态不落库是设计决策，
// 持久化的只有人工启停；见 docs/design-upstream-account-pool.md §2）。
type AccountState struct {
	mu            sync.Mutex
	inflight      int64
	coolingUntil  time.Time
	cooldownCause string

	// 观测计数（管理面展示；成功/失败按"该账号承载的每次上游尝试"计）
	successCount        int64
	failureCount        int64
	consecutiveFailures int64
	lastStatusCode      int
	lastError           string
	lastFailureClass    string // rate_limited | credential | quota | server_error | transport | client_error
	lastUsedAt          time.Time
}

// RecordResult 数据面每次上游尝试结束后回写观测计数。
// status<=0 表示传输层失败（未拿到响应）。
func (s *AccountState) RecordResult(status int, failureClass string, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsedAt = time.Now()
	if failureClass == "" {
		s.successCount++
		s.consecutiveFailures = 0
		s.lastStatusCode = status
		s.lastError = ""
		s.lastFailureClass = ""
		return
	}
	s.failureCount++
	s.lastStatusCode = status
	s.lastError = errMsg
	s.lastFailureClass = failureClass
	// client_error 是请求侧问题，不代表账号健康恶化，不计入连续失败
	if failureClass != "client_error" {
		s.consecutiveFailures++
	}
}

// Recover 手动恢复：清冷却与失败状态（不清累计计数）。仅冷却中的账号调用合法。
func (s *AccountState) Recover() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.coolingUntil = time.Time{}
	s.cooldownCause = ""
	s.consecutiveFailures = 0
	s.lastStatusCode = 0
	s.lastError = ""
	s.lastFailureClass = ""
}

// AccountRef 配置与运行状态的组合，随快照发布；请求全程持有同一只读引用。
type AccountRef struct {
	Spec  *AccountSpec
	State *AccountState
}

func (r *AccountRef) ID() int64 { return r.Spec.ID }

// tryAcquire 检查与递增必须在同一把锁内完成，不能 Load 后再 Add。
// 拒绝条件：人工禁用 / 冷却中 / 并发槽满。
func (s *AccountState) tryAcquire(spec *AccountSpec, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !spec.Enabled || now.Before(s.coolingUntil) {
		return false
	}
	if spec.MaxConcurrency > 0 && s.inflight >= spec.MaxConcurrency {
		return false
	}
	s.inflight++
	return true
}

// release 归还并发槽；inflight 永不小于 0。
func (s *AccountState) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight > 0 {
		s.inflight--
	}
}

// coolUntil 施加冷却（仅当新的截止更晚时生效，避免短冷却覆盖长冷却）。
func (s *AccountState) coolUntil(t time.Time, cause string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.After(s.coolingUntil) {
		s.coolingUntil = t
		s.cooldownCause = cause
	}
}

// Status 运行时摘要（管理面只读）。
type Status struct {
	Inflight            int64     `json:"inflight"`
	CoolingUntil        time.Time `json:"cooling_until"` // zero = 未冷却
	CooldownCause       string    `json:"cooldown_cause"`
	SuccessCount        int64     `json:"success_count"`
	FailureCount        int64     `json:"failure_count"`
	ConsecutiveFailures int64     `json:"consecutive_failures"`
	LastStatusCode      int       `json:"last_status_code"`
	LastError           string    `json:"last_error"`
	LastFailureClass    string    `json:"last_failure_class"`
	LastUsedAt          time.Time `json:"last_used_at"`
}

func (r *AccountRef) Status() Status {
	r.State.mu.Lock()
	defer r.State.mu.Unlock()
	return Status{
		Inflight:            r.State.inflight,
		CoolingUntil:        r.State.coolingUntil,
		CooldownCause:       r.State.cooldownCause,
		SuccessCount:        r.State.successCount,
		FailureCount:        r.State.failureCount,
		ConsecutiveFailures: r.State.consecutiveFailures,
		LastStatusCode:      r.State.lastStatusCode,
		LastError:           r.State.lastError,
		LastFailureClass:    r.State.lastFailureClass,
		LastUsedAt:          r.State.lastUsedAt,
	}
}

// IsCooling 当前是否在冷却期。
func (r *AccountRef) IsCooling(now time.Time) bool {
	r.State.mu.Lock()
	defer r.State.mu.Unlock()
	return now.Before(r.State.coolingUntil)
}

// Lease 一次账号占用。Release 必须幂等（sync.Once）；
// 每次 attempt 在独立作用域内获取并释放，不能把多个 attempt 的
// defer 全堆到最外层 handler。
type Lease struct {
	ref  *AccountRef
	once sync.Once
}

// Release 归还并发槽，可安全多次调用。
func (l *Lease) Release() {
	l.once.Do(func() { l.ref.State.release() })
}

// Account 返回承载本次尝试的账号。
func (l *Lease) Account() *AccountRef { return l.ref }
