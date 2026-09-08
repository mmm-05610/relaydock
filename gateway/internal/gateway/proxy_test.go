package gateway

// handleProxy 数据面特征测试（characterization tests）。
//
// 锁定当前链路的外部行为：认证 → 提取 model → FindModel first-match → 按 path 查 Route
// → 替换 upstream model → 设置上游认证 Header → 调用上游 → 透传状态码/Header/body → 计量旁路。
//
// 原则：
//   - 上游一律用 httptest.Server 模拟，不访问任何公网 API；
//   - 断言对外可观察行为（状态码、Header、body、上游收到什么），不固化内部实现细节；
//   - 已知缺陷（无 Context 传播、无超时、无重试等）只记录现状，不写成阻止后续修复的硬断言。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gateway/internal/config"
	"gateway/internal/keys"
)

// --- 测试配置构造 ---

// testModel 构造一个启用模型（价格非零，走真实 fillMetering 路径但不影响对外行为）。
func testModel(name string, routes map[string]config.Route) config.Model {
	return config.Model{
		Name:    name,
		Routes:  routes,
		Enabled: true,
		Pricing: config.Pricing{InputPerM: 3, OutputPerM: 6, CacheReadPerM: 0.025, CacheWritePerM: 3},
	}
}

func testChannel(provider, authMode string, models ...config.Model) config.Channel {
	return config.Channel{
		Provider: provider,
		Name:     "Channel " + provider,
		Enabled:  true,
		AuthMode: authMode,
		Models:   models,
	}
}

// threeProtocolRoutes 一个模型的三条协议路由，全部指向 srv。
func threeProtocolRoutes(srv *httptest.Server, upstreamModel string) map[string]config.Route {
	return map[string]config.Route{
		"/v1/messages":         {Upstream: srv.URL + "/up/anthropic/messages", Model: upstreamModel, Usage: "anthropic"},
		"/v1/responses":        {Upstream: srv.URL + "/up/responses", Model: upstreamModel, Usage: "responses"},
		"/v1/chat/completions": {Upstream: srv.URL + "/up/chat/completions", Model: upstreamModel, Usage: "chat_completions"},
	}
}

const anthropicReqBody = `{"model":"model-a","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"system":"be brief","temperature":0.5}`
const responsesReqBody = `{"model":"model-a","input":"hello","max_output_tokens":64,"reasoning":{"effort":"low"}}`
const chatReqBody = `{"model":"model-a","messages":[{"role":"user","content":"hello"}],"max_completion_tokens":64,"stream":false}`

const anthropicRespBody = `{"id":"msg_01","type":"message","role":"assistant","model":"upstream-a","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":87,"output_tokens":30}}`
const responsesRespBody = `{"id":"resp_01","object":"response","model":"upstream-a","output":[],"usage":{"input_tokens":167,"output_tokens":2,"input_tokens_details":{"cached_tokens":128}}}`
const chatRespBody = `{"id":"chatcmpl-01","object":"chat.completion","model":"upstream-a","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":40}}}`

// ===================================================================
// A. 认证与权限
// ===================================================================

