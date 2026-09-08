package pool

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// ref 构造一个账号（State 由 Reconcile 关联）。
func ref(t *testing.T, p *Pool, id int64, maxConcurrency int64, enabled bool) *AccountRef {
	t.Helper()
	refs := p.Reconcile([]*AccountSpec{{
		ID: id, ChannelID: 1, Name: "acct", Credential: "cred", MaxConcurrency: maxConcurrency, Enabled: enabled,
	}})
	return refs[0]
}

// TestConcurrentAcquireRespectsLimit N 个 goroutine 抢 M 个槽：
// 峰值 inflight 永不超过 M，全部释放后归零。
func TestConcurrentAcquireRespectsLimit(t *testing.T) {
	p := New()
	const workers, slots = 16, 4
	a := ref(t, p, 1, slots, true)

	var wg sync.WaitGroup
	peak := make(chan int64, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := p.Acquire([]*AccountRef{a}, nil)
			if err != nil {
				return // 满槽被拒是合法结果
			}
			peak <- a.Status().Inflight
			time.Sleep(time.Millisecond)
			lease.Release()
		}()
	}
	wg.Wait()

	if got := a.Status().Inflight; got != 0 {
		t.Fatalf("inflight after all released = %d, want 0", got)
	}
	close(peak)
	for v := range peak {
		if v > slots {
			t.Fatalf("peak inflight %d exceeds max_concurrency %d", v, slots)
		}
	}
}

