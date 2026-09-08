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
	Credential     string // 进程内明文，不序列化、不打日志
	MaxConcurrency int64  // 0 = 不限
	Enabled        bool
}

// AccountState 账号运行状态（进程内，重启清零）。
type AccountState struct {
	mu            sync.Mutex
	inflight      int64
	coolingUntil  time.Time
	cooldownCause string
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
	Inflight      int64     `json:"inflight"`
	CoolingUntil  time.Time `json:"cooling_until"` // zero = 未冷却
	CooldownCause string    `json:"cooldown_cause"`
}

func (r *AccountRef) Status() Status {
	r.State.mu.Lock()
	defer r.State.mu.Unlock()
	return Status{
		Inflight:      r.State.inflight,
		CoolingUntil:  r.State.coolingUntil,
		CooldownCause: r.State.cooldownCause,
	}
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
