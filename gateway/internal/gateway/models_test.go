package gateway

// handleModels 特征测试（characterization tests）：/v1/models 的认证、
// 渠道/模型启停过滤、allowed_models 过滤与 OpenAI-compatible list 返回结构。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gateway/internal/config"
)

// dummyRoutes handleModels 不访问上游，路由 URL 用不可解析占位即可。
func dummyRoutes(upstreamModel string) map[string]config.Route {
	return map[string]config.Route{
		"/v1/messages": {Upstream: "http://models-unused.invalid/v1/messages", Model: upstreamModel, Usage: "anthropic"},
	}
}

func performModelsRequest(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	testGW.handleModels(w, r)
	return w
}

type modelsList struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

func decodeModelsList(t *testing.T, body string) modelsList {
	t.Helper()
	var list modelsList
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("/v1/models body not json: %v\n%s", err, body)
	}
	return list
}

func modelIDs(t *testing.T, list modelsList) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	return ids
}

func TestModelsRequiresAuth(t *testing.T) {
	ch := testChannel("deepseek", "", testModel("model-a", dummyRoutes("model-a")))
	_ = setupGatewayTest(t, []config.Channel{ch}, "")

	w := performModelsRequest(t, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// TestModelsListsEnabledOnly 只返回启用 Channel 下的启用 Model。
// 返回格式保持 OpenAI-compatible list 基本结构（object=list，data 元素含
// id/object/created/owned_by）。
//
// 现状记录（不做强断言）：当前实现按「渠道 × 模型」平铺，不做同名模型去重；
// 若同一模型名出现在多个启用渠道，会返回重复条目。此处不为该重复行为固化断言，
// 给后续合理去重留空间。
func TestModelsListsEnabledOnly(t *testing.T) {
	disabledModel := testModel("model-b", dummyRoutes("model-b"))
	disabledModel.Enabled = false
	enabledCh := testChannel("deepseek", "",
		testModel("model-a", dummyRoutes("model-a")),
		disabledModel,
	)
	disabledCh := testChannel("offline", "", testModel("model-c", dummyRoutes("model-c")))
	disabledCh.Enabled = false
	vk := setupGatewayTest(t, []config.Channel{enabledCh, disabledCh}, "")

	w := performModelsRequest(t, vk)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	list := decodeModelsList(t, w.Body.String())
	if list.Object != "list" {
		t.Fatalf("object = %q, want list", list.Object)
	}
	ids := modelIDs(t, list)
	if !ids["model-a"] {
		t.Fatalf("enabled model missing: %v", ids)
	}
	if ids["model-b"] {
		t.Fatalf("disabled model listed: %v", ids)
	}
	if ids["model-c"] {
		t.Fatalf("model of disabled channel listed: %v", ids)
	}
	if len(list.Data) != 1 {
		t.Fatalf("data length = %d, want 1; body=%s", len(list.Data), w.Body.String())
	}
	item := list.Data[0]
	if item.Object != "model" {
		t.Fatalf("item object = %q, want model", item.Object)
	}
	if item.OwnedBy != "deepseek" {
		t.Fatalf("item owned_by = %q, want deepseek", item.OwnedBy)
	}
	if item.Created <= 0 {
		t.Fatalf("item created = %d, want positive", item.Created)
	}
}

func TestModelsAllowedModelsFilter(t *testing.T) {
	ch := testChannel("deepseek", "",
		testModel("model-a", dummyRoutes("model-a")),
		testModel("model-b", dummyRoutes("model-b")),
		testModel("model-c", dummyRoutes("model-c")),
	)
	vk := setupGatewayTest(t, []config.Channel{ch}, "model-a,model-c")

	w := performModelsRequest(t, vk)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	ids := modelIDs(t, decodeModelsList(t, w.Body.String()))
	if !ids["model-a"] || !ids["model-c"] {
		t.Fatalf("allowed models missing: %v", ids)
	}
	if ids["model-b"] {
		t.Fatalf("disallowed model listed: %v", ids)
	}
}