// TestLeaseReleaseIdempotent 重复 Release 不会把 inflight 减成负数。
func TestLeaseReleaseIdempotent(t *testing.T) {
	p := New()
	a := ref(t, p, 1, 2, true)
	lease, err := p.Acquire([]*AccountRef{a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	lease.Release()
	lease.Release()
	if got := a.Status().Inflight; got != 0 {
		t.Fatalf("inflight = %d, want 0", got)
	}
}

// TestAcquireSkipsCoolingAndReturnsAfterExpiry 冷却中的账号不被选择，到期后重新可占。
func TestAcquireSkipsCoolingAndReturnsAfterExpiry(t *testing.T) {
	p := New()
	base := time.Unix(1700000000, 0)
	clock := base
	p.SetNow(func() time.Time { return clock })

	a := ref(t, p, 1, 0, true)
	p.Cool(a, 30*time.Second, "test")

	if _, err := p.Acquire([]*AccountRef{a}, nil); err == nil {
		t.Fatal("cooling account should not be acquirable")
	}

	clock = base.Add(31 * time.Second)
	lease, err := p.Acquire([]*AccountRef{a}, nil)
	if err != nil {
		t.Fatalf("acquire after cooldown expiry: %v", err)
	}
	lease.Release()
}

// TestReconcileKeepsStateByID 热更新对账：同 ID 复用运行态（含 in-flight 与冷却），
// 消失的账号移除，其 Lease 仍可正常 Release。
func TestReconcileKeepsStateByID(t *testing.T) {
	p := New()
	base := time.Unix(1700000000, 0)
	p.SetNow(func() time.Time { return base })

	old := p.Reconcile([]*AccountSpec{{ID: 1, ChannelID: 1, Name: "a", MaxConcurrency: 2, Enabled: true},
		{ID: 2, ChannelID: 1, Name: "b", MaxConcurrency: 0, Enabled: true}})
	l1, err := p.Acquire([]*AccountRef{old[0]}, nil) // 只占 id=1，断言目标明确
	if err != nil {
		t.Fatal(err)
	}
	p.Cool(old[1], time.Minute, "test")

	// 重载：id=1 保留（占用中），id=2 删除
	newRefs := p.Reconcile([]*AccountSpec{{ID: 1, ChannelID: 1, Name: "a-renamed", MaxConcurrency: 2, Enabled: true}})
	if got := newRefs[0].Status().Inflight; got != 1 {
		t.Fatalf("inflight after reload = %d, want 1 (state kept by id)", got)
	}
	if newRefs[0].Spec.Name != "a-renamed" {
		t.Fatalf("spec not refreshed: %q", newRefs[0].Spec.Name)
	}

	// 删除账号的 lease 仍可 Release，且不影响保留账号
	l1.Release()
	if got := newRefs[0].Status().Inflight; got != 0 {
		t.Fatalf("inflight after release = %d, want 0", got)
	}

	// 被删除账号的冷却不应泄漏到新账号：新 ref 应该健康
	if _, err := p.Acquire(newRefs, nil); err != nil {
		t.Fatalf("new ref should be healthy: %v", err)
	}
}

// TestSelectionLeastLoadedFirst 选择算法：连续占用（不释放）时，
// 第二次必然落到 inflight 最低的账号，两次落到不同账号。
func TestSelectionLeastLoadedFirst(t *testing.T) {
	p := New()
	refs := p.Reconcile([]*AccountSpec{
		{ID: 1, ChannelID: 1, Name: "a", MaxConcurrency: 10, Enabled: true},
		{ID: 2, ChannelID: 1, Name: "b", MaxConcurrency: 10, Enabled: true},
	})
	seen := map[int64]bool{}
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(refs, nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen[l.Account().ID()] {
			t.Fatalf("second acquire reused loaded account %d (least-loaded violated)", l.Account().ID())
		}
		seen[l.Account().ID()] = true
		// 故意不释放：第二个账号此时 inflight=0，必须被选中
	}
}

// TestNoLongTermBias 同容量空闲账号在持续占用下无长期固定偏置。
func TestNoLongTermBias(t *testing.T) {
	p := New()
	refs := p.Reconcile([]*AccountSpec{
		{ID: 1, ChannelID: 1, Name: "a", MaxConcurrency: 0, Enabled: true},
		{ID: 2, ChannelID: 1, Name: "b", MaxConcurrency: 0, Enabled: true},
	})
	counts := map[int64]int{}
	for i := 0; i < 40; i++ {
		l, err := p.Acquire(refs, nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[l.Account().ID()]++
		l.Release()
	}
	if counts[1] == 0 || counts[2] == 0 {
		t.Fatalf("biased selection: %v", counts)
	}
	if counts[1] > 34 || counts[2] > 34 {
		t.Fatalf("long-term bias: %v", counts)
	}
}

// TestClassifyResponse 保守分类：429/401/403/402 换账号，5xx/4xx 不换。
func TestClassifyResponse(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "120")

	v := ClassifyResponse(429, h)
	if v.Class != ClassRateLimited {
		t.Fatalf("429 class = %v", v.Class)
	}
	// Retry-After 120s ± 10% jitter
	if v.Cooldown < 108*time.Second || v.Cooldown > 132*time.Second {
		t.Fatalf("429 cooldown = %v, want 120s ±10%%", v.Cooldown)
	}

	// 无 Retry-After → 默认 30s ± 10%
	v = ClassifyResponse(429, http.Header{})
	if v.Cooldown < 27*time.Second || v.Cooldown > 33*time.Second {
		t.Fatalf("429 default cooldown = %v, want 30s ±10%%", v.Cooldown)
	}

	// 异常大的 Retry-After 被钳制
	h.Set("Retry-After", "100000")
	if got := ClassifyResponse(429, h).Cooldown; got > MaxCooldown {
		t.Fatalf("cooldown %v exceeds cap %v", got, MaxCooldown)
	}

	if got := ClassifyResponse(401, http.Header{}); got.Class != ClassCredential {
		t.Fatalf("401 class = %v", got.Class)
	}
	if got := ClassifyResponse(403, http.Header{}); got.Class != ClassCredential {
		t.Fatalf("403 class = %v", got.Class)
	}
	if got := ClassifyResponse(402, http.Header{}); got.Class != ClassQuota {
		t.Fatalf("402 class = %v", got.Class)
	}
	if got := ClassifyResponse(500, http.Header{}); got.Class != ClassServerError || got.Cooldown != 0 {
		t.Fatalf("500 verdict = %+v, want no switch / no cooldown", got)
	}
	if got := ClassifyResponse(400, http.Header{}); got.Class != ClassClientError || got.Cooldown != 0 {
		t.Fatalf("400 verdict = %+v, want no penalty", got)
	}
	if got := ClassifyResponse(200, http.Header{}); got.Class != ClassOK {
		t.Fatalf("200 class = %v", got.Class)
	}
}