func TestProxyAuthMissingKey(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	_ = setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", "", anthropicReqBody)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unauthorized") {
		t.Fatalf("body = %q, want contains unauthorized", w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

func TestProxyAuthInvalidKey(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	_ = setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", "gw-invalid-not-issued", anthropicReqBody)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

func TestProxyAuthValidKeyReachesUpstream(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ur.count() != 1 {
		t.Fatalf("upstream hit %d times, want 1", ur.count())
	}
	if w.Body.String() != anthropicRespBody {
		t.Fatalf("client body = %q, want upstream body verbatim", w.Body.String())
	}
}

func TestProxyAuthQuotaExhausted(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	hash := keys.SHA256Hash(vk)
	if err := testGW.KeyMgr.UpdateQuota(hash, 1); err != nil {
		t.Fatal(err)
	}
	if err := testGW.KeyMgr.AddUsage(hash, 2); err != nil {
		t.Fatal(err)
	}

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (quota exceeded pre-call hard block); body=%s", w.Code, w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

func TestProxyModelNotAllowed(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "model-b") // allowed_models 不含 model-a

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not allowed") {
		t.Fatalf("body = %q, want contains 'not allowed'", w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

func TestProxyEmptyAllowedModelsAllowsAny(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "") // 空 = 不限

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ur.count() != 1 {
		t.Fatalf("upstream hit %d times, want 1", ur.count())
	}
}

// ===================================================================
// B. 模型和路由解析
// ===================================================================

func TestProxyMissingModel(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, `{"max_tokens":8,"messages":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "missing model") {
		t.Fatalf("body = %q, want contains 'missing model'", w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

func TestProxyUnknownModel(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, `{"model":"no-such-model","max_tokens":8,"messages":[]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

func TestProxyNoRouteForPath(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(`{}`)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	// 模型存在，但 /v1/embeddings 不在 routes 里
	w := performProxyRequest(t, "/v1/embeddings", vk, `{"model":"model-a","input":"hi"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no route") {
		t.Fatalf("body = %q, want contains 'no route'", w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

// TestProxyDisabledModelReturns404WithoutUpstream 现状：FindModel 只返回启用模型，
// 禁用模型在解析阶段即 404（错误文案是 "unknown model"——handleProxy 里的
// "model disabled" 分支经 FindModel 过滤后实际不可达）。这里锁定对外可见行为：
// 404 且不请求上游；不断言具体错误文案。
func TestProxyDisabledModelReturns404WithoutUpstream(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	disabled := testModel("model-a", threeProtocolRoutes(srv, "upstream-a"))
	disabled.Enabled = false
	ch := testChannel("deepseek", "", disabled)
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

// TestProxyDisabledChannelReturns404WithoutUpstream 现状：FindModel 跳过禁用渠道，
// 请求得到 404（文案 "unknown model"，"channel disabled" 分支同样不可达）。
func TestProxyDisabledChannelReturns404WithoutUpstream(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	ch.Enabled = false
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

// TestProxySameNameModelFirstChannelWins 锁定当前兼容行为：同名模型位于多个启用渠道时，
// 请求路由到配置顺序中的第一个渠道（FindModel first-match）。
// 这只是现状刻画，不代表未来账号池调度算法（见 docs/design-upstream-account-pool.md）。
func TestProxySameNameModelFirstChannelWins(t *testing.T) {
	urA := &upstreamRecorder{}
	urB := &upstreamRecorder{}
	srvA := newUpstreamServer(t, upstreamJSONHandler(urA, 200, nil, []byte(`{"ok":"a"}`)))
	srvB := newUpstreamServer(t, upstreamJSONHandler(urB, 200, nil, []byte(`{"ok":"b"}`)))

	chA := testChannel("prov-a", "", testModel("shared-model", map[string]config.Route{
		"/v1/messages": {Upstream: srvA.URL + "/a/messages", Model: "shared-a", Usage: "anthropic"},
	}))
	chB := testChannel("prov-b", "", testModel("shared-model", map[string]config.Route{
		"/v1/messages": {Upstream: srvB.URL + "/b/messages", Model: "shared-b", Usage: "anthropic"},
	}))
	vk := setupGatewayTest(t, []config.Channel{chA, chB}, "")

	w := performProxyRequest(t, "/v1/messages", vk, `{"model":"shared-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if urA.count() != 1 {
		t.Fatalf("first channel upstream hit %d times, want 1", urA.count())
	}
	if urB.count() != 0 {
		t.Fatalf("second channel upstream hit %d times, want 0 (first-match)", urB.count())
	}
	up := urA.mustAt(t, 0)
	if up.Path != "/a/messages" {
		t.Fatalf("upstream path = %q, want /a/messages", up.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(up.Body, &sent); err != nil {
		t.Fatalf("upstream body not json: %v", err)
	}
	if sent["model"] != "shared-a" {
		t.Fatalf("upstream model = %v, want shared-a (first channel's upstream model)", sent["model"])
	}
}

// ===================================================================
// C. 三种协议的非流式代理
// ===================================================================

// TestProxyNonStreamProtocolVariants 三种协议的非流式透传行为表驱动测试：
// 正确上游地址、method、认证 Header、model 替换、其余字段语义不变、
// 状态码/Header/body 原样返回、无额外包装层。
func TestProxyNonStreamProtocolVariants(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		clientBody   string
		upstreamPath string
		upstreamBody string
	}{
		{
			name:         "anthropic_messages",
			path:         "/v1/messages",
			clientBody:   strings.Replace(anthropicReqBody, `"model":"model-a"`, `"model":"client-x"`, 1),
			upstreamPath: "/up/anthropic/messages",
			upstreamBody: anthropicRespBody,
		},
		{
			name:         "responses",
			path:         "/v1/responses",
			clientBody:   strings.Replace(responsesReqBody, `"model":"model-a"`, `"model":"client-x"`, 1),
			upstreamPath: "/up/responses",
			upstreamBody: responsesRespBody,
		},
		{
			name:         "chat_completions",
			path:         "/v1/chat/completions",
			clientBody:   strings.Replace(chatReqBody, `"model":"model-a"`, `"model":"client-x"`, 1),
			upstreamPath: "/up/chat/completions",
			upstreamBody: chatRespBody,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ur := &upstreamRecorder{}
			respHeader := http.Header{}
			respHeader.Set("Content-Type", "application/json")
			respHeader.Set("X-Request-Id", "req-123")
			srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, respHeader, []byte(tc.upstreamBody)))
			// 配置的客户端模型名就叫 client-x（路由到上游名 upstream-a），
			// 以覆盖「客户端模型名 != 上游模型名」的替换路径。
			ch := testChannel("deepseek", "", testModel("client-x", threeProtocolRoutes(srv, "upstream-a")))
			vk := setupGatewayTest(t, []config.Channel{ch}, "")

			w := performProxyRequest(t, tc.path, vk, tc.clientBody)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if ur.count() != 1 {
				t.Fatalf("upstream hit %d times, want 1", ur.count())
			}
			up := ur.mustAt(t, 0)

			// 到达配置的正确上游地址，method 保持一致
			if up.Path != tc.upstreamPath {
				t.Fatalf("upstream path = %q, want %q", up.Path, tc.upstreamPath)
			}
			if up.Method != http.MethodPost {
				t.Fatalf("upstream method = %q, want POST", up.Method)
			}

			// 上游认证 Header（默认 bearer）
			if got := up.Header.Get("Authorization"); got != "Bearer upstream-key-deepseek" {
				t.Fatalf("upstream Authorization = %q, want bearer upstream key", got)
			}

			// model 替换 + 除 model 外字段语义不变
			var sent map[string]any
			if err := json.Unmarshal(up.Body, &sent); err != nil {
				t.Fatalf("upstream body not json: %v", err)
			}
			if sent["model"] != "upstream-a" {
				t.Fatalf("upstream model = %v, want upstream-a", sent["model"])
			}
			assertJSONSameExceptModel(t, []byte(tc.clientBody), up.Body)

			// 客户端响应：状态码、Content-Type、自定义 Header、body 原样（无包装层）
			if w.Code != http.StatusOK {
				t.Fatalf("client status = %d, want 200", w.Code)
			}
			if got := w.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("client Content-Type = %q, want application/json", got)
			}
			if got := w.Header().Get("X-Request-Id"); got != "req-123" {
				t.Fatalf("client X-Request-Id = %q, want req-123", got)
			}
			if w.Body.String() != tc.upstreamBody {
				t.Fatalf("client body != upstream body:\n got: %s\nwant: %s", w.Body.String(), tc.upstreamBody)
			}
		})
	}
}

// TestProxyIdenticalModelBodyPassedThroughVerbatim 客户端 model 与上游 model 相同时，
// 当前实现跳过 replaceModel，body 逐字节原样透传（与 model 不同时的重序列化行为并存）。
func TestProxyIdenticalModelBodyPassedThroughVerbatim(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "model-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	up := ur.mustAt(t, 0)
	if string(up.Body) != anthropicReqBody {
		t.Fatalf("upstream body = %q, want verbatim %q", up.Body, anthropicReqBody)
	}
}

// ===================================================================
// D. 上游认证模式
// ===================================================================

// TestProxyUpstreamAuthBearerMode 默认（auth_mode 空）与显式 bearer 均使用
// Authorization: Bearer <upstream-key>。
func TestProxyUpstreamAuthBearerMode(t *testing.T) {
	for _, mode := range []string{"", "bearer"} {
		t.Run("auth_mode="+mode, func(t *testing.T) {
			ur := &upstreamRecorder{}
			srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
			ch := testChannel("deepseek", mode, testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
			vk := setupGatewayTest(t, []config.Channel{ch}, "")

			w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			up := ur.mustAt(t, 0)
			if got := up.Header.Get("Authorization"); got != "Bearer upstream-key-deepseek" {
				t.Fatalf("upstream Authorization = %q, want %q", got, "Bearer upstream-key-deepseek")
			}
			if got := up.Header.Get("x-api-key"); got != "" {
				t.Fatalf("upstream x-api-key = %q, want empty", got)
			}
		})
	}
}

func TestProxyUpstreamAuthXAPIKeyMode(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("minimax", "x_api_key", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	up := ur.mustAt(t, 0)
	if got := up.Header.Get("x-api-key"); got != "upstream-key-minimax" {
		t.Fatalf("upstream x-api-key = %q, want %q", got, "upstream-key-minimax")
	}
	if got := up.Header.Get("Authorization"); got != "" {
		t.Fatalf("upstream Authorization = %q, want empty in x_api_key mode", got)
	}
}

// TestProxyVirtualKeyNotSentUpstream 客户端虚拟 key 不允许原样出现在发给上游的请求里。
func TestProxyVirtualKeyNotSentUpstream(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")
	if !strings.HasPrefix(vk, "gw-") {
		t.Fatalf("virtual key %q should have gw- prefix", vk)
	}

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	up := ur.mustAt(t, 0)
	if got := up.Header.Get("Authorization"); got != "Bearer upstream-key-deepseek" {
		t.Fatalf("upstream Authorization = %q, want upstream key, not virtual key", got)
	}
	for k, vv := range up.Header {
		for _, v := range vv {
			if strings.Contains(v, vk) {
				t.Fatalf("virtual key leaked in upstream header %s: %q", k, v)
			}
		}
	}
	if strings.Contains(string(up.Body), vk) {
		t.Fatalf("virtual key leaked in upstream body")
	}
}

// TestProxyAnthropicVersionHeader anthropic 协议的 anthropic-version 处理：
// 客户端提供时保持该值；缺失时补当前默认值；非 anthropic 协议不注入默认值。
func TestProxyAnthropicVersionHeader(t *testing.T) {
	t.Run("client version kept", func(t *testing.T) {
		ur := &upstreamRecorder{}
		srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
		ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
		vk := setupGatewayTest(t, []config.Channel{ch}, "")

		extra := http.Header{}
		extra.Set("anthropic-version", "2023-01-01")
		w := performProxyRequestHeader(t, "/v1/messages", vk, anthropicReqBody, extra)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		up := ur.mustAt(t, 0)
		if got := up.Header.Get("anthropic-version"); got != "2023-01-01" {
			t.Fatalf("upstream anthropic-version = %q, want client value 2023-01-01", got)
		}
	})

	t.Run("default version injected", func(t *testing.T) {
		ur := &upstreamRecorder{}
		srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
		ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
		vk := setupGatewayTest(t, []config.Channel{ch}, "")

		w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		up := ur.mustAt(t, 0)
		if got := up.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("upstream anthropic-version = %q, want default 2023-06-01", got)
		}
	})

	t.Run("non-anthropic route gets no default", func(t *testing.T) {
		ur := &upstreamRecorder{}
		srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(chatRespBody)))
		ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
		vk := setupGatewayTest(t, []config.Channel{ch}, "")

		w := performProxyRequest(t, "/v1/chat/completions", vk, chatReqBody)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		up := ur.mustAt(t, 0)
		if got := up.Header.Get("anthropic-version"); got != "" {
			t.Fatalf("upstream anthropic-version = %q, want empty for chat_completions route", got)
		}
	})
}

// ===================================================================
// E. 上游配置和失败响应
// ===================================================================

// TestProxyUpstreamKeyMissing 上游 key 缺失：500，不请求上游。
func TestProxyUpstreamKeyMissing(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")
	setUpstreamKey(t, "deepseek", "") // 模拟 PG/env 都没有该渠道 key

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream key missing") {
		t.Fatalf("body = %q, want contains 'upstream key missing'", w.Body.String())
	}
	if ur.count() != 0 {
		t.Fatalf("upstream hit %d times, want 0", ur.count())
	}
}

// TestProxyInvalidUpstreamURL 非法上游 URL：锁定当前错误状态与 body 的主要语义。
// 不访问网络：解析失败发生在 http.NewRequest；ftp scheme 在 Transport 内被拒绝。
func TestProxyInvalidUpstreamURL(t *testing.T) {
	t.Run("unparseable url -> 500 build upstream", func(t *testing.T) {
		ch := testChannel("deepseek", "", testModel("model-a", map[string]config.Route{
			"/v1/messages": {Upstream: "http://exa mple.com/v1/messages", Model: "upstream-a", Usage: "anthropic"},
		}))
		vk := setupGatewayTest(t, []config.Channel{ch}, "")

		w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "build upstream") {
			t.Fatalf("body = %q, want contains 'build upstream'", w.Body.String())
		}
	})

	t.Run("unsupported scheme -> 502 upstream error", func(t *testing.T) {
		ch := testChannel("deepseek", "", testModel("model-a", map[string]config.Route{
			"/v1/messages": {Upstream: "ftp://127.0.0.1:1/v1/messages", Model: "upstream-a", Usage: "anthropic"},
		}))
		vk := setupGatewayTest(t, []config.Channel{ch}, "")

		w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "upstream error") {
			t.Fatalf("body = %q, want contains 'upstream error'", w.Body.String())
		}
	})
}

