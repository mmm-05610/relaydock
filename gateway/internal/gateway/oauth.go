package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gateway/internal/config"
	"gateway/internal/keys"
	"gateway/internal/pool"
	"gateway/internal/routing"
	"gateway/internal/store"
)

// OAuth 订阅账号编排（对齐 gpt-load credential-stages 形态，第一版 manual paste）：
//
//	start   → 生成 PKCE + state，返回授权 URL（浏览器打开、登录、同意）
//	回调    → 浏览器跳到 redirect_uri（本机不可达即报错），用户把地址栏 URL/ code 贴回
//	submit  → 服务端用 code + verifier 换 token，加密落库为 oauth 型账号，进账号池
//	refresh → 后台调度器在过期前 5 分钟用 refresh_token 续期，失败则冷却账号
//
// stages 是短生命周期（15 分钟 TTL）的进程内状态，重启丢失可接受（重新发起即可）。

const oauthStageTTL = 15 * time.Minute

// oauthTokenBundle EncryptedToken 解密后的凭据包。
type oauthTokenBundle struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// oauthLiveToken 内存中的当前 access token（数据面请求时取用，刷新调度器更新）。
type oauthLiveToken struct {
	AccessToken string
	AccountID   string
	ExpiresAt   time.Time
}

type oauthStage struct {
	ID             string
	ChannelID      int64
	Provider       string
	ProfileName    string
	Name           string
	MaxConcurrency int64
	State          string // OAuth state
	Verifier       string // PKCE verifier
	AuthorizeURL   string
	Status         string // pending | completed | failed | cancelled
	Error          string
	AccountID      int64
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// stageForRoute 按 URL 里的 stage id 取 stage（顺带清理过期）。
func (g *Gateway) stageForRoute(id string) *oauthStage {
	g.oauthMu.Lock()
	defer g.oauthMu.Unlock()
	st, ok := g.stages[id]
	if !ok {
		return nil
	}
	if time.Now().After(st.ExpiresAt) {
		delete(g.stages, id)
		return nil
	}
	return st
}

func (g *Gateway) putStage(st *oauthStage) {
	g.oauthMu.Lock()
	defer g.oauthMu.Unlock()
	g.stages[st.ID] = st
	now := time.Now()
	for id, s := range g.stages {
		if now.After(s.ExpiresAt) {
			delete(g.stages, id)
		}
	}
}

// handleOAuthProfiles GET /api/oauth/profiles —— 可用的 OAuth profile 列表（不含密钥）。
func (g *Gateway) handleOAuthProfiles(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	out := map[string]config.OAuthProfile{}
	for name, p := range g.Snapshots.Load().Config.OAuthProfiles {
		out[name] = p
	}
	writeJSON(w, out)
}

// handleOAuthStart POST /api/channels/{p}/accounts/oauth/start
// body: {profile, name?, max_concurrency?} → {stage_id, authorize_url}
func (g *Gateway) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	snap := g.Snapshots.Load()
	ch := snap.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	var req struct {
		Profile        string `json:"profile"`
		Name           string `json:"name"`
		MaxConcurrency int64  `json:"max_concurrency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	profile, ok := snap.Config.OAuthProfiles[req.Profile]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown oauth profile %q", req.Profile), http.StatusBadRequest)
		return
	}

	verifier, err := randomToken(48)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state, err := randomToken(24)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", profile.ClientID)
	q.Set("redirect_uri", profile.RedirectURI)
	q.Set("scope", profile.Scopes)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	authURL := profile.AuthorizeURL + "?" + q.Encode()

	st := &oauthStage{
		ID:             state[:8] + "-" + randomSuffix(),
		ChannelID:      ch.ID,
		Provider:       provider,
		ProfileName:    req.Profile,
		Name:           req.Name,
		MaxConcurrency: req.MaxConcurrency,
		State:          state,
		Verifier:       verifier,
		AuthorizeURL:   authURL,
		Status:         "pending",
		CreatedAt:      time.Now(),
		ExpiresAt:      time.Now().Add(oauthStageTTL),
	}
	g.putStage(st)
	writeJSON(w, map[string]any{"stage_id": st.ID, "authorize_url": authURL, "expires_at": st.ExpiresAt})
}

// handleOAuthStatus GET /api/channels/{p}/accounts/oauth/{stage}
func (g *Gateway) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	st := g.stageForRoute(r.PathValue("stage"))
	if st == nil {
		http.Error(w, "stage not found or expired", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"status": st.Status, "error": st.Error, "account_id": st.AccountID,
		"authorize_url": st.AuthorizeURL, "expires_at": st.ExpiresAt,
	})
}

// handleOAuthSubmitCode POST /api/channels/{provider}/accounts/oauth/{stage}/code
// body: {code_or_url} —— 用户从回调页地址栏复制的完整 URL 或裸 code。
func (g *Gateway) handleOAuthSubmitCode(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	st := g.stageForRoute(r.PathValue("stage"))
	if st == nil {
		http.Error(w, "stage not found or expired", http.StatusNotFound)
		return
	}
	if st.Status != "pending" {
		http.Error(w, "stage already "+st.Status, http.StatusConflict)
		return
	}
	var req struct {
		CodeOrURL string `json:"code_or_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	code := extractOAuthCode(req.CodeOrURL)
	if code == "" {
		http.Error(w, "no code found in input", http.StatusBadRequest)
		return
	}

	snap := g.Snapshots.Load()
	profile, ok := snap.Config.OAuthProfiles[st.ProfileName]
	if !ok {
		http.Error(w, "profile gone", http.StatusInternalServerError)
		return
	}

	bundle, err := g.exchangeOAuthCode(r.Context(), g.AdminClient, profile, code, st.Verifier)
	if err != nil {
		st.Status = "failed"
		st.Error = err.Error()
		writeJSON(w, map[string]any{"status": "failed", "error": err.Error()})
		return
	}

	// 加密 token 包落库为 oauth 型账号
	encBundle, err := json.Marshal(bundle)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	masterKey, err := keys.MasterKeyFromHex(g.masterKeyHex)
	if err != nil {
		http.Error(w, "master key 未配置", http.StatusInternalServerError)
		return
	}
	encTok, err := keys.Encrypt(encBundle, masterKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := st.Name
	if name == "" {
		name = fmt.Sprintf("oauth-%s-%s", st.ProfileName, st.ID[:8])
	}
	a := &store.UpstreamAccount{
		ChannelID:      st.ChannelID,
		Name:           name,
		EncryptedKey:   nil, // oauth 型无静态 key；指纹取 token 包指纹防重复
		KeyFingerprint: keys.Fingerprint(bundle.RefreshToken, masterKey),
		MaxConcurrency: st.MaxConcurrency,
		Enabled:        true,
		CredentialType: "oauth",
		OAuthProfile:   st.ProfileName,
		EncryptedToken: encTok,
		TokenExpiresAt: bundle.ExpiresAt,
		LastRefreshAt:  time.Now(),
	}
	if err := g.DB.CreateUpstreamAccount(a); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.oauthCachePut(a.ID, bundle.AccessToken, bundle.AccountID, bundle.ExpiresAt)
	g.reloadChannels()

	st.Status = "completed"
	st.AccountID = a.ID
	writeJSON(w, map[string]any{"status": "completed", "account_id": a.ID, "name": a.Name})
}

// handleOAuthCancel DELETE /api/channels/{p}/accounts/oauth/{stage}
func (g *Gateway) handleOAuthCancel(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	st := g.stageForRoute(r.PathValue("stage"))
	if st == nil {
		http.Error(w, "stage not found or expired", http.StatusNotFound)
		return
	}
	st.Status = "cancelled"
	writeJSON(w, map[string]string{"status": "cancelled"})
}

// exchangeOAuthCode 用授权码换 token（PKCE）。
func (g *Gateway) exchangeOAuthCode(ctx context.Context, adminClient *http.Client, profile config.OAuthProfile, code, verifier string) (*oauthTokenBundle, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", profile.RedirectURI)
	form.Set("client_id", profile.ClientID)
	form.Set("code_verifier", verifier)
	return g.oauthTokenRequest(ctx, adminClient, profile.TokenURL, form)
}

// oauthTokenRequest 发 token 请求并解析公共字段。
func (g *Gateway) oauthTokenRequest(ctx context.Context, adminClient *http.Client, tokenURL string, form url.Values) (*oauthTokenBundle, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := adminClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("token response: %w", err)
	}
	if raw.Error != "" {
		return nil, fmt.Errorf("oauth error %s: %s", raw.Error, raw.ErrorDescription)
	}
	if resp.StatusCode != http.StatusOK || raw.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint status %d", resp.StatusCode)
	}
	bundle := &oauthTokenBundle{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second),
	}
	if raw.IDToken != "" {
		bundle.AccountID = chatgptAccountIDFromIDToken(raw.IDToken)
	}
	return bundle, nil
}

