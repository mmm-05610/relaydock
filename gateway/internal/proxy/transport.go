package proxy

import (
	"net"
	"net/http"
	"time"
)

// NewUpstreamClient 数据面上游客户端：显式配置 Transport，替代 http.DefaultClient。
//
//   - 不设整体 Client.Timeout：SSE 是长流，整体超时会截断正常长响应，
//     由 Transport 的分阶段超时兜底；
//   - Proxy 显式置 nil：网关直连上游，不读环境变量代理设置
//     （DefaultClient/DefaultTransport 会读 *_proxy，测试与生产都可能被劫持）；
//   - ResponseHeaderTimeout 给足 10 分钟：非流式请求在上游生成完整个响应
//     之前不返回响应头，推理模型长思考可能耗时数分钟。
func NewUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Minute,
		},
	}
}

// NewAdminClient 管理面出站调用（渠道测试 / 余额 / 远端模型列表）：
// 复用上游连接池配置，加整体超时——管理调用都是短请求。
func NewAdminClient() *http.Client {
	return &http.Client{
		Transport: NewUpstreamClient().Transport,
		Timeout:   15 * time.Second,
	}
}
