package gateway

// 账号池故障注入测试（docs/design-upstream-account-pool.md §13 HTTP 部分）。
//
// 拓扑与生产一致：一个渠道 → 一个上游地址（服务器），多个账号共用同一路由，
// 上游按 Authorization header（cred-acct-<i>）区分到达的账号。
// 账号经 setAccountSpecs 注入快照（生产路径由 RebuildSnapshot 从 PG 构建）；
// 归因断言通过 MemStore 的 usage logs 读取。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gateway/internal/config"
	"gateway/internal/pool"
	"gateway/internal/routing"
	"gateway/internal/store"
)

// ---- 装配 ----

// setupPoolTest 装配一个带 n 个账号的渠道 + 一个模拟上游。
// 上游凭 Authorization header 区分账号；handler 收到的请求已带
// "Bearer cred-acct-<i>"。返回的 gateway/ch/srv 供断言使用。
func setupPoolTest(t *testing.T, n int, maxConcurrency int64, handler http.HandlerFunc) (*Gateway, *config.Channel, *httptest.Server) {
	t.Helper()
	const chID = int64(7)

	srv := newUpstreamServer(t, handler)
	var specs []*pool.AccountSpec
	for i := 0; i < n; i++ {
		specs = append(specs, &pool.AccountSpec{
			ID:             100 + int64(i),
			ChannelID:      chID,
			Name:           fmt.Sprintf("acct-%d", i),
			Credential:     fmt.Sprintf("cred-acct-%d", i),
			MaxConcurrency: maxConcurrency,
			Enabled:        true,
		})
	}
	routes := map[string]config.Route{
		"/v1/messages": {Upstream: srv.URL + "/up/anthropic/messages", Model: "upstream-a", Usage: "anthropic"},
	}
	ch := testChannel("deepseek", "", testModel("model-a", routes))
	ch.ID = chID

	setupGatewayTest(t, []config.Channel{ch}, "")
	testGW.setAccountSpecs(specs)
	return testGW, &ch, srv
}

func findAccountRef(t *testing.T, g *Gateway, ch *config.Channel, id int64) *pool.AccountRef {
	t.Helper()
	for _, ref := range g.Snapshots.Load().AccountsFor(ch) {
		if ref.ID() == id {
			return ref
		}
	}
	t.Fatalf("account %d not in snapshot", id)
	return nil
}

func mustVirtualKey(t *testing.T, g *Gateway) string {
	t.Helper()
	raw, err := g.KeyMgr.CreateKey("pool-test", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

// usageLogs 读取内存库的用量记录（最新在前）。
func usageLogs(t *testing.T, g *Gateway) []store.UsageLog {
	t.Helper()
	db, ok := g.DB.(*store.MemStore)
	if !ok {
		t.Fatalf("test DB is %T, want *store.MemStore", g.DB)
	}
	logs, err := db.QueryLogs(store.LogFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

// assertLastUsage 断言最近一条用量记录。
func assertLastUsage(t *testing.T, g *Gateway, check func(rec store.UsageLog)) {
	t.Helper()
	logs := usageLogs(t, g)
	if len(logs) == 0 {
		t.Fatal("no usage logs recorded")
	}
	check(logs[0])
}

// ---- 1. 并发容量：4 槽满后第 5 个本地快速失败 429 ----

func TestPoolCapacityLocal429(t *testing.T) {
	var started atomic.Int32
	hold := make(chan struct{})
	handler := func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-hold
		_, _ = w.Write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}\n\n"))
	}

	g, ch, _ := setupPoolTest(t, 2, 2, handler)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthropicReqBody))
			r.Header.Set("Authorization", "Bearer "+mustVirtualKey(t, g))
			g.handleProxy(w, r)
		}()
	}
	waitFor(t, func() bool { return started.Load() == 4 }, 2*time.Second)

	// 第 5 个请求：无空闲槽 → 本地 429
	w := performProxyRequest(t, "/v1/messages", mustVirtualKey(t, g), anthropicReqBody)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 local; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("local 429 missing Retry-After")
	}
	var errBody struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("local error body not json: %v", err)
	}
	if errBody.Error.Type != "gateway_rate_limited" {
		t.Fatalf("error type = %q, want gateway_rate_limited", errBody.Error.Type)
	}

	close(hold)
	wg.Wait()

	waitFor(t, func() bool {
		total := int64(0)
		for _, ref := range g.Snapshots.Load().AccountsFor(ch) {
			total += ref.Status().Inflight
		}
		return total == 0
	}, 2*time.Second)
}

// ---- 2. 429 failover：第一个尝试的账号冷却，切到另一个 ----