// chatgptAccountIDFromIDToken 从 id_token JWT payload 提取 chatgpt_account_id
// （claim 可能在顶层或 https://api.openai.com/auth 对象里，codex-rs token_data 语义）。
func chatgptAccountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if v, ok := claims["chatgpt_account_id"].(string); ok && v != "" {
		return v
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if v, ok := auth["chatgpt_account_id"].(string); ok {
			return v
		}
	}
	return ""
}

// extractOAuthCode 从完整回调 URL 或裸 code 中提取 code。
func extractOAuthCode(input string) string {
	input = strings.TrimSpace(input)
	if !strings.Contains(input, "://") {
		return input
	}
	u, err := url.Parse(input)
	if err != nil {
		return ""
	}
	return u.Query().Get("code")
}

// ---- 内存 token cache + 刷新调度 ----

func (g *Gateway) oauthCachePut(accountID int64, accessToken, chatgptAccountID string, expiresAt time.Time) {
	g.oauthMu.Lock()
	defer g.oauthMu.Unlock()
	g.oauthCache[accountID] = oauthLiveToken{AccessToken: accessToken, AccountID: chatgptAccountID, ExpiresAt: expiresAt}
}

// credentialFor 数据面取该账号当前可用凭据：api_key 型直接返回静态 key；
// oauth 型取内存 token cache（缺失时退回快照里预热的值）。
// chatgpt_codex 样式附带上游必需 header。
func (g *Gateway) credentialFor(snap *routing.Snapshot, ref *pool.AccountRef) (string, map[string]string) {
	if ref.Spec.CredentialType != "oauth" {
		return ref.Spec.Credential, nil
	}
	g.oauthMu.Lock()
	tok, ok := g.oauthCache[ref.Spec.ID]
	g.oauthMu.Unlock()
	cred := ref.Spec.Credential
	var accountID string
	if ok {
		cred = tok.AccessToken
		accountID = tok.AccountID
	}
	profile := snap.Config.OAuthProfiles[ref.Spec.OAuthProfile]
	if profile.UpstreamAuthStyle == "chatgpt_codex" {
		return cred, map[string]string{
			"chatgpt-account-id": accountID,
			"OpenAI-Beta":        "responses=experimental",
		}
	}
	return cred, nil
}

