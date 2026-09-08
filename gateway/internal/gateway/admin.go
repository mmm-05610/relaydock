package gateway

import (
	"context"
	"encoding/csv"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gateway/internal/config"
	"gateway/internal/keys"
	"gateway/internal/proxy"
	"gateway/internal/routing"
	"gateway/internal/store"
)

// passwordMu 保护 panelPassword：管理 API 支持运行中改口令。
type adminAuth struct {
	mu       sync.Mutex
	password string // 管理面板口令（PANEL_PASSWORD），空 = 无认证（仅开发）
}

func (a *adminAuth) set(p string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.password = p
}

func (a *adminAuth) get() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.password
}

// requireAuth 校验管理 API 口令（Bearer PANEL_PASSWORD）；未配置口令时放行（开发）。
// ConstantTimeCompare 防时序攻击。
func (g *Gateway) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if g.panelPassword.get() == "" {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(g.panelPassword.get())) == 1 {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// maskKey 脱敏：只留前 8 位，其余用 * 掩盖。
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:8] + "****"
}

// ---- keys ----

func (g *Gateway) handleListKeys(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	ks, err := g.KeyMgr.ListKeys()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ks == nil {
		ks = []keys.Key{} // nil 序列化为 null，前端数组消费方会崩
	}
	writeJSON(w, ks)
}

func (g *Gateway) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	var req struct {
		Name          string  `json:"name"`
		Owner         string  `json:"owner"`
		AgentType     string  `json:"agent_type"`
		Quota         float64 `json:"quota"`
		AllowedModels string  `json:"allowed_models"`
		ExpiresIn     string  `json:"expires_in"` // never|1h|1d|7d|30d|90d
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	raw, err := g.KeyMgr.CreateKeyWithModels(req.Name, req.Owner, req.AgentType, req.Quota, req.AllowedModels, expiresFromPreset(req.ExpiresIn))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"key": raw, "note": "仅此一次可见，请保存"})
}

func (g *Gateway) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	var req struct {
		KeyHash string `json:"key_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := g.KeyMgr.RevokeByHash(req.KeyHash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "revoked"})
}

func (g *Gateway) handleGetKey(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	hash := r.PathValue("hash")
	k, err := g.KeyMgr.GetKey(hash)
	if err != nil || k == nil {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	var logs []store.UsageLog
	if g.DB != nil {
		logs, _ = g.DB.QueryLogs(store.LogFilter{KeyID: k.ID, Limit: 20})
	}
	var cost float64
	var tokens int64
	for _, l := range logs {
		cost += l.Cost
		tokens += l.InputTokens + l.OutputTokens
	}
	writeJSON(w, map[string]any{
		"key": k, "cost": cost, "tokens": tokens, "requests": len(logs), "recent": logs,
	})
}

func (g *Gateway) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	hash := r.PathValue("hash")
	var req struct {
		Name          string  `json:"name"`
		Owner         string  `json:"owner"`
		AgentType     string  `json:"agent_type"`
		Quota         float64 `json:"quota"`
		AllowedModels string  `json:"allowed_models"`
		ExpiresIn     string  `json:"expires_in"` // never|1h|1d|7d|30d|90d|keep（编辑时 keep = 不变）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	exp, err := expiresAtForUpdate(hash, req.ExpiresIn, g.KeyMgr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := g.KeyMgr.UpdateKey(keys.Key{
		KeyHash: hash, Name: req.Name, Owner: req.Owner, AgentType: req.AgentType,
		QuotaLimit: req.Quota, AllowedModels: req.AllowedModels, ExpiresAt: exp,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (g *Gateway) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	hash := r.PathValue("hash")
	raw, err := g.KeyMgr.Rotate(hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"key": raw, "note": "新 key 仅此一次可见，请保存"})
}

func (g *Gateway) handleUpdateQuota(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	hash := r.PathValue("hash")
	var req struct {
		Quota float64 `json:"quota"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := g.KeyMgr.UpdateQuota(hash, req.Quota); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ---- usage ----

func (g *Gateway) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	if g.DB == nil {
		writeJSON(w, store.UsageStats{})
		return
	}
	st, err := g.DB.GetUsageStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

func (g *Gateway) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	if g.DB == nil {
		writeJSON(w, store.DashboardStats{})
		return
	}
	st, err := g.DB.GetDashboard()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

func (g *Gateway) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	tr := parseTimeRange(r)
	if g.DB == nil {
		writeJSON(w, []store.TimeseriesPoint{})
		return
	}
	pts, err := g.DB.GetTimeseries(tr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if pts == nil {
		pts = []store.TimeseriesPoint{}
	}
	writeJSON(w, pts)
}

func (g *Gateway) handleGrouped(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "model"
	}
	if g.DB == nil {
		writeJSON(w, []store.GroupedUsage{})
		return
	}
	gr, err := g.DB.GetGrouped(by, parseTimeRange(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if gr == nil {
		gr = []store.GroupedUsage{}
	}
	writeJSON(w, gr)
}

func (g *Gateway) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	filter := store.LogFilter{Limit: 50}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Offset = n
		}
	}
	filter.Model = r.URL.Query().Get("model")
	if v := r.URL.Query().Get("status"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Status = n
		}
	}
	if g.DB == nil {
		writeJSON(w, []store.UsageLog{})
		return
	}
	logs, err := g.DB.QueryLogs(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if logs == nil {
		logs = []store.UsageLog{}
	}
	writeJSON(w, logs)
}

