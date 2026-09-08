package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"gateway/internal/config"
	"gateway/internal/keys"
	"gateway/internal/metering"
	"gateway/internal/proxy"
	"gateway/internal/store"
)

// handleModels 返回可用模型列表（OpenAI 兼容格式，认证后按 key 权限过滤）。
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	snap := g.Snapshots.Load()
	authKey, err := g.KeyMgr.Authenticate(bearerToken(r))
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
	for _, ch := range snap.Config.Channels {
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
// 渠道配置了显式账号时走账号池路径（failover），否则走隐式单 key 路径（兼容现状）。
func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	rec := store.UsageLog{Attempts: 1}
	start := time.Now()
	var authKey *keys.Key
	loggable := false // 只有解析到真实模型路由的请求才落库（过滤 /v1/models、count_tokens 等噪声）
	defer func() {
		rec.LatencyMs = time.Since(start).Milliseconds()
		if loggable {
			g.recordUsage(rec, authKey)
		}
	}()

	snap := g.Snapshots.Load()

	// 认证：Bearer 虚拟 key -> SHA-256 -> 查表 + 额度硬挡
	authKey, err := g.KeyMgr.Authenticate(bearerToken(r))
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

	m := snap.FindModel(model)
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
	ch := snap.FindChannel(m.Provider)
	if ch != nil && !ch.Enabled {
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
	loggable = true // 真实模型请求，落库
	if ch != nil {
		rec.ChannelID = ch.ID
	}

	// 显式账号池路径（有账号才走 failover；否则保持单 key 兼容行为）
	if ch != nil {
		if refs := snap.AccountsFor(ch); len(refs) > 0 {
			g.proxyWithAccounts(w, r, m, ch, route, refs, body, model, &rec)
			return
		}
	}

	key := snap.UpstreamKey(m.Provider)
	if key == "" {
		rec.Status = http.StatusInternalServerError
		rec.Error = "upstream key missing"
		http.Error(w, rec.Error, http.StatusInternalServerError)
		return
	}

	// 改 body.model 为上游 model 名（若不同）
	if route.Model != model {
		body = proxy.ReplaceModel(body, route.Model)
	}
	upReq, err := proxy.BuildRequest(r.Context(), r.Method, body, r.Header, proxy.Target{
		URL:      route.Upstream,
		AuthMode: channelAuthMode(ch),
		Key:      key,
		Protocol: route.Usage,
	})
	if err != nil {
		rec.Status = http.StatusInternalServerError
		rec.Error = err.Error()
		http.Error(w, "build upstream", http.StatusInternalServerError)
		return
	}

	resp, err := g.Client.Do(upReq)
	if err != nil {
		rec.Status = http.StatusBadGateway
		rec.Error = err.Error()
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	g.relayUpstream(w, resp, m, route, body, &rec)
}

// relayUpstream 把上游响应转发给客户端并填充计量字段（不关闭 resp.Body，调用方管理）。
// body 是发给上游的最终请求体（流式中断时 input 估算的输入）。
func (g *Gateway) relayUpstream(w http.ResponseWriter, resp *http.Response, m *config.Model, route *config.Route, body []byte, rec *store.UsageLog) {
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
	if proxy.IsStream(resp.Header) {
		acc := g.Meter.NewStreamAccumulator(route.Usage)
		proxy.RelayStream(w, resp.Body, func(d []byte) {
			acc.Feed(d)
			if rec.RequestID == "" {
				rec.RequestID = extractRequestID(d)
			}
		})
		usage := acc.Usage()
		if usage.InputTokens == 0 && usage.OutputTokens == 0 {
			usage.InputTokens = estInput // 流式没拿到 usage（如 chat_completions），退估算
		}
		fillMetering(rec, m, usage)
		return
	}

	// 非流式
	respBody, err := io.ReadAll(resp.Body)
	if err == nil {
		w.Write(respBody)
	}
	rec.RequestID = extractRequestID(respBody)
	fillMetering(rec, m, g.Meter.MeterNonStream(route.Usage, respBody))
}

// recordUsage 落库用量 + 累加额度（旁路，失败只 log 不阻断）。
func (g *Gateway) recordUsage(rec store.UsageLog, authKey *keys.Key) {
	log.Printf("usage model=%s proto=%s status=%d in=%d out=%d cost=$%.6f attempts=%d err=%s",
		rec.Model, rec.Protocol, rec.Status, rec.InputTokens, rec.OutputTokens, rec.Cost, rec.Attempts, rec.Error)
	if g.DB == nil {
		return
	}
	if err := g.DB.InsertUsageLog(rec); err != nil {
		log.Printf("insert usage log: %v", err)
	}
	if authKey != nil && rec.Cost > 0 {
		if err := g.DB.AddQuotaUsed(authKey.KeyHash, rec.Cost); err != nil {
			log.Printf("add quota: %v", err)
		}
	}
}

// fillMetering 填充用量记录的成本字段。
func fillMetering(rec *store.UsageLog, m *config.Model, usage metering.Usage) {
	rec.InputTokens = usage.InputTokens
	rec.OutputTokens = usage.OutputTokens
	rec.CacheReadTokens = usage.CacheReadTokens
	rec.CacheWriteTokens = usage.CacheWriteTokens
	rec.Cost = metering.Cost(usage, metering.Pricing{
		InputPerM:      m.Pricing.InputPerM,
		OutputPerM:     m.Pricing.OutputPerM,
		CacheReadPerM:  m.Pricing.CacheReadPerM,
		CacheWritePerM: m.Pricing.CacheWritePerM,
	})
	rec.Unmetered = usage.InputTokens == 0 && usage.OutputTokens == 0
}

// channelAuthMode 渠道可为 nil（config.yaml 只读路径），此时按默认 bearer。
func channelAuthMode(ch *config.Channel) string {
	if ch == nil {
		return ""
	}
	return ch.AuthMode
}

// extractModel 从请求 body 提取 model 字段。
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
		ID      string `json:"id"`
		Message struct {
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

// parseTimeRange 解析查询参数里的时间范围。
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