// TestProxyUpstreamErrorStatusPassthrough 上游 4xx/5xx：状态码与错误 body 原样透传，
// 每次只请求上游一次——当前阶段没有任何重试或账号切换逻辑。
func TestProxyUpstreamErrorStatusPassthrough(t *testing.T) {
	cases := []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError}
	for _, status := range cases {
		t.Run(fmt.Sprintf("upstream_%d", status), func(t *testing.T) {
			ur := &upstreamRecorder{}
			errBody := `{"error":{"type":"api_error","message":"upstream says no"}}`
			hdr := http.Header{}
			hdr.Set("Content-Type", "application/json")
			srv := newUpstreamServer(t, upstreamJSONHandler(ur, status, hdr, []byte(errBody)))
			ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
			vk := setupGatewayTest(t, []config.Channel{ch}, "")

			w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
			if w.Code != status {
				t.Fatalf("status = %d, want %d (passthrough); body=%s", w.Code, status, w.Body.String())
			}
			if w.Body.String() != errBody {
				t.Fatalf("body = %q, want upstream error body verbatim %q", w.Body.String(), errBody)
			}
			if ur.count() != 1 {
				t.Fatalf("upstream hit %d times, want exactly 1 (no retry today)", ur.count())
			}
		})
	}
}

// TestProxyUpstreamNetworkError 上游网络错误：502，客户端 body 是固定的
// "upstream error"（内部错误细节只进 usage 日志，不透传），不泄漏任何 key。
func TestProxyUpstreamNetworkError(t *testing.T) {
	dead := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request) {})
	deadURL := dead.URL
	dead.Close() // 先关闭，制造 connection refused

	ch := testChannel("deepseek", "", testModel("model-a", map[string]config.Route{
		"/v1/messages": {Upstream: deadURL + "/up/anthropic/messages", Model: "upstream-a", Usage: "anthropic"},
	}))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream error") {
		t.Fatalf("body = %q, want contains 'upstream error'", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "upstream-key-deepseek") || strings.Contains(w.Body.String(), vk) {
		t.Fatalf("client error body leaked a key: %q", w.Body.String())
	}
}