// ---- channels & upstream keys ----

// handleChannels 返回渠道列表（含 key 状态）。
func (g *Gateway) handleChannels(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	snap := g.Snapshots.Load()
	out := []map[string]any{}
	for _, ch := range snap.Config.Channels {
		out = append(out, map[string]any{
			"id":             ch.ID,
			"provider":       ch.Provider,
			"name":           ch.Name,
			"enabled":        ch.Enabled,
			"balance_type":   ch.BalanceType,
			"balance_url":    ch.BalanceURL,
			"models_url":     ch.ModelsURL,
			"auth_mode":      ch.AuthMode,
			"key_configured": snap.UpstreamKey(ch.Provider) != "",
			"key_prefix":     maskKey(snap.UpstreamKey(ch.Provider)),
			"preset":         ch.Preset,
			"models":         ch.Models,
		})
	}
	writeJSON(w, out)
}

func (g *Gateway) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	var ch config.Channel
	if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if ch.Provider == "" || ch.Name == "" {
		http.Error(w, "provider/name required", http.StatusBadRequest)
		return
	}
	if g.DB == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := g.DB.CreateChannel(ch); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (g *Gateway) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	var ch config.Channel
	if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	ch.Provider = provider
	if g.DB == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := g.DB.UpdateChannel(ch); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (g *Gateway) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	if g.DB == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := g.DB.DeleteChannel(provider); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (g *Gateway) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	var m config.Model
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if m.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if g.DB == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := g.DB.CreateModel(provider, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (g *Gateway) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	name := r.PathValue("name")
	var m config.Model
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	m.Name = name
	if g.DB == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := g.DB.UpdateModel(provider, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (g *Gateway) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	name := r.PathValue("name")
	if g.DB == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := g.DB.DeleteModel(provider, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	g.reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleSetChannelKey 录入/更新渠道 API key（加密存 PG + 发布新快照）。
func (g *Gateway) handleSetChannelKey(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "empty key", http.StatusBadRequest)
		return
	}
	if g.DB != nil {
		masterKey, err := keys.MasterKeyFromHex(g.masterKeyHex)
		if err != nil {
			http.Error(w, "master key 未配置", http.StatusInternalServerError)
			return
		}
		enc, err := keys.Encrypt([]byte(req.Key), masterKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := g.DB.SetUpstreamKey(provider, enc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	g.Snapshots.Update(func(cur *routing.Snapshot) *routing.Snapshot {
		return cur.WithUpstreamKey(provider, req.Key)
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleTestChannel 测试渠道连通性。
//   - 带 model: 测该模型真实调用
//   - 不带 model: 测该渠道第一个 enabled 模型的真实调用（订阅制套餐没 balance_url 也可用）
func (g *Gateway) handleTestChannel(w http.ResponseWriter, r *http.Request) {
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
	key := snap.UpstreamKey(provider)
	if key == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "未配置 key"})
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// 选定被测模型：只从当前渠道里找（避免 FindModel 全局查撞名，误用别的渠道的模型配置）
	var target *config.Model
	if req.Model != "" {
		for i := range ch.Models {
			if ch.Models[i].Name == req.Model {
				target = &ch.Models[i]
				break
			}
		}
		if target == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "model not found"})
			return
		}
	} else {
		for i := range ch.Models {
			if ch.Models[i].Enabled {
				target = &ch.Models[i]
				break
			}
		}
		if target == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "渠道无 enabled 模型"})
			return
		}
	}
	writeJSON(w, g.testModelCall(r.Context(), *target, ch, key))
}

