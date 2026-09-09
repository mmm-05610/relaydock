package gateway

// OAuth 编排测试：用 httptest 假 token 端点验证授权码交换、
// id_token 的 chatgpt_account_id 提取、stage 流程与凭据解析 header。

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gateway/internal/config"
	"gateway/internal/pool"
	"gateway/internal/routing"
)

// fakeIDToken 造一个三段式 JWT（payload 含 https://api.openai.com/auth.chatgpt_account_id）。
func fakeIDToken(t *testing.T, accountID string) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	payload := map[string]any{
		"sub":                         "user-1",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(payload) + ".sig"
}

func TestChatgptAccountIDFromIDToken(t *testing.T) {
	if got := chatgptAccountIDFromIDToken(fakeIDToken(t, "acct-123")); got != "acct-123" {
		t.Fatalf("account id = %q, want acct-123", got)
	}
	if got := chatgptAccountIDFromIDToken("not-a-jwt"); got != "" {
		t.Fatalf("garbage token should give empty, got %q", got)
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	tok := enc(map[string]string{"alg": "none"}) + "." + enc(map[string]any{"chatgpt_account_id": "top"}) + ".x"
	if got := chatgptAccountIDFromIDToken(tok); got != "top" {
		t.Fatalf("top-level claim = %q, want top", got)
	}
}

func TestExtractOAuthCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain-code", "plain-code"},
		{"http://localhost:1455/auth/callback?code=abc&state=s", "abc"},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractOAuthCode(c.in); got != c.want {
			t.Fatalf("extractOAuthCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func jsonRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	return httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(b)))
}

func TestOAuthStageFlow(t *testing.T) {
	var tokenCalls int
	tokenSrv := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", r.FormValue("grant_type"))
		}
		if r.FormValue("code_verifier") == "" {
			t.Error("PKCE code_verifier missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "test-access",
			"refresh_token": "test-refresh",
			"id_token":      fakeIDToken(t, "acct-xyz"),
			"expires_in":    3600,
		})
	})

	ch := testChannel("openai-sub", "", testModel("model-a", map[string]config.Route{
		"/v1/responses": {Upstream: tokenSrv.URL + "/backend/codex/responses", Model: "gpt-x", Usage: "responses"},
	}))
	ch.ID = 9
	setupGatewayTest(t, []config.Channel{ch}, "")
	testGW.masterKeyHex = strings.Repeat("ab", 32) // 有效 master key（32 字节 hex），token 包加密需要
	testGW.Snapshots.Update(func(cur *routing.Snapshot) *routing.Snapshot {
		cfg := *cur.Config
		cfg.OAuthProfiles = map[string]config.OAuthProfile{
			"codex-test": {
				AuthorizeURL:      tokenSrv.URL + "/oauth/authorize",
				TokenURL:          tokenSrv.URL + "/oauth/token",
				ClientID:          "test-client",
				Scopes:            "openid profile email offline_access",
				RedirectURI:       "http://localhost:1455/auth/callback",
				UpstreamAuthStyle: "chatgpt_codex",
			},
		}
		return cur.WithConfig(&cfg)
	})

	// 走真实 mux 路由（PathValue 需要 ServeMux 匹配填充）
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/channels/{provider}/accounts/oauth/start", testGW.handleOAuthStart)
	mux.HandleFunc("GET /api/channels/{provider}/accounts/oauth/{stage}", testGW.handleOAuthStatus)
	mux.HandleFunc("POST /api/channels/{provider}/accounts/oauth/{stage}/code", testGW.handleOAuthSubmitCode)

	// start：返回 stage_id + 带 PKCE 的授权 URL
	rec := httptest.NewRecorder()
	req := jsonRequest(t, map[string]any{"profile": "codex-test", "name": "sub-a", "max_concurrency": 2})
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/channels/openai-sub/accounts/oauth/start", req.Body))
	if rec.Code != 200 {
		t.Fatalf("start = %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		StageID      string `json:"stage_id"`
		AuthorizeURL string `json:"authorize_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	if started.StageID == "" || !strings.Contains(started.AuthorizeURL, "code_challenge_method=S256") ||
		!strings.Contains(started.AuthorizeURL, "client_id=test-client") {
		t.Fatalf("bad start response: %+v", started)
	}

	// status：pending
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/channels/openai-sub/accounts/oauth/"+started.StageID, nil))
	if !strings.Contains(rec.Body.String(), `"pending"`) {
		t.Fatalf("status = %s", rec.Body.String())
	}

	// submit code：换 token → 建账号 → completed
	rec = httptest.NewRecorder()
	req = jsonRequest(t, map[string]string{"code_or_url": "http://localhost:1455/auth/callback?code=the-code&state="})
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/channels/openai-sub/accounts/oauth/"+started.StageID+"/code", req.Body))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"completed"`) {
		t.Fatalf("submit = %d: %s", rec.Code, rec.Body.String())
	}
	if tokenCalls != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", tokenCalls)
	}
}

// TestCredentialForChatgptHeaders oauth 型账号：数据面凭据取自内存 cache，
// chatgpt_codex 样式附带 chatgpt-account-id + OpenAI-Beta。
func TestCredentialForChatgptHeaders(t *testing.T) {
	ch := testChannel("openai-sub", "", testModel("model-a", nil))
	ch.ID = 7
	setupGatewayTest(t, []config.Channel{ch}, "")
	testGW.Snapshots.Update(func(cur *routing.Snapshot) *routing.Snapshot {
		cfg := *cur.Config
		cfg.OAuthProfiles = map[string]config.OAuthProfile{
			"codex": {UpstreamAuthStyle: "chatgpt_codex"},
		}
		return cur.WithConfig(&cfg)
	})
	testGW.setAccountSpecs([]*pool.AccountSpec{
		{ID: 42, ChannelID: 7, Name: "sub", CredentialType: "oauth", OAuthProfile: "codex", Enabled: true},
	})
	testGW.oauthCachePut(42, "live-token", "acct-1", time.Now().Add(time.Hour))

	snap := testGW.Snapshots.Load()
	refs := snap.AccountsFor(snap.FindChannel("openai-sub"))
	if len(refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(refs))
	}
	cred, headers := testGW.credentialFor(snap, refs[0])
	if cred != "live-token" {
		t.Fatalf("credential = %q, want live-token (from cache)", cred)
	}
	if headers["chatgpt-account-id"] != "acct-1" || headers["OpenAI-Beta"] != "responses=experimental" {
		t.Fatalf("headers = %v", headers)
	}

	// api_key 型：无额外 header，凭据为静态 key
	testGW.setAccountSpecs([]*pool.AccountSpec{
		{ID: 43, ChannelID: 7, Name: "plain", Credential: "sk-static", Enabled: true},
	})
	refs = snap.AccountsFor(snap.FindChannel("openai-sub"))
	for _, ref := range refs {
		if ref.Spec.ID == 43 {
			cred, headers = testGW.credentialFor(snap, ref)
			if cred != "sk-static" || len(headers) != 0 {
				t.Fatalf("api_key credentialFor = %q %v", cred, headers)
			}
		}
	}
}
