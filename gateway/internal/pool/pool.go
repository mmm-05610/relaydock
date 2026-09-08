package pool

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNoCandidates 没有可用账号（全部禁用/冷却/已尝试）→ 本地 503。
	ErrNoCandidates = errors.New("pool: no available upstream account")
	// ErrAtCapacity 存在健康账号但并发槽全满 → 本地 429（带 Retry-After）。
	ErrAtCapacity = errors.New("pool: all upstream accounts at capacity")
)

// Pool 账号运行时注册表 + 选择器 + 观测计数。
type Pool struct {
	mu     sync.Mutex
	states map[int64]*AccountState
	cursor atomic.Uint64 // 同分轮换游标，避免总偏向配置中的第一个账号
	nowFn  func() time.Time

	rejections atomic.Int64 // 本地容量拒绝次数
	cooldowns  atomic.Int64 // 上游触发的冷却次数
	failovers  atomic.Int64 // 换账号重试次数
	stickyHits atomic.Int64 // 会话粘性命中次数

	affinity map[string]affinityEntry // 会话键 -> 账号（KV 缓存亲和）
}

const (
	// AffinityTTL 会话亲和保持时长：覆盖一个 coding 会话的典型时长。
	AffinityTTL = time.Hour
)

type affinityEntry struct {
	ref      *AccountRef
	lastSeen time.Time
}

func New() *Pool {
	return &Pool{states: map[int64]*AccountState{}, nowFn: time.Now, affinity: map[string]affinityEntry{}}
}

// SetNow 替换时钟（测试用）。
func (p *Pool) SetNow(fn func() time.Time) { p.nowFn = fn }

// Reconcile 按新一份配置对账运行状态：同 ID 复用 State、新增建 State、
// 消失的从注册表移除（在途请求持有的旧引用仍可正常 Release）。
// 返回与 specs 顺序一致的 refs。
func (p *Pool) Reconcile(specs []*AccountSpec) []*AccountRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	refs := make([]*AccountRef, len(specs))
	live := make(map[int64]bool, len(specs))
	for i, spec := range specs {
		live[spec.ID] = true
		st, ok := p.states[spec.ID]
		if !ok {
			st = &AccountState{}
			p.states[spec.ID] = st
		}
		refs[i] = &AccountRef{Spec: spec, State: st}
	}
	for id := range p.states {
		if !live[id] {
			delete(p.states, id)
		}
	}
	return refs
}

// Acquire 从候选里选一个健康且有容量的账号并占用并发槽。
//
// exclude 是本请求已尝试过的账号。排序只决定尝试顺序：
// 按占用比例（inflight/max_concurrency，无限并发用 inflight）从低到高，
// 同分块按进程内游标轮转；最终容量判定由 tryAcquire 在锁内原子完成。
func (p *Pool) Acquire(refs []*AccountRef, exclude map[int64]bool, prefer *AccountRef) (*Lease, error) {
	type cand struct {
		ref   *AccountRef
		score float64
	}
	now := p.nowFn()
	var cands []cand
	for _, ref := range refs {
		spec := ref.Spec
		if exclude[spec.ID] || !spec.Enabled {
			continue
		}
		st := ref.Status()
		if now.Before(st.CoolingUntil) {
			continue
		}
		score := float64(st.Inflight)
		if spec.MaxConcurrency > 0 {
			score = float64(st.Inflight) / float64(spec.MaxConcurrency)
		}
		cands = append(cands, cand{ref, score})
	}
	if len(cands) == 0 {
		return nil, ErrNoCandidates
	}

	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score < cands[j].score })
	// 与最低分同分的块按游标轮转起点，避免空闲账号总是偏向配置第一个
	if k := len(cands); k > 1 {
		best := cands[0].score
		tie := 1
		for tie < k && cands[tie].score == best {
			tie++
		}
		if tie > 1 {
			start := int(p.cursor.Add(1) % uint64(tie))
			block := append([]cand(nil), cands[:tie]...)
			copy(cands[:tie-start], block[start:])
			copy(cands[tie-start:], block[:start])
		}
	}

	// 会话粘性：优先尝试上次服务的账号（吃上游 prompt cache）。
	// tryAcquire 内含冷却/禁用/容量校验，不健康则自然落回常规选择。
	if prefer != nil && !exclude[prefer.Spec.ID] {
		if prefer.State.tryAcquire(prefer.Spec, now) {
			p.stickyHits.Add(1)
			return &Lease{ref: prefer}, nil
		}
	}
	for _, c := range cands {
		if c.ref.State.tryAcquire(c.ref.Spec, now) {
			return &Lease{ref: c.ref}, nil
		}
	}
	p.rejections.Add(1)
	return nil, ErrAtCapacity
}

// Affinity 取会话绑定的账号（TTL 过期或不存在返回 nil）。
func (p *Pool) Affinity(sessionKey string, now time.Time) *AccountRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.affinity[sessionKey]
	if !ok {
		return nil
	}
	if now.Sub(e.lastSeen) > AffinityTTL {
		delete(p.affinity, sessionKey)
		return nil
	}
	return e.ref
}

// BindAffinity 会话成功结束后绑定账号，顺带清理过期条目。
func (p *Pool) BindAffinity(sessionKey string, ref *AccountRef, now time.Time) {
	if sessionKey == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.affinity {
		if now.Sub(e.lastSeen) > AffinityTTL {
			delete(p.affinity, k)
		}
	}
	p.affinity[sessionKey] = affinityEntry{ref: ref, lastSeen: now}
}

// Cool 对账号施加冷却并计数。
func (p *Pool) Cool(ref *AccountRef, d time.Duration, cause string) {
	if d <= 0 {
		return
	}
	ref.State.coolUntil(p.nowFn().Add(d), cause)
	p.cooldowns.Add(1)
}

// IncFailover 数据面发生一次换账号重试时计数。
func (p *Pool) IncFailover() { p.failovers.Add(1) }

// Counters 可观测性计数快照。
type Counters struct {
	Rejections int64 `json:"capacity_rejections"`
	Cooldowns  int64 `json:"cooldowns"`
	Failovers  int64 `json:"failovers"`
	StickyHits int64 `json:"sticky_hits"`
}

func (p *Pool) Counters() Counters {
	return Counters{Rejections: p.rejections.Load(), Cooldowns: p.cooldowns.Load(), Failovers: p.failovers.Load(), StickyHits: p.stickyHits.Load()}
}