// testModelCall 发最小请求测试模型可调用性。
// 优先 /v1/messages（anthropic 协议）；fallback 到任意一条路由（按对应协议的 body 格式）。
func (g *Gateway) testModelCall(ctx context.Context, m config.Model, ch *config.Channel, key string) map[string]any {
	route, ok := m.Routes["/v1/messages"]
	if !ok {
		for _, rt := range m.Routes {
			route = rt
			ok = true
			break
		}
	}
	if !ok {
		return map[string]any{"ok": false, "error": "无路由"}
	}
	// 按协议选最小 body 格式
	var body string
	switch route.Usage {
	case "responses":
		body = fmt.Sprintf(`{"model":%q,"input":"hi","max_output_tokens":1}`, route.Model)
	case "chat_completions":
		body = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1}`, route.Model)
	default: // anthropic
		body = fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, route.Model)
	}
	req, err := proxy.BuildRequest(ctx, http.MethodPost, []byte(body), nil, proxy.Target{
		URL:      route.Upstream,
		AuthMode: ch.AuthMode,
		Key:      key,
		Protocol: route.Usage,
	})
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	start := time.Now()
	resp, err := g.AdminClient.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error(), "latency_ms": latency}
	}
	defer resp.Body.Close()
	return map[string]any{"ok": resp.StatusCode == 200, "status": resp.StatusCode, "latency_ms": latency}
}

// handleChannelBalance 查询单个渠道的余额/余量。
func (g *Gateway) handleChannelBalance(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, g.queryChannelBalance(*ch, snap.UpstreamKey(provider)))
}

// handleRemoteModels 从上游拉取模型列表。
func (g *Gateway) handleRemoteModels(w http.ResponseWriter, r *http.Request) {
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
	if ch.ModelsURL == "" {
		writeJSON(w, map[string]any{"error": "未配置 models_url"})
		return
	}
	key := snap.UpstreamKey(provider)
	if key == "" {
		writeJSON(w, map[string]any{"error": "未配置 key"})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, ch.ModelsURL, nil)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := g.AdminClient.Do(req)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var d struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&d)
	var names []string
	for _, m := range d.Data {
		names = append(names, m.ID)
	}
	writeJSON(w, names)
}

// handleUpstreamStatus 返回各渠道 key 配置状态。
func (g *Gateway) handleUpstreamStatus(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	snap := g.Snapshots.Load()
	status := map[string]bool{}
	for _, ch := range snap.Config.Channels {
		status[ch.Provider] = snap.UpstreamKey(ch.Provider) != ""
	}
	writeJSON(w, status)
}

func (g *Gateway) handleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Password == "" {
		http.Error(w, "empty password", http.StatusBadRequest)
		return
	}
	g.panelPassword.set(req.Password)
	writeJSON(w, map[string]string{"status": "ok", "note": "重启后恢复为 env PANEL_PASSWORD"})
}

// handleUpstreamBalance 查询所有渠道的余额/余量。
func (g *Gateway) handleUpstreamBalance(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	snap := g.Snapshots.Load()
	out := map[string]any{}
	for _, ch := range snap.Config.Channels {
		out[ch.Provider] = g.queryChannelBalance(ch, snap.UpstreamKey(ch.Provider))
	}
	writeJSON(w, out)
}

// queryChannelBalance 按渠道配置查询余额/余量，按 balance_type 分发解析。
func (g *Gateway) queryChannelBalance(ch config.Channel, key string) map[string]any {
	if key == "" {
		return map[string]any{"error": "未配置 key"}
	}
	req, _ := http.NewRequest(http.MethodGet, ch.BalanceURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := g.AdminClient.Do(req)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	if ch.BalanceType == "quota" {
		return parseQuota(resp.Body)
	}
	return parseBalance(resp.Body)
}

// parseBalance 余额型：解析 total_balance（金额）。
func parseBalance(r io.Reader) map[string]any {
	var d struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	_ = json.NewDecoder(r).Decode(&d)
	total, currency := "", ""
	if len(d.BalanceInfos) > 0 {
		total = d.BalanceInfos[0].TotalBalance
		currency = d.BalanceInfos[0].Currency
	}
	return map[string]any{"is_available": d.IsAvailable, "currency": currency, "total_balance": total}
}

// parseQuota 余量型：解析 5h/7天 剩余百分比。
func parseQuota(r io.Reader) map[string]any {
	var d struct {
		ModelRemains []struct {
			ModelName                       string  `json:"model_name"`
			CurrentIntervalRemainingPercent float64 `json:"current_interval_remaining_percent"`
			CurrentWeeklyRemainingPercent   float64 `json:"current_weekly_remaining_percent"`
		} `json:"model_remains"`
	}
	_ = json.NewDecoder(r).Decode(&d)
	var interval, weekly float64
	for _, m := range d.ModelRemains {
		if m.ModelName == "general" || strings.HasPrefix(m.ModelName, "MiniMax-M") {
			interval = m.CurrentIntervalRemainingPercent
			weekly = m.CurrentWeeklyRemainingPercent
			break
		}
	}
	return map[string]any{"interval_remain_percent": interval, "weekly_remain_percent": weekly}
}


// expiresFromPreset 过期快捷档解析（never = 永不过期）。
func expiresFromPreset(preset string) time.Time {
	switch preset {
	case "1h":
		return time.Now().Add(time.Hour)
	case "1d":
		return time.Now().AddDate(0, 0, 1)
	case "7d":
		return time.Now().AddDate(0, 0, 7)
	case "30d":
		return time.Now().AddDate(0, 0, 30)
	case "90d":
		return time.Now().AddDate(0, 0, 90)
	default:
		return time.Time{}
	}
}

// expiresAtForUpdate 编辑时解析过期时间：空/keep = 保持不变；never = 清除。
func expiresAtForUpdate(hash, preset string, mgr *keys.Manager) (*time.Time, error) {
	switch preset {
	case "", "keep":
		if cur, err := mgr.GetKey(hash); err == nil && cur != nil {
			return cur.ExpiresAt, nil
		}
		return nil, nil
	case "never":
		return nil, nil
	default:
		t := expiresFromPreset(preset)
		if t.IsZero() {
			return nil, nil
		}
		return &t, nil
	}
}

// handleUsageOverview GET /api/usage/overview?days=7 —— 单端点聚合（summary+series+分布 top5+Other）。
func (g *Gateway) handleUsageOverview(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	if g.DB == nil {
		writeJSON(w, store.UsageOverview{Series: []store.UsageOverviewPoint{}, ByModel: []store.UsageDistribution{}, ByKey: []store.UsageDistribution{}})
		return
	}
	ov, err := g.DB.GetUsageOverview(days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ov.Series == nil {
		ov.Series = []store.UsageOverviewPoint{}
	}
	if ov.ByModel == nil {
		ov.ByModel = []store.UsageDistribution{}
	}
	if ov.ByKey == nil {
		ov.ByKey = []store.UsageDistribution{}
	}
	writeJSON(w, ov)
}

// handleLogsExport GET /api/logs/export?days=30 —— CSV 导出（流式，含 BOM 便于 Excel）。
func (g *Gateway) handleLogsExport(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	logs, err := g.DB.QueryLogs(store.LogFilter{Days: days, Limit: 100000})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=relaydock-usage-%dd.csv", days))
	w.Write([]byte("\xef\xbb\xbf")) // UTF-8 BOM
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"时间", "模型", "上游模型", "协议", "状态", "输入tokens", "输出tokens", "缓存读", "缓存写", "成本(元)", "延迟ms", "尝试次数", "request_id", "错误"})
	for _, l := range logs {
		_ = cw.Write([]string{
			l.CreatedAt.Format("2006-01-02 15:04:05"), l.Model, l.UpstreamModel, l.Protocol,
			strconv.Itoa(l.Status),
			strconv.FormatInt(l.InputTokens, 10), strconv.FormatInt(l.OutputTokens, 10),
			strconv.FormatInt(l.CacheReadTokens, 10), strconv.FormatInt(l.CacheWriteTokens, 10),
			fmt.Sprintf("%.6f", l.Cost), strconv.FormatInt(l.LatencyMs, 10), strconv.Itoa(l.Attempts),
			l.RequestID, l.Error,
		})
	}
	cw.Flush()
}