func TestPoolFailover429SwitchesAccount(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	var auth429 atomic.Value // 第一个尝试的账号（返回 429）
	var auth200 atomic.Value // 第二个尝试的账号（返回 200）
	handler := func(w http.ResponseWriter, r *http.Request) {
		cred := r.Header.Get("Authorization")
		mu.Lock()
		hits[cred]++
		mu.Unlock()
		if auth429.Load() == nil {
			auth429.Store(cred)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		if auth200.Load() != nil {
			t.Error("more than two upstream attempts")
			return
		}
		auth200.Store(cred)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicRespBody))
	}

	g, ch, _ := setupPoolTest(t, 2, 0, handler)

	w := performProxyRequest(t, "/v1/messages", mustVirtualKey(t, g), anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover; body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	distinct := len(hits)
	mu.Unlock()
	if distinct != 2 {
		t.Fatalf("distinct accounts hit = %d, want 2", distinct)
	}

	// 凭据 → 账号
	byCred := map[string]*pool.AccountRef{}
	for _, ref := range g.Snapshots.Load().AccountsFor(ch) {
		byCred["Bearer "+ref.Spec.Credential] = ref
	}
	ref429 := byCred[auth429.Load().(string)]
	ref200 := byCred[auth200.Load().(string)]

	if st := ref429.Status(); !time.Now().Before(st.CoolingUntil) {
		t.Fatalf("429 account not cooling: %+v", st)
	}
	assertLastUsage(t, g, func(rec store.UsageLog) {
		if rec.AccountID != ref200.Spec.ID {
			t.Fatalf("attribution account_id = %d, want %d (the 200 account)", rec.AccountID, ref200.Spec.ID)
		}
		if rec.Attempts != 2 {
			t.Fatalf("attempts = %d, want 2", rec.Attempts)
		}
	})
	if got := g.Pool.Counters(); got.Failovers != 1 || got.Cooldowns != 1 {
		t.Fatalf("counters = %+v, want 1 failover / 1 cooldown", got)
	}
}

// ---- 3. 429 后换账号，槽不泄漏 ----

func TestPoolFailoverClosesFailedBody(t *testing.T) {
	var auth429 atomic.Value
	handler := func(w http.ResponseWriter, r *http.Request) {
		if auth429.Load() == nil {
			auth429.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicRespBody))
	}
	g, ch, _ := setupPoolTest(t, 2, 0, handler)

	w := performProxyRequest(t, "/v1/messages", mustVirtualKey(t, g), anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	byCred := map[string]*pool.AccountRef{}
	for _, ref := range g.Snapshots.Load().AccountsFor(ch) {
		byCred["Bearer "+ref.Spec.Credential] = ref
	}
	ref429 := byCred[auth429.Load().(string)]
	waitFor(t, func() bool { return ref429.Status().Inflight == 0 }, 2*time.Second)
}

// ---- 4. 5xx 不换账号、不冷却 ----

func TestPoolServerErrorNoSwitch(t *testing.T) {
	var hits atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}
	g, ch, _ := setupPoolTest(t, 2, 0, handler)

	w := performProxyRequest(t, "/v1/messages", mustVirtualKey(t, g), anthropicReqBody)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 passthrough", w.Code)
	}
	if w.Body.String() != `{"error":{"message":"boom"}}` {
		t.Fatalf("body = %q, want verbatim upstream error", w.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hit %d times, want 1 (no switch on 5xx)", hits.Load())
	}
	for _, ref := range g.Snapshots.Load().AccountsFor(ch) {
		if st := ref.Status(); !st.CoolingUntil.IsZero() {
			t.Fatalf("account %d cooled on 5xx: %+v", ref.ID(), st)
		}
	}
	assertLastUsage(t, g, func(rec store.UsageLog) {
		if rec.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no retry on 5xx)", rec.Attempts)
		}
	})
}

// ---- 5. 网络错误不换账号、不冷却 ----

func TestPoolNetworkErrorNoSwitch(t *testing.T) {
	dead := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request) {})
	deadURL := dead.URL
	dead.Close()

	g, ch, _ := setupPoolTest(t, 2, 0, func(w http.ResponseWriter, r *http.Request) {})
	refA := findAccountRef(t, g, ch, 100)

	// 路由整体指向死地址（深拷贝替换，不污染旧快照）
	g.Snapshots.Update(func(cur *routing.Snapshot) *routing.Snapshot {
		cfg := config.Config{Channels: append([]config.Channel(nil), cur.Config.Channels...)}
		for i := range cfg.Channels {
			cfg.Channels[i].Models = append([]config.Model(nil), cfg.Channels[i].Models...)
			for j := range cfg.Channels[i].Models {
				cfg.Channels[i].Models[j].Routes = map[string]config.Route{
					"/v1/messages": {Upstream: deadURL + "/up/anthropic/messages", Model: "upstream-a", Usage: "anthropic"},
				}
			}
		}
		return cur.WithConfig(&cfg)
	})

	w := performProxyRequest(t, "/v1/messages", mustVirtualKey(t, g), anthropicReqBody)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if st := refA.Status(); !st.CoolingUntil.IsZero() {
		t.Fatalf("account cooled on network error: %+v", st)
	}
	assertLastUsage(t, g, func(rec store.UsageLog) {
		if rec.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no retry on network error)", rec.Attempts)
		}
	})
}

