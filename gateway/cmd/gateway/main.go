package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gateway/internal/config"
	"gateway/internal/keys"
	"gateway/internal/metering"
	"gateway/internal/store"
)

var (
	cfg           *config.Config
	upstreamKeys  map[string]string // provider -> 明文 key
	keyMgr        *keys.Manager
	meter         *metering.Meter
	db            *store.PgStore // nil = 内存模式
	panelPassword string          // 管理面板口令（PANEL_PASSWORD）
)

func main() {
	// 子命令：gateway keys set-upstream --provider <name>
	if len(os.Args) > 1 {
		if os.Args[1] == "keys" && len(os.Args) > 2 && os.Args[2] == "set-upstream" {
			runSetUpstream(os.Args[3:])
			return
		}
		fmt.Println("用法: gateway [keys set-upstream --provider <deepseek|minimax>]")
		return
	}

	var err error
	cfg, err = config.Load("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("loaded %d channels", len(cfg.Channels))

	// 存储：有 DATABASE_URL 用 PG，否则内存（本地开发）
	ctx := context.Background()
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		pg, err := store.NewPgStore(ctx, dbURL)
		if err != nil {
			log.Fatalf("connect pg: %v", err)
		}
		db = pg
		keyMgr = keys.NewManager(pg)
		log.Printf("using PostgreSQL store")
	} else {
		keyMgr = keys.NewManager(store.NewMemStore())
		if raw, err := keyMgr.CreateKey("bootstrap", "", "", 0); err == nil {
			log.Printf("bootstrap key (仅此一次可见): %s", raw)
		}
		log.Printf("using in-memory store (no DATABASE_URL)")
	}

	// 渠道从 PG 加载（空则从 config.yaml 种子导入）
	if db != nil {
		channels, err := db.LoadChannels()
		if err != nil {
			log.Fatalf("load channels: %v", err)
		}
		if len(channels) > 0 {
			cfg.Channels = channels
			log.Printf("loaded %d channels from DB", len(channels))
		} else {
			for _, ch := range cfg.Channels {
				ch.Enabled = true
				for i := range ch.Models {
					ch.Models[i].Enabled = true
				}
				if err := db.CreateChannel(ch); err != nil {
					log.Printf("seed channel %s: %v", ch.Provider, err)
				}
			}
			log.Printf("seeded %d channels from config.yaml", len(cfg.Channels))
		}
	}

	loadUpstreamKeys()

	panelPassword = os.Getenv("PANEL_PASSWORD")
	if panelPassword == "" {
		log.Printf("⚠️ 未设置 PANEL_PASSWORD，管理 API 无认证（仅限开发）")
	}

	meter = metering.NewMeter()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", handleModels) // 模型列表（OpenAI 兼容）
	mux.HandleFunc("/v1/", handleProxy)             // 所有 /v1/* 透传
	mux.HandleFunc("GET /api/keys", handleListKeys)
	mux.HandleFunc("POST /api/keys", handleCreateKey)
	mux.HandleFunc("POST /api/keys/revoke", handleRevokeKey)
	mux.HandleFunc("GET /api/keys/{hash}", handleGetKey)
	mux.HandleFunc("PUT /api/keys/{hash}", handleUpdateKey)
	mux.HandleFunc("POST /api/keys/{hash}/rotate", handleRotateKey)
	mux.HandleFunc("POST /api/keys/{hash}/quota", handleUpdateQuota)
	mux.HandleFunc("GET /api/usage", handleUsage)
	mux.HandleFunc("GET /api/dashboard", handleDashboard)
	mux.HandleFunc("GET /api/usage/timeseries", handleTimeseries)
	mux.HandleFunc("GET /api/usage/grouped", handleGrouped)
	mux.HandleFunc("GET /api/logs", handleLogs)
	mux.HandleFunc("GET /api/channels", handleChannels)
	mux.HandleFunc("POST /api/channels", handleCreateChannel)
	mux.HandleFunc("PUT /api/channels/{provider}", handleUpdateChannel)
	mux.HandleFunc("DELETE /api/channels/{provider}", handleDeleteChannel)
	mux.HandleFunc("POST /api/channels/{provider}/key", handleSetChannelKey)
	mux.HandleFunc("POST /api/channels/{provider}/test", handleTestChannel)
	mux.HandleFunc("GET /api/channels/{provider}/balance", handleChannelBalance)
	mux.HandleFunc("GET /api/channels/{provider}/remote-models", handleRemoteModels)
	mux.HandleFunc("POST /api/channels/{provider}/models", handleCreateModel)
	mux.HandleFunc("PUT /api/channels/{provider}/models/{name}", handleUpdateModel)
	mux.HandleFunc("DELETE /api/channels/{provider}/models/{name}", handleDeleteModel)
	mux.HandleFunc("GET /api/settings/upstream", handleUpstreamStatus)
	mux.HandleFunc("POST /api/settings/password", handleUpdatePassword)
	mux.HandleFunc("GET /api/upstream/balance", handleUpstreamBalance)

	// 静态文件（前端面板），作为 fallback
	staticDir := os.Getenv("STATIC_DIR")
	if staticDir == "" {
		staticDir = "../navpage"
	}
	mux.Handle("/", http.FileServer(http.Dir(staticDir)))

	addr := ":8080"
	log.Printf("gateway listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// handleModels 返回可用模型列表（OpenAI 兼容格式，认证后按 key 权限过滤）。
func handleModels(w http.ResponseWriter, r *http.Request) {
	authKey, err := keyMgr.Authenticate(bearerToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	now := time.Now().Unix()
	type modelItem struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := []modelItem{}
	for _, ch := range cfg.Channels {
		if !ch.Enabled {
			continue
		}
		for _, m := range ch.Models {
			if !m.Enabled || !authKey.CanAccessModel(m.Name) {
				continue
			}
			data = append(data, modelItem{ID: m.Name, Object: "model", Created: now, OwnedBy: m.Provider})
		}
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

// handleProxy 协议无关透传：认证 -> 路由 -> 换上游 -> 原样转发。成功失败都落库。
func handleProxy(w http.ResponseWriter, r *http.Request) {
	rec := store.UsageLog{}
	start := time.Now()
	var authKey *keys.Key
	defer func() {
		rec.LatencyMs = time.Since(start).Milliseconds()
		recordUsage(rec, authKey)
	}()

	// 认证：Bearer 虚拟 key -> SHA-256 -> 查表 + 额度硬挡
	authKey, err := keyMgr.Authenticate(bearerToken(r))
	if err != nil {
		rec.Status = http.StatusUnauthorized
		rec.Error = err.Error()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rec.KeyID = authKey.ID

	body, err := io.ReadAll(r.Body)
	if err != nil {
		rec.Status = http.StatusBadRequest
		rec.Error = err.Error()
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	model := extractModel(body)
	if model == "" {
		rec.Status = http.StatusBadRequest
		rec.Error = "missing model"
		http.Error(w, "missing model", http.StatusBadRequest)
		return
	}

	m := cfg.FindModel(model)
	if m == nil {
		// 不记录 model：未知模型不进用量统计
		rec.Status = http.StatusNotFound
		rec.Error = fmt.Sprintf("unknown model %q", model)
		http.Error(w, rec.Error, http.StatusNotFound)
		return
	}
	rec.Model = model

	// 渠道/模型禁用检查
	if !m.Enabled {
		rec.Status = http.StatusNotFound
		rec.Error = "model disabled"
		http.Error(w, "model disabled", http.StatusNotFound)
		return
	}
	if ch := cfg.FindChannel(m.Provider); ch != nil && !ch.Enabled {
		rec.Status = http.StatusNotFound
		rec.Error = "channel disabled"
		http.Error(w, "channel disabled", http.StatusNotFound)
		return
	}

	// key 的模型访问权限检查
	if !authKey.CanAccessModel(model) {
		rec.Status = http.StatusForbidden
		rec.Error = "model not allowed"
		http.Error(w, "model not allowed", http.StatusForbidden)
		return
	}

	route, ok := m.FindRoute(r.URL.Path)
	if !ok {
		rec.Status = http.StatusNotFound
		rec.Error = fmt.Sprintf("no route for %q", r.URL.Path)
		http.Error(w, rec.Error, http.StatusNotFound)
		return
	}
	rec.UpstreamModel = route.Model
	rec.Protocol = route.Usage

	key := upstreamKeys[m.Provider]
	if key == "" {
		rec.Status = http.StatusInternalServerError
		rec.Error = "upstream key missing"
		http.Error(w, rec.Error, http.StatusInternalServerError)
		return
	}

	// 改 body.model 为上游 model 名（若不同）
	if route.Model != model {
		body = replaceModel(body, route.Model)
	}

	upReq, err := http.NewRequest(r.Method, route.Upstream, bytes.NewReader(body))
	if err != nil {
		rec.Status = http.StatusInternalServerError
		rec.Error = err.Error()
		http.Error(w, "build upstream", http.StatusInternalServerError)
		return
	}
	upReq.Header.Set("Authorization", "Bearer "+key)
	upReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(upReq)
	if err != nil {
		rec.Status = http.StatusBadGateway
		rec.Error = err.Error()
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rec.Status = resp.StatusCode

	// 透传响应 header
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 计量：pre-call 估算 input（兜底流式中断拿不到 usage）
	estInput := metering.EstimateInputTokens(string(body))

	// 流式透传 + 逐行 Feed 计量
	if flusher, ok := w.(http.Flusher); ok && isStream(resp.Header) {
		acc := meter.NewStreamAccumulator(route.Usage)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 1<<20) // 允许 1MB 长行
		for scanner.Scan() {
			line := scanner.Bytes()
			w.Write(line)
			w.Write([]byte("\n"))
			flusher.Flush()
			if d, ok := sseData(line); ok {
				acc.Feed(d)
				if rec.RequestID == "" {
					rec.RequestID = extractRequestID(d)
				}
			}
		}
		usage := acc.Usage()
		if usage.InputTokens == 0 && usage.OutputTokens == 0 {
			usage.InputTokens = estInput // 流式没拿到 usage（如 chat_completions），退估算
		}
		fillMetering(&rec, m, usage)
		return
	}

	// 非流式
	respBody, err := io.ReadAll(resp.Body)
	if err == nil {
		w.Write(respBody)
	}
	rec.RequestID = extractRequestID(respBody)
	fillMetering(&rec, m, meter.MeterNonStream(route.Usage, respBody))
}

func isStream(h http.Header) bool {
	return bytes.Contains([]byte(h.Get("Content-Type")), []byte("event-stream"))
}

func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func extractModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

// extractRequestID 从响应 JSON 提取 request id。
// 非流式：顶层 id；流式事件：anthropic 的 message.id 或 responses 的 response.id。
func extractRequestID(data []byte) string {
	var v struct {
		ID       string `json:"id"`
		Message  struct {
			ID string `json:"id"`
		} `json:"message"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return ""
	}
	if v.ID != "" {
		return v.ID
	}
	if v.Message.ID != "" {
		return v.Message.ID
	}
	return v.Response.ID
}

func replaceModel(body []byte, newModel string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = newModel
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// sseData 剥离 SSE 的 "data:" 前缀，返回 JSON 载荷。
func sseData(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	d := bytes.TrimSpace(line[len("data:"):])
	if len(d) == 0 || string(d) == "[DONE]" {
		return nil, false
	}
	return d, true
}

// fillMetering 填充用量记录的成本字段。
func fillMetering(rec *store.UsageLog, m *config.Model, usage metering.Usage) {
	p := metering.Pricing{
		InputPerM:      m.Pricing.InputPerM,
		OutputPerM:     m.Pricing.OutputPerM,
		CacheReadPerM:  m.Pricing.CacheReadPerM,
		CacheWritePerM: m.Pricing.CacheWritePerM,
	}
	rec.InputTokens = usage.InputTokens
	rec.OutputTokens = usage.OutputTokens
	rec.CacheReadTokens = usage.CacheReadTokens
	rec.CacheWriteTokens = usage.CacheWriteTokens
	rec.Cost = metering.Cost(usage, p)
	rec.Unmetered = usage.InputTokens == 0 && usage.OutputTokens == 0
}

// recordUsage 落库用量 + 累加额度（旁路，失败只 log 不阻断）。
func recordUsage(rec store.UsageLog, authKey *keys.Key) {
	log.Printf("usage model=%s proto=%s status=%d in=%d out=%d cost=$%.6f err=%s",
		rec.Model, rec.Protocol, rec.Status, rec.InputTokens, rec.OutputTokens, rec.Cost, rec.Error)
	if db == nil {
		return
	}
	if err := db.InsertUsageLog(rec); err != nil {
		log.Printf("insert usage log: %v", err)
	}
	if authKey != nil && rec.Cost > 0 {
		if err := db.AddQuotaUsed(authKey.KeyHash, rec.Cost); err != nil {
			log.Printf("add quota: %v", err)
		}
	}
}

// loadUpstreamKeys 从 PG（加密）或 env 加载上游 key。
func loadUpstreamKeys() {
	upstreamKeys = map[string]string{}
	envFallback := map[string]string{
		"deepseek": os.Getenv("DEEPSEEK_API_KEY"),
		"minimax":  os.Getenv("MINIMAX_API_KEY"),
	}
	masterKey, _ := keys.MasterKeyFromHex(os.Getenv("GATEWAY_MASTER_KEY"))

	seen := map[string]bool{}
	for _, ch := range cfg.Channels {
		if seen[ch.Provider] {
			continue
		}
		seen[ch.Provider] = true
		// PG 加密读取（master key 存在 + db 非空）
		if db != nil && masterKey != nil {
			if enc, err := db.GetUpstreamKey(ch.Provider); err == nil && len(enc) > 0 {
				if plain, err := keys.Decrypt(enc, masterKey); err == nil {
					upstreamKeys[ch.Provider] = string(plain)
					continue
				}
			}
		}
		upstreamKeys[ch.Provider] = envFallback[ch.Provider]
	}
}

func handleListKeys(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	ks, err := keyMgr.ListKeys()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, ks)
}

func handleUsage(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	if db == nil {
		writeJSON(w, store.UsageStats{})
		return
	}
	st, err := db.GetUsageStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	if db == nil {
		writeJSON(w, store.DashboardStats{})
		return
	}
	st, err := db.GetDashboard()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, st)
}

func parseTimeRange(r *http.Request) store.TimeRange {
	var tr store.TimeRange
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tr.Days = n
		}
	}
	tr.From = r.URL.Query().Get("from")
	tr.To = r.URL.Query().Get("to")
	return tr
}

func handleTimeseries(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	tr := parseTimeRange(r)
	if db == nil {
		writeJSON(w, []store.TimeseriesPoint{})
		return
	}
	pts, err := db.GetTimeseries(tr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, pts)
}

func handleGrouped(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "model"
	}
	if db == nil {
		writeJSON(w, []store.GroupedUsage{})
		return
	}
	g, err := db.GetGrouped(by, parseTimeRange(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, g)
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
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
	if db == nil {
		writeJSON(w, []store.UsageLog{})
		return
	}
	logs, err := db.QueryLogs(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, logs)
}

// handleChannels 返回渠道列表（含 key 状态）。
func handleChannels(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	out := []map[string]any{}
	for _, ch := range cfg.Channels {
		out = append(out, map[string]any{
			"provider":       ch.Provider,
			"name":           ch.Name,
			"balance_type":   ch.BalanceType,
			"balance_url":    ch.BalanceURL,
			"key_configured": upstreamKeys[ch.Provider] != "",
			"key_prefix":     maskKey(upstreamKeys[ch.Provider]),
			"models":         ch.Models,
		})
	}
	writeJSON(w, out)
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

// handleSetChannelKey 录入/更新渠道 API key（加密存 PG + 更新内存）。
func handleSetChannelKey(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
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
	if db != nil {
		masterKey, err := keys.MasterKeyFromHex(os.Getenv("GATEWAY_MASTER_KEY"))
		if err != nil {
			http.Error(w, "master key 未配置", http.StatusInternalServerError)
			return
		}
		enc, err := keys.Encrypt([]byte(req.Key), masterKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := db.SetUpstreamKey(provider, enc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	upstreamKeys[provider] = req.Key
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleTestChannel 测试渠道连通性。传 model 则测模型真实调用，否则测 balance_url。
func handleTestChannel(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	ch := cfg.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	key := upstreamKeys[provider]
	if key == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "未配置 key"})
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// 测模型真实调用（发最小请求）
	if req.Model != "" {
		if m := cfg.FindModel(req.Model); m != nil {
			writeJSON(w, testModelCall(*m, key))
			return
		}
		writeJSON(w, map[string]any{"ok": false, "error": "model not found"})
		return
	}

	// 测 balance_url 连通性
	start := time.Now()
	breq, _ := http.NewRequest("GET", ch.BalanceURL, nil)
	breq.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(breq)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "latency_ms": latency})
		return
	}
	defer resp.Body.Close()
	writeJSON(w, map[string]any{"ok": resp.StatusCode == 200, "status": resp.StatusCode, "latency_ms": latency})
}

// testModelCall 发最小请求测试模型可调用性（优先 anthropic 路由）。
func testModelCall(m config.Model, key string) map[string]any {
	route, ok := m.Routes["/v1/messages"]
	if !ok {
		for _, r := range m.Routes {
			route = r
			ok = true
			break
		}
	}
	if !ok {
		return map[string]any{"ok": false, "error": "无路由"}
	}
	body := fmt.Sprintf(`{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`, route.Model)
	req, _ := http.NewRequest("POST", route.Upstream, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error(), "latency_ms": latency}
	}
	defer resp.Body.Close()
	return map[string]any{"ok": resp.StatusCode == 200, "status": resp.StatusCode, "latency_ms": latency}
}

// handleChannelBalance 查询单个渠道的余额/余量。
func handleChannelBalance(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	ch := cfg.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	writeJSON(w, queryChannelBalance(*ch))
}

// handleRemoteModels 从上游拉取模型列表。
func handleRemoteModels(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	ch := cfg.FindChannel(provider)
	if ch == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	if ch.ModelsURL == "" {
		writeJSON(w, map[string]any{"error": "未配置 models_url"})
		return
	}
	key := upstreamKeys[provider]
	if key == "" {
		writeJSON(w, map[string]any{"error": "未配置 key"})
		return
	}
	req, _ := http.NewRequest("GET", ch.ModelsURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
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

// reloadChannels 从 PG 重新加载渠道到内存（热更新）。
func reloadChannels() {
	if db == nil {
		return
	}
	channels, err := db.LoadChannels()
	if err != nil {
		log.Printf("reload channels: %v", err)
		return
	}
	cfg.Channels = channels
}

func handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
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
	if db == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := db.CreateChannel(ch); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	var ch config.Channel
	if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	ch.Provider = provider
	if db == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := db.UpdateChannel(ch); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	if db == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := db.DeleteChannel(provider); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleCreateModel(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
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
	if db == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := db.CreateModel(provider, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
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
	if db == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := db.UpdateModel(provider, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	provider := r.PathValue("provider")
	name := r.PathValue("name")
	if db == nil {
		http.Error(w, "需要 PG", http.StatusInternalServerError)
		return
	}
	if err := db.DeleteModel(provider, name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reloadChannels()
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleUpstreamStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	status := map[string]bool{}
	for p, k := range upstreamKeys {
		status[p] = k != ""
	}
	writeJSON(w, status)
}

func handleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
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
	panelPassword = req.Password
	writeJSON(w, map[string]string{"status": "ok", "note": "重启后恢复为 env PANEL_PASSWORD"})
}

// handleUpstreamBalance 查询所有渠道的余额/余量。
func handleUpstreamBalance(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	out := map[string]any{}
	for _, ch := range cfg.Channels {
		out[ch.Provider] = queryChannelBalance(ch)
	}
	writeJSON(w, out)
}

// queryChannelBalance 按渠道配置查询余额/余量，按 balance_type 分发解析。
func queryChannelBalance(ch config.Channel) map[string]any {
	key := upstreamKeys[ch.Provider]
	if key == "" {
		return map[string]any{"error": "未配置 key"}
	}
	req, _ := http.NewRequest("GET", ch.BalanceURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
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

func handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	var req struct {
		Name          string  `json:"name"`
		Owner         string  `json:"owner"`
		AgentType     string  `json:"agent_type"`
		Quota         float64 `json:"quota"`
		AllowedModels string  `json:"allowed_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	raw, err := keyMgr.CreateKeyWithModels(req.Name, req.Owner, req.AgentType, req.Quota, req.AllowedModels)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"key": raw, "note": "仅此一次可见，请保存"})
}

func handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	var req struct {
		KeyHash string `json:"key_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := keyMgr.RevokeByHash(req.KeyHash); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "revoked"})
}

func handleGetKey(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	hash := r.PathValue("hash")
	k, err := keyMgr.GetKey(hash)
	if err != nil || k == nil {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	var logs []store.UsageLog
	if db != nil {
		logs, _ = db.QueryLogs(store.LogFilter{KeyID: k.ID, Limit: 20})
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

func handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	hash := r.PathValue("hash")
	var req struct {
		Name          string  `json:"name"`
		Owner         string  `json:"owner"`
		AgentType     string  `json:"agent_type"`
		Quota         float64 `json:"quota"`
		AllowedModels string  `json:"allowed_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := keyMgr.UpdateKey(keys.Key{
		KeyHash: hash, Name: req.Name, Owner: req.Owner, AgentType: req.AgentType,
		QuotaLimit: req.Quota, AllowedModels: req.AllowedModels,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleRotateKey(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	hash := r.PathValue("hash")
	raw, err := keyMgr.Rotate(hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"key": raw, "note": "新 key 仅此一次可见，请保存"})
}

func handleUpdateQuota(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
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
	if err := keyMgr.UpdateQuota(hash, req.Quota); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// requireAuth 校验管理 API 口令（Bearer PANEL_PASSWORD）；未配置口令时放行（开发）。
func requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if panelPassword == "" {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(panelPassword)) == 1 {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

// runSetUpstream 录入上游 key（AES 加密存 PG）。
func runSetUpstream(args []string) {
	provider := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--provider" && i+1 < len(args) {
			provider = args[i+1]
		}
	}
	if provider == "" {
		log.Fatal("用法: gateway keys set-upstream --provider <deepseek|minimax>")
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("需要 DATABASE_URL 环境变量")
	}
	masterKey, err := keys.MasterKeyFromHex(os.Getenv("GATEWAY_MASTER_KEY"))
	if err != nil {
		log.Fatalf("GATEWAY_MASTER_KEY 需 32 字节 hex: %v", err)
	}
	fmt.Printf("输入 %s 的 API key（会回显，注意遮挡）: ", provider)
	apiKey, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		log.Fatalf("读取失败: %v", err)
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		log.Fatal("key 不能为空")
	}
	encrypted, err := keys.Encrypt([]byte(apiKey), masterKey)
	if err != nil {
		log.Fatalf("加密失败: %v", err)
	}
	ctx := context.Background()
	pg, err := store.NewPgStore(ctx, dbURL)
	if err != nil {
		log.Fatalf("连 PG: %v", err)
	}
	if err := pg.SetUpstreamKey(provider, encrypted); err != nil {
		log.Fatalf("存 PG: %v", err)
	}
	fmt.Printf("✅ 已加密存储 %s 的 key（重启 gateway 生效）\n", provider)
}