// oauthRefreshLoop 后台续期：过期前 5 分钟用 refresh_token 换新；
// 失败冷却账号 10 分钟（数据面正常逻辑会自然重试）。
func (g *Gateway) oauthRefreshLoop() {
	if g.DB == nil {
		return
	}
	for {
		time.Sleep(30 * time.Second)
		accounts, err := g.DB.ListUpstreamAccounts()
		if err != nil {
			continue
		}
		masterKey, err := keys.MasterKeyFromHex(g.masterKeyHex)
		if err != nil {
			continue
		}
		for _, a := range accounts {
			if a.CredentialType != "oauth" || !a.Enabled || a.OAuthProfile == "" || len(a.EncryptedToken) == 0 {
				continue
			}
			if !a.TokenExpiresAt.IsZero() && time.Until(a.TokenExpiresAt) > 5*time.Minute {
				continue
			}
			plain, err := keys.Decrypt(a.EncryptedToken, masterKey)
			if err != nil {
				log.Printf("oauth account %d(%s): decrypt token: %v", a.ID, a.Name, err)
				continue
			}
			var bundle oauthTokenBundle
			if err := json.Unmarshal(plain, &bundle); err != nil {
				log.Printf("oauth account %d(%s): token bundle: %v", a.ID, a.Name, err)
				continue
			}
			profile, ok := g.Snapshots.Load().Config.OAuthProfiles[a.OAuthProfile]
			if !ok {
				continue
			}
			form := url.Values{}
			form.Set("grant_type", "refresh_token")
			form.Set("refresh_token", bundle.RefreshToken)
			form.Set("client_id", profile.ClientID)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			newBundle, err := g.oauthTokenRequest(ctx, g.AdminClient, profile.TokenURL, form)
			cancel()
			if err != nil {
				log.Printf("oauth account %d(%s): refresh failed: %v", a.ID, a.Name, err)
				if ref := g.findAccountRef(a.ID); ref != nil {
					g.Pool.Cool(ref, 10*time.Minute, "oauth refresh failed")
					ref.State.RecordResult(0, "transport", "oauth refresh: "+err.Error())
				}
				continue
			}
			if newBundle.RefreshToken == "" {
				newBundle.RefreshToken = bundle.RefreshToken // 部分供应商不轮换 refresh_token
			}
			if newBundle.AccountID == "" {
				newBundle.AccountID = bundle.AccountID
			}
			out, err := json.Marshal(newBundle)
			if err != nil {
				continue
			}
			enc, err := keys.Encrypt(out, masterKey)
			if err != nil {
				continue
			}
			if err := g.DB.SetUpstreamToken(a.ID, enc, newBundle.ExpiresAt); err != nil {
				log.Printf("oauth account %d(%s): persist token: %v", a.ID, a.Name, err)
				continue
			}
			g.oauthCachePut(a.ID, newBundle.AccessToken, newBundle.AccountID, newBundle.ExpiresAt)
			log.Printf("oauth account %d(%s): token refreshed, expires %s", a.ID, a.Name, newBundle.ExpiresAt.Format(time.RFC3339))
		}
	}
}