// ===================================================================
// F. SSE 流式响应
// ===================================================================

var anthropicSSELines = []string{
	`event: message_start`,
	`data: {"type":"message_start","message":{"id":"msg_sse","usage":{"input_tokens":10,"output_tokens":0}}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"he"}}`,
	``,
	`event: content_block_delta`,
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"llo"}}`,
	``,
	`event: message_delta`,
	`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
	``,
	`event: message_stop`,
	`data: {"type":"message_stop"}`,
	``,
}

// TestProxySSEAnthropicRelay anthropic SSE：事件按顺序完整到达、终止事件不丢、
// Header/状态码正确、上游只请求一次。只断言事件内容与顺序，不断言逐字节换行框架
// （当前 Scanner 转发会重写行结束符）。
func TestProxySSEAnthropicRelay(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamSSEHandler(ur, "text/event-stream", anthropicSSELines))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("client Content-Type = %q, want event-stream passthrough", ct)
	}
	if ur.count() != 1 {
		t.Fatalf("upstream hit %d times, want 1", ur.count())
	}

	got := dataLines(t, w.Body.String())
	want := []string{
		`{"type":"message_start","message":{"id":"msg_sse","usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"he"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"llo"}}`,
		`{"type":"message_delta","usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSE data events mismatch:\n got:  %v\n want: %v", got, want)
	}
	// 多行 SSE 事件的 event: 行也必须透传
	for _, ev := range []string{"event: message_start", "event: message_stop"} {
		if !strings.Contains(w.Body.String(), ev) {
			t.Fatalf("SSE body missing %q", ev)
		}
	}
}

var responsesSSEPrefix = []string{
	`event: response.created`,
	`data: {"type":"response.created","response":{"id":"resp_sse"}}`,
	``,
	`event: response.output_text.delta`,
	`data: {"type":"response.output_text.delta","delta":"hi"}`,
	``,
}

// TestProxySSEResponsesRelay responses SSE：completed / incomplete 两种终止事件
// 都必须完整转发（含 usage 载荷），顺序保持，上游只请求一次。
func TestProxySSEResponsesRelay(t *testing.T) {
	cases := []struct {
		name     string
		terminal string
	}{
		{"completed", `{"type":"response.completed","response":{"id":"resp_sse","usage":{"input_tokens":167,"input_tokens_details":{"cached_tokens":128},"output_tokens":30}}}`},
		{"incomplete", `{"type":"response.incomplete","response":{"id":"resp_sse","usage":{"input_tokens":87,"output_tokens":5}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ur := &upstreamRecorder{}
			lines := append(append([]string{}, responsesSSEPrefix...),
				`event: response.`+tc.name,
				`data: `+tc.terminal,
				``,
			)
			srv := newUpstreamServer(t, upstreamSSEHandler(ur, "text/event-stream", lines))
			ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
			vk := setupGatewayTest(t, []config.Channel{ch}, "")

			w := performProxyRequest(t, "/v1/responses", vk, responsesReqBody)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if ur.count() != 1 {
				t.Fatalf("upstream hit %d times, want 1", ur.count())
			}
			got := dataLines(t, w.Body.String())
			want := []string{
				`{"type":"response.created","response":{"id":"resp_sse"}}`,
				`{"type":"response.output_text.delta","delta":"hi"}`,
				tc.terminal,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("SSE data events mismatch:\n got:  %v\n want: %v", got, want)
			}
		})
	}
}

