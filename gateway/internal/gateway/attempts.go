package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gateway/internal/config"
	"gateway/internal/pool"
	"gateway/internal/proxy"
	"gateway/internal/routing"
	"gateway/internal/store"
)

// proxyWithAccounts 显式账号池路径。
//
// 边界（保守策略，docs/design-upstream-account-pool.md §7）：
//   - 只有明确的账号级错误（429/401/403/402）才在响应未提交时换账号重试；
//   - 400/404/413 等请求问题、5xx、网络错误的结果未知（上游可能已执行并计费），
//     一律原样转发、不惩罚账号、不换账号重试；
//   - 全部账号满槽 → 本地 429（带 Retry-After）；全部不可用 → 本地 503；
//   - 没有下一个账号可试时，透传最近一次上游错误响应（比本地 503 更真实）；
//   - 每次尝试从原始 body 重新构造请求（ReplaceModel 是纯函数）。
func (g *Gateway) proxyWithAccounts(w http.ResponseWriter, r *http.Request, snap *routing.Snapshot, m *config.Model, ch *config.Channel, route *config.Route, refs []*pool.AccountRef, originalBody []byte, clientModel string, rec *store.UsageLog) {
	tried := map[int64]bool{}
	attempts := 0
	sink := &bodySink{}
	if g.captureEnabled() {
		sink.enabled = true
	}
	sessionKey := sessionKeyOf(originalBody, r.Header)
	var prefer *pool.AccountRef
	if sessionKey != "" {
		if a := g.Pool.Affinity(sessionKey, time.Now()); a != nil {
			for _, r := range refs {
				if r.Spec.ID == a.Spec.ID {
					prefer = r // 粘性目标已不在候选集（被删/禁用）时保持 nil，走常规选择
					break
				}
			}
		}
	}

	// 最近一个「可切换」错误响应，尚未转发给客户端
	var pending *http.Response
	var pendingRef *pool.AccountRef
	closePending := func() {
		if pending != nil {
			drainAndClose(pending)
			pending = nil
		}
	}
	defer closePending()

	for {
		lease, err := g.Pool.Acquire(refs, tried, prefer)
		if err != nil {
			if pending != nil {
				// 没有下一个账号：透传最近的上游错误响应
				ref := pendingRef
				pendingRef = nil
				resp := pending
				pending = nil // relayUpstream 负责读完，此处不再由 defer 关闭
				rec.AccountID = ref.Spec.ID
				g.relayUpstream(w, resp, m, route, upstreamBody(originalBody, route, clientModel), sink, rec)
				resp.Body.Close()
				if rec.RequestID != "" {
					g.storeRequestBodyAsync(rec.RequestID, rec.Model, originalBody, sink, rec.Status)
				}
				return
			}
			if errors.Is(err, pool.ErrAtCapacity) {
				writeGatewayError(w, rec, http.StatusTooManyRequests, "all upstream accounts at capacity", 5)
			} else {
				writeGatewayError(w, rec, http.StatusServiceUnavailable, "no available upstream account", 0)
			}
			return
		}
		attempts++
		rec.Attempts = attempts
		ref := lease.Account()

		cred, extraHeaders := g.credentialFor(snap, ref)
		upReq, err := proxy.BuildRequest(r.Context(), r.Method,
			upstreamBody(originalBody, route, clientModel), r.Header, proxy.Target{
				URL:          route.Upstream,
				AuthMode:     ch.AuthMode,
				Key:          cred,
				Protocol:     route.Usage,
				ExtraHeaders: extraHeaders,
			})
		if err != nil {
			lease.Release()
			rec.Status = http.StatusInternalServerError
			rec.Error = err.Error()
			http.Error(w, "build upstream", http.StatusInternalServerError)
			return
		}
		resp, err := g.Client.Do(upReq)
		if err != nil {
			// 网络错误/超时：结果未知，不换账号
			ref.State.RecordResult(0, "transport", err.Error())
			lease.Release()
			rec.Status = http.StatusBadGateway
			rec.Error = err.Error()
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}

		v := pool.ClassifyResponse(resp.StatusCode, resp.Header)
		switch v.Class {
		case pool.ClassRateLimited, pool.ClassCredential, pool.ClassQuota:
			// 账号级错误：冷却 + 尝试下一个账号（客户端响应尚未提交）
			ref.State.RecordResult(resp.StatusCode, v.Class.String(), fmt.Sprintf("HTTP %d", resp.StatusCode))
			g.Pool.Cool(ref, v.Cooldown, fmt.Sprintf("upstream %d", resp.StatusCode))
			tried[ref.Spec.ID] = true
			closePending() // 上一个可切换响应被更近的取代
			pending = resp
			pendingRef = ref
			lease.Release()
			g.Pool.IncFailover()
			continue
		default:
			// 正常响应 / 不可安全重试的错误：原样转发，本请求结束
			if v.Class == pool.ClassOK {
				ref.State.RecordResult(resp.StatusCode, "", "")
			} else {
				ref.State.RecordResult(resp.StatusCode, v.Class.String(), fmt.Sprintf("HTTP %d", resp.StatusCode))
			}
			rec.AccountID = ref.Spec.ID
			g.relayUpstream(w, resp, m, route, upstreamBody(originalBody, route, clientModel), sink, rec)
			resp.Body.Close()
			lease.Release()
			if v.Class == pool.ClassOK {
				g.Pool.BindAffinity(sessionKey, ref, time.Now())
			} else {
				g.Pool.BindAffinity(sessionKey, nil, time.Now()) // 失败解除粘性
			}
			if rec.RequestID != "" {
				g.storeRequestBodyAsync(rec.RequestID, rec.Model, originalBody, sink, rec.Status)
			}
			return
		}
	}
}

// upstreamBody 从原始 body 生成发给上游的最终请求体（替换 model，纯函数可重复调用）。
func upstreamBody(originalBody []byte, route *config.Route, clientModel string) []byte {
	if route.Model == clientModel {
		return originalBody
	}
	return proxy.ReplaceModel(originalBody, route.Model)
}

// writeGatewayError 网关本地错误（不来自上游）：稳定的 JSON 错误格式。
func writeGatewayError(w http.ResponseWriter, rec *store.UsageLog, status int, msg string, retryAfter int) {
	rec.Status = status
	rec.Error = msg
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    map[int]string{http.StatusTooManyRequests: "gateway_rate_limited", http.StatusServiceUnavailable: "gateway_unavailable"}[status],
			"message": msg,
		},
	})
}

// drainAndClose 读取少量 body 后关闭，便于 HTTP 连接复用。
func drainAndClose(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, 8<<10)
	_ = resp.Body.Close()
}

// sessionKeyOf 会话粘性键：优先 body.prompt_cache_key（Codex），
// 其次 session-id 头；都没有则不做粘性（返回空）。
func sessionKeyOf(body []byte, header http.Header) string {
	var m struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(body, &m); err == nil && m.PromptCacheKey != "" {
		return "pc:" + m.PromptCacheKey
	}
	if sid := header.Get("session-id"); sid != "" {
		return "sid:" + sid
	}
	return ""
}