// ---- 6. 凭据失效（401）换账号 ----

func TestPoolCredentialInvalidSwitches(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	var auth401 atomic.Value // string：第一个尝试的账号凭据（返回 401）
	handler := func(w http.ResponseWriter, r *http.Request) {
		cred := r.Header.Get("Authorization")
		mu.Lock()
		hits[cred]++
		mu.Unlock()
		if auth401.Load() == nil {
			auth401.Store(cred)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicRespBody))
	}
	g, ch, _ := setupPoolTest(t, 2, 0, handler)

	w := performProxyRequest(t, "/v1/messages", mustVirtualKey(t, g), anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover; body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	total := len(hits)
	mu.Unlock()
	if total != 2 {
		t.Fatalf("distinct accounts hit = %d, want 2 (one 401, then switch)", total)
	}

	// 返回 401 的那个账号被冷却
	cred401 := auth401.Load().(string)
	var cooled *pool.AccountRef
	for _, ref := range g.Snapshots.Load().AccountsFor(ch) {
		if "Bearer "+ref.Spec.Credential == cred401 {
			cooled = ref
		}
	}
	if cooled == nil || !time.Now().Before(cooled.Status().CoolingUntil) {
		t.Fatal("the 401 account is not cooling")
	}
	assertLastUsage(t, g, func(rec store.UsageLog) {
		if rec.Attempts != 2 {
			t.Fatalf("attempts = %d, want 2", rec.Attempts)
		}
	})
}

// ---- 7. SSE 中途断流：不换账号、不拼接第二个响应 ----

func TestPoolSSEMidstreamDisconnectNoSwitch(t *testing.T) {
	var hits atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		// 无论先命中哪个账号：SSE 提交后都不允许第二次上游尝试
		if hits.Add(1) > 1 {
			t.Error("second upstream attempt made after SSE committed")
			return
		}
		// 模拟上游在 SSE 中途断流
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack support", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"part1\"}}\n\n"))
		// 直接断开
	}
	g, _, _ := setupPoolTest(t, 2, 0, handler)

	w := performProxyRequest(t, "/v1/messages", mustVirtualKey(t, g), anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE already committed)", w.Code)
	}
	got := dataLines(t, w.Body.String())
	if len(got) != 1 || !strings.Contains(got[0], "part1") {
		t.Fatalf("events = %v, want single part1", got)
	}
}

// ---- 8. 客户端取消：上游取消 + 槽归还 ----

func TestPoolClientCancelReleasesSlot(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"first\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // 等客户端取消
	}
	g, ch, _ := setupPoolTest(t, 1, 0, handler)
	refA := findAccountRef(t, g, ch, 100)

	gw := newUpstreamServer(t, http.HandlerFunc(g.handleProxy))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(anthropicReqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+mustVirtualKey(t, g))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.Contains(line, "first") {
		t.Fatalf("first SSE line = %q", line)
	}
	cancel()
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	waitFor(t, func() bool { return refA.Status().Inflight == 0 }, 3*time.Second)
}

// ---- 9. 全部账号禁用 → 503，不请求上游 ----

func TestPoolAllDisabledLocal503(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit when all accounts disabled")
	}
	g, _, _ := setupPoolTest(t, 1, 0, handler)
	g.setAccountSpecs([]*pool.AccountSpec{{ID: 100, ChannelID: 7, Name: "acct-0", Credential: "c", Enabled: false}})

	w := performProxyRequest(t, "/v1/messages", mustVirtualKey(t, g), anthropicReqBody)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// ---- 10. 热更新与持续请求并发运行（-race 观察窗口） ----

func TestPoolReloadWhileRequestsInFlight(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicRespBody))
	}
	g, _, _ := setupPoolTest(t, 2, 0, handler)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthropicReqBody))
				r.Header.Set("Authorization", "Bearer "+mustVirtualKey(t, g))
				g.handleProxy(w, r)
			}
		}()
	}
	for i := 0; i < 20; i++ {
		g.setAccountSpecs([]*pool.AccountSpec{
			{ID: 100 + int64(i%3), ChannelID: 7, Name: fmt.Sprintf("acct-%d", i), Credential: "c", Enabled: true},
		})
	}
	close(stop)
	wg.Wait()
}
