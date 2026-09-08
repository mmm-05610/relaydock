package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// Target 一次上游尝试的完整目标。数据面透传与管理面渠道测试共用，
// 保证「测通 = 能用」。
type Target struct {
	URL      string // 完整上游 URL（配置里存完整 URL，不拼接）
	AuthMode string // bearer(默认) | x_api_key
	Key      string // 上游凭据
	Protocol string // anthropic | responses | chat_completions（决定协议 header）
	// ExtraHeaders OAuth 等凭据形态需要的额外上游 header（如 chatgpt-account-id）
	ExtraHeaders map[string]string
}

// BuildRequest 构造上游请求。除认证 / 协议 header 外 body 原样（纯透传）：
// 认证按渠道 auth_mode；anthropic-version 客户端带则透传、缺失补默认
// （opencode.ai 实测缺失会 401）。
func BuildRequest(ctx context.Context, method string, body []byte, clientHeader http.Header, t Target) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if t.AuthMode == "x_api_key" {
		req.Header.Set("x-api-key", t.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+t.Key)
	}
	if t.Protocol == "anthropic" {
		if v := clientHeader.Get("anthropic-version"); v != "" {
			req.Header.Set("anthropic-version", v)
		} else {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}
	for k, v := range t.ExtraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// ReplaceModel 把 body.model 替换为上游模型名；解析失败时原样返回
// （上游自己报错，不在网关层拦截）。
func ReplaceModel(body []byte, newModel string) []byte {
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
