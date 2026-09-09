package gateway

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"gateway/internal/store"
)

// 全文请求/响应日志（观测旁路）：默认关闭；开启时响应结束后异步落库，
// 永不阻塞转发链路（计量旁路同款原则）。单体截断上限内累积。

const logBodyMaxBytes = 2 << 20 // 单体捕获上限 2MB，超出标记 truncated

// bodySink 响应累积器（流式/非流式共用，带截断）。
type bodySink struct {
	enabled   bool
	buf       []byte
	truncated bool
}

func (b *bodySink) write(p []byte) {
	if !b.enabled || b.truncated {
		return
	}
	if len(b.buf)+len(p) > logBodyMaxBytes {
		b.truncated = true
		return
	}
	b.buf = append(b.buf, p...)
}

// handleGetLoggingSettings GET /api/settings/logging
func (g *Gateway) handleGetLoggingSettings(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	writeJSON(w, map[string]any{
		"enabled":        g.captureEnabled(),
		"retention_days": g.logRetention.Load(),
		"max_bytes":      logBodyMaxBytes,
	})
}

// handleUpdateLoggingSettings PUT /api/settings/logging {enabled, retention_days}
func (g *Gateway) handleUpdateLoggingSettings(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	if g.DB == nil {
		http.Error(w, "内存模式设置不持久化，重启失效；仅 PG 模式支持", http.StatusBadRequest)
		return
	}
	var req struct {
		Enabled       *bool  `json:"enabled"`
		RetentionDays *int64 `json:"retention_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Enabled != nil {
		g.logCapture.Store(*req.Enabled)
		_ = g.DB.SetSetting("log_capture_enabled", strconv.FormatBool(*req.Enabled))
	}
	if req.RetentionDays != nil && *req.RetentionDays > 0 && *req.RetentionDays <= 365 {
		g.logRetention.Store(*req.RetentionDays)
		_ = g.DB.SetSetting("log_capture_retention_days", strconv.FormatInt(*req.RetentionDays, 10))
	}
	writeJSON(w, map[string]any{
		"enabled":        g.captureEnabled(),
		"retention_days": g.logRetention.Load(),
	})
}

// handleGetLogBody GET /api/logs/body?request_id=xxx —— 取该请求的全文。
func (g *Gateway) handleGetLogBody(w http.ResponseWriter, r *http.Request) {
	if !g.requireAuth(w, r) {
		return
	}
	rid := r.URL.Query().Get("request_id")
	if rid == "" {
		http.Error(w, "request_id required", http.StatusBadRequest)
		return
	}
	if g.DB == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	b, err := g.DB.GetLogBodyByRequestID(rid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if b == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"request_id":    b.UsageRequestID,
		"model":         b.Model,
		"request_body":  string(b.RequestBody),
		"response_body": string(b.ResponseBody),
		"truncated":     b.Truncated,
		"status":        b.Status,
		"created_at":    b.CreatedAt,
	})
}

// storeRequestBodyAsync 响应结束后异步落库（失败只 log）。
func (g *Gateway) storeRequestBodyAsync(usageRequestID, model string, reqBody []byte, sink *bodySink, status int) {
	if !sink.enabled || len(reqBody) == 0 {
		return
	}
	b := &store.LogBody{
		UsageRequestID: usageRequestID,
		Model:          model,
		RequestBody:    reqBody,
		ResponseBody:   sink.buf,
		Truncated:      sink.truncated,
		Status:         status,
	}
	go func() {
		if err := g.DB.InsertLogBody(b); err != nil {
			log.Printf("insert log body: %v", err)
		}
	}()
}