// findAccountRef 在当前快照里按账号 ID 找运行态引用。
func (g *Gateway) findAccountRef(id int64) *pool.AccountRef {
	for _, refs := range g.Snapshots.Load().Accounts {
		for _, ref := range refs {
			if ref.Spec.ID == id {
				return ref
			}
		}
	}
	return nil
}

// buildAccountCredential 解密 oauth 账号 token 包并预热内存 cache；
// 返回当前 access token（快照预热的 fallback 值）。
func (g *Gateway) buildAccountCredential(a store.UpstreamAccount) (cred string, credType, profileName string) {
	if a.CredentialType != "oauth" || len(a.EncryptedToken) == 0 {
		return string(a.EncryptedKey), a.CredentialType, a.OAuthProfile
	}
	masterKey, err := keys.MasterKeyFromHex(g.masterKeyHex)
	if err != nil {
		return "", a.CredentialType, a.OAuthProfile
	}
	plain, err := keys.Decrypt(a.EncryptedToken, masterKey)
	if err != nil {
		log.Printf("account %d(%s): decrypt oauth token: %v", a.ID, a.Name, err)
		return "", a.CredentialType, a.OAuthProfile
	}
	var bundle oauthTokenBundle
	if err := json.Unmarshal(plain, &bundle); err != nil {
		log.Printf("account %d(%s): oauth bundle: %v", a.ID, a.Name, err)
		return "", a.CredentialType, a.OAuthProfile
	}
	g.oauthCachePut(a.ID, bundle.AccessToken, bundle.AccountID, bundle.ExpiresAt)
	return bundle.AccessToken, "oauth", a.OAuthProfile
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
