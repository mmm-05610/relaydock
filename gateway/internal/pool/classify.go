package pool

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// 错误类别（保守策略，docs/design-upstream-account-pool.md §7）。
// 核心边界：网络错误/超时/5xx 的执行结果未知（上游可能已执行并计费），
// 默认不换账号重试；只有明确的账号级错误才切换。
type Class int

const (
	ClassOK          Class = iota // 2xx/3xx：正常转发
	ClassRateLimited              // 429：冷却账号 + 换账号（响应未提交）
	ClassCredential               // 401/403：凭据问题，较长冷却 + 换账号
	ClassQuota                    // 402：配额耗尽，较长冷却 + 换账号
	ClassClientError              // 400/404/413 等请求问题：透传，不惩罚账号
	ClassServerError              // 5xx：透传，默认不换（结果未知）
)

// Class Class 的稳定字符串名（日志、管理面展示用）。
func (c Class) String() string {
	switch c {
	case ClassOK:
		return "ok"
	case ClassRateLimited:
		return "rate_limited"
	case ClassCredential:
		return "credential"
	case ClassQuota:
		return "quota"
	case ClassClientError:
		return "client_error"
	case ClassServerError:
		return "server_error"
	default:
		return "unknown"
	}
}

// Verdict 一次上游响应的处置决定。
type Verdict struct {
	Class    Class
	Cooldown time.Duration // >0 时对账号施加冷却（已含上限与 jitter）
}

// 冷却时长策略（第一版固定值；异常 Retry-After 被上限钳制，
// 不能让账号永久消失）。
const (
	Default429Cooldown = 30 * time.Second
	CredentialCooldown = 10 * time.Minute
	QuotaCooldown      = 30 * time.Minute
	MaxCooldown        = 10 * time.Minute
)

// ClassifyResponse 按状态码保守归类。Provider 特定的错误文本判定
// （如某些供应商把限流写成 400 + 文本）留作 ProviderClassifier 扩展点。
func ClassifyResponse(status int, header http.Header) Verdict {
	switch {
	case status == http.StatusTooManyRequests:
		d := ParseRetryAfter(header)
		if d <= 0 {
			d = Default429Cooldown
		}
		return Verdict{ClassRateLimited, jitter(d)}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Verdict{ClassCredential, jitter(CredentialCooldown)}
	case status == http.StatusPaymentRequired:
		return Verdict{ClassQuota, jitter(QuotaCooldown)}
	case status >= 200 && status < 400:
		return Verdict{ClassOK, 0}
	case status >= 500:
		return Verdict{ClassServerError, 0}
	default:
		return Verdict{ClassClientError, 0}
	}
}

// ParseRetryAfter 解析标准 Retry-After（秒数或 HTTP 日期），无效返回 0。
func ParseRetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// jitter 加 ±10% 抖动并钳制上限，避免多账号同时恢复造成瞬时拥塞。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * 0.1
	d += time.Duration(rand.Float64()*2*spread - spread)
	if d > MaxCooldown {
		d = MaxCooldown
	}
	return d
}