var chatSSELines = []string{
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"he"}}]}`,
	``,
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
	``,
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	``,
	`data: [DONE]`,
	``,
}

// TestProxySSEChatCompletionsRelay chat completions SSE：chunk 顺序保持、
// 终止标记 [DONE] 不丢失、Handler 在流结束后正常返回（状态码 200）。
func TestProxySSEChatCompletionsRelay(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamSSEHandler(ur, "text/event-stream", chatSSELines))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/chat/completions", vk, chatReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ur.count() != 1 {
		t.Fatalf("upstream hit %d times, want 1", ur.count())
	}
	got := dataLines(t, w.Body.String())
	want := []string{
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"he"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSE data events mismatch:\n got:  %v\n want: %v", got, want)
	}
}

// TestProxySSEUpstreamDisconnectMidStream 上游在 SSE 中途断流：已输出的内容仍能被
// 客户端观察到，当前实现不会请求第二个上游（无重试），测试有界完成不阻塞。
func TestProxySSEUpstreamDisconnectMidStream(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, disconnectAfterPrefix(ur, []string{
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"part1"}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"part2"}}`,
		``,
	}))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	got := dataLines(t, w.Body.String())
	if len(got) != 2 {
		t.Fatalf("got %d data events, want 2 (already-relayed prefix); body=%q", len(got), w.Body.String())
	}
	if !strings.Contains(got[0], "part1") || !strings.Contains(got[1], "part2") {
		t.Fatalf("prefix events lost or reordered: %v", got)
	}
	if ur.count() != 1 {
		t.Fatalf("upstream hit %d times, want 1 (no second upstream attempt today)", ur.count())
	}
}

