package gateway

// 全文请求/响应日志捕获测试：开关开启时正文 1:1 落库（请求体为客户端原始 body），
// 关闭时不落库。异步写入用轮询等待。

import (
	"strings"
	"testing"
	"time"

	"gateway/internal/config"
	"gateway/internal/store"
)

func waitLogBody(t *testing.T, requestID string) *store.LogBody {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		db, ok := testGW.DB.(*store.MemStore)
		if !ok {
			t.Fatal("test DB is not MemStore")
		}
		if b, _ := db.GetLogBodyByRequestID(requestID); b != nil {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func TestRequestBodyCaptureOn(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")
	testGW.logCapture.Store(true)

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	b := waitLogBody(t, "msg_01")
	if b == nil {
		t.Fatal("request body not stored")
	}
	// 请求体 = 客户端原始 body（未替换 model，model-a 本就一致）
	if !strings.Contains(string(b.RequestBody), `"model":"model-a"`) {
		t.Fatalf("request body mismatch: %s", b.RequestBody)
	}
	// 响应体 = 上游响应全文
	if !strings.Contains(string(b.ResponseBody), "msg_01") {
		t.Fatalf("response body mismatch: %s", b.ResponseBody)
	}
	if b.Status != 200 || b.Model != "model-a" {
		t.Fatalf("meta mismatch: %+v", b)
	}
}

func TestRequestBodyCaptureOff(t *testing.T) {
	ur := &upstreamRecorder{}
	srv := newUpstreamServer(t, upstreamJSONHandler(ur, 200, nil, []byte(anthropicRespBody)))
	ch := testChannel("deepseek", "", testModel("model-a", threeProtocolRoutes(srv, "upstream-a")))
	vk := setupGatewayTest(t, []config.Channel{ch}, "")
	testGW.logCapture.Store(false)

	w := performProxyRequest(t, "/v1/messages", vk, anthropicReqBody)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if b := waitLogBody(t, "msg_01"); b != nil {
		t.Fatal("body stored while capture disabled")
	}
}
