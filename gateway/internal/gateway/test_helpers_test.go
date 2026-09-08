package gateway

// 特征测试（characterization tests）辅助装配。
//
// 这些测试锁定当前数据面（handleProxy / handleModels）的外部行为，
// 作为账号池拆分（Executor / Resolver / Relay）之前的回归安全网。
//
// 注意：每个测试通过 setupGatewayTest 装配独立的 Gateway 实例，
// 因此所有测试都不要使用 t.Parallel()。

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gateway/internal/config"
	"gateway/internal/keys"
	"gateway/internal/routing"
	"gateway/internal/store"
)

// testGW 当前测试的 Gateway 实例（setupGatewayTest 设置；测试串行执行）。
var testGW *Gateway

// setupGatewayTest 用内存存储装配一套可用的网关：
//   - cfg 使用传入的渠道（生产路径由 config.yaml/PG 加载，此处直接构造等价内存状态）；
//   - 每个渠道 provider 自动配一个上游 key："upstream-key-<provider>"；
//   - KeyMgr 使用 MemStore（不触碰任何数据库），Meter 使用真实 Meter；
//   - DB = nil（内存模式：recordUsage 不落库）。
//
// 返回签发的虚拟 key 明文（allowedModels 为逗号分隔，空 = 不限）。
// 需要特殊上游 key 状态（如缺失）的测试，用 setUpstreamKey 覆盖。
func setupGatewayTest(t *testing.T, channels []config.Channel, allowedModels string) string {
	t.Helper()

	// 与 config.Load / PG LoadChannels 一致：模型的 Provider 继承自渠道
	for i := range channels {
		for j := range channels[i].Models {
			channels[i].Models[j].Provider = channels[i].Provider
		}
	}

	upstreamKeys := map[string]string{}
	for _, ch := range channels {
		if _, ok := upstreamKeys[ch.Provider]; !ok {
			upstreamKeys[ch.Provider] = "upstream-key-" + ch.Provider
		}
	}
	mem := store.NewMemStore()
	keyMgr := keys.NewManager(mem)
	testGW = New(&config.Config{Channels: channels}, upstreamKeys, keyMgr, mem, "", "")
	raw, err := keyMgr.CreateKeyWithModels("test-key", "test-owner", "test-agent", 0, allowedModels)
	if err != nil {
		t.Fatalf("create virtual key: %v", err)
	}
	return raw
}

// setUpstreamKey 覆盖某渠道的上游 key（发布新快照；空串 = 模拟未配置）。
func setUpstreamKey(t *testing.T, provider, key string) {
	t.Helper()
	testGW.Snapshots.Update(func(cur *routing.Snapshot) *routing.Snapshot {
		return cur.WithUpstreamKey(provider, key)
	})
}

// upstreamCapture 模拟上游收到的单个请求快照。
type upstreamCapture struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// upstreamRecorder 线程安全地按到达顺序记录上游收到的所有请求。
type upstreamRecorder struct {
	mu   sync.Mutex
	reqs []upstreamCapture
}

func (ur *upstreamRecorder) add(c upstreamCapture) {
	ur.mu.Lock()
	defer ur.mu.Unlock()
	ur.reqs = append(ur.reqs, c)
}

func (ur *upstreamRecorder) count() int {
	ur.mu.Lock()
	defer ur.mu.Unlock()
	return len(ur.reqs)
}

func (ur *upstreamRecorder) mustAt(t *testing.T, i int) upstreamCapture {
	t.Helper()
	ur.mu.Lock()
	defer ur.mu.Unlock()
	if i < 0 || i >= len(ur.reqs) {
		t.Fatalf("upstream request #%d not captured (count=%d)", i, len(ur.reqs))
	}
	return ur.reqs[i]
}

// readCapture 把收到的请求转成快照（读空 body）。
func readCapture(r *http.Request) upstreamCapture {
	body, _ := io.ReadAll(r.Body)
	return upstreamCapture{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	}
}

// newUpstreamServer 启动模拟上游并注册 t.Cleanup 关闭。
// 注意：显式绑定 IPv6 回环 [::1]。本仓库开发环境（WSL2 mirrored 网络）下
// 127.0.0.1 的连接会被拒绝，[::1] 正常；httptest.NewServer 默认绑 127.0.0.1。
// （数据面 client 已显式 Proxy: nil，不再受 *_proxy 环境变量影响。）
func newUpstreamServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatalf("listen test upstream: %v", err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: handler}}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// upstreamJSONHandler 记录请求后按固定 status/header/body 应答（非流式）。
func upstreamJSONHandler(ur *upstreamRecorder, status int, header http.Header, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ur.add(readCapture(r))
		for k, vv := range header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// upstreamSSEHandler 记录请求后按 event-stream 逐行写出（每行 Flush），模拟 SSE 上游。
// lines 中的空字符串表示事件之间的空行。
func upstreamSSEHandler(ur *upstreamRecorder, contentType string, lines []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ur.add(readCapture(r))
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, ln := range lines {
			_, _ = w.Write([]byte(ln + "\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// disconnectAfterPrefix 劫持连接手写 SSE 前缀后立即断开，模拟上游在 SSE 中途断流
// （不写终止事件、不写 chunked 结尾）。
func disconnectAfterPrefix(ur *upstreamRecorder, prefixLines []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ur.add(readCapture(r))
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
		var b strings.Builder
		b.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n")
		for _, ln := range prefixLines {
			b.WriteString(ln)
			b.WriteString("\n")
		}
		_, _ = conn.Write([]byte(b.String()))
		// 直接断开，模拟上游中途断流。
	}
}

// performProxyRequest 以 POST + "Bearer <key>" 调用 testGW.handleProxy。
func performProxyRequest(t *testing.T, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return performProxyRequestHeader(t, path, key, body, nil)
}

// performProxyRequestHeader 同 performProxyRequest，但可附加额外请求 header。
func performProxyRequestHeader(t *testing.T, path, key, body string, extra http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	for k, vv := range extra {
		for _, v := range vv {
			r.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	testGW.handleProxy(w, r)
	return w
}

// --- 断言辅助 ---

// assertJSONSameExceptModel 断言上游收到的 body 与客户端原始 body 语义一致（除 model 字段）。
// 当前实现 model 不同时会经 ReplaceModel 重新序列化 JSON（键序会变），因此按解析后结构比较。
func assertJSONSameExceptModel(t *testing.T, clientBody, upstreamBody []byte) {
	t.Helper()
	var cm, um map[string]any
	if err := json.Unmarshal(clientBody, &cm); err != nil {
		t.Fatalf("client body not json: %v", err)
	}
	if err := json.Unmarshal(upstreamBody, &um); err != nil {
		t.Fatalf("upstream body not json: %v", err)
	}
	delete(cm, "model")
	delete(um, "model")
	if !reflect.DeepEqual(cm, um) {
		t.Fatalf("body changed besides model:\n client: %s\n upstream: %s", clientBody, upstreamBody)
	}
}

// dataLines 从 SSE body 中按到达顺序提取 data: 行的载荷（TrimSpace 后）。
func dataLines(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, ln := range strings.Split(body, "\n") {
		if s, ok := strings.CutPrefix(ln, "data:"); ok {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