// TestProxySSELargeEventNotTruncated 单个明显大于普通事件、但低于当前 1MB 扫描上限的
// SSE 事件必须完整到达客户端。有意不断言 1MB 上限本身——那是已知实现细节
// （docs/software-architecture.md §8），未来 relay 重写时可能调整。
func TestProxySSELargeEventNotTruncated(t *testing.T) {
	ur := &upstreamRecorder{}
	payload := strings.Repeat("x", 600_000) + "END-MARKER"
	lines := []string{
		`data: {"type":"big_event","payload":"` + payload + `"}`,
		``,
	}
	srv := newUpstreamServer(t, upstreamSSEHandler(ur, "text/event-stream", lines))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := dataLines(t, w.Body.String())
	if len(got) != 1 {
		t.Fatalf("got %d data events, want 1", len(got))
	}
	var ev struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal([]byte(got[0]), &ev); err != nil {
		t.Fatalf("large event payload not valid json: %v", err)
	}
	if ev.Payload != payload {
		t.Fatalf("large event truncated: got %d bytes, want %d", len(ev.Payload), len(payload))
	}
}

// TestProxySSEClientCancelObservation 只记录现状，不做取消语义硬断言：
// handleProxy 当前用 http.NewRequest 构造上游请求，未绑定客户端 Context，
// 因此客户端断开后上游请求不会被取消（已知缺陷，第二/三阶段修复后观察值预期翻转为 true）。
// 稳定断言：整个过程中上游只被请求一次，且测试有界完成。
func TestProxySSEClientCancelObservation(t *testing.T) {
	ur := &upstreamRecorder{}
	observedCancel := make(chan bool, 1)
	srv := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request) {
		ur.add(readCapture(r))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"first\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 等待最多 1.5s，观察自身 Context 是否因客户端取消而结束
		select {
		case <-r.Context().Done():
			observedCancel <- true
			return
		case <-time.After(1500 * time.Millisecond):
		}
		observedCancel <- false
		_, _ = w.Write([]byte("data: {\"type\":\"after-wait\"}\n\n"))
	})
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")

	// 网关本体也用 [::1]（见 newUpstreamServer 说明）
	gw := newUpstreamServer(t, http.HandlerFunc(testGW.handleProxy))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(anthropicReqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+vk)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// 读到第一个事件后取消客户端请求
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "first") {
		t.Fatalf("first SSE line = %q, err=%v", line, err)
	}
	cancel()

	var observed bool
	select {
	case observed = <-observedCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for upstream handler to finish")
	}
	t.Logf("客户端取消是否传播到上游 Context: %v（当前实现预期 false：上游请求未绑定客户端 Context；修复后应为 true）", observed)

	if ur.count() != 1 {
		t.Fatalf("upstream hit %d times, want 1", ur.count())
	}
}
