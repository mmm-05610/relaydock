package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 是 gateway 的静态配置（渠道 = 上游供应商 → 模型路由 + 价格）。
type Config struct {
	Channels []Channel `yaml:"channels"`
	// OAuthProfiles OAuth 订阅账号的授权参数（每个 profile 一种订阅类型，
	// 如 codex = ChatGPT 订阅账号）。凭据生命周期见 gateway/internal/gateway/oauth.go。
	OAuthProfiles map[string]OAuthProfile `yaml:"oauth_profiles" json:"oauth_profiles"`
}

// OAuthProfile 一种 OAuth 订阅凭据的授权与使用参数（数据来自对应官方 CLI 的公开常量）。
type OAuthProfile struct {
	AuthorizeURL string `yaml:"authorize_url" json:"authorize_url"`
	TokenURL     string `yaml:"token_url" json:"token_url"`
	ClientID     string `yaml:"client_id" json:"client_id"`
	Scopes       string `yaml:"scopes" json:"scopes"`
	RedirectURI  string `yaml:"redirect_uri" json:"redirect_uri"`
	// UpstreamAuthStyle 数据面用该凭据请求上游的认证 header 样式：
	// bearer(默认, 仅 Authorization) | chatgpt_codex(Bearer + chatgpt-account-id + OpenAI-Beta)
	UpstreamAuthStyle string `yaml:"upstream_auth_style" json:"upstream_auth_style"`
}

// Channel 一个上游供应商渠道，含余额查询配置 + 支持的模型。
// ID 由 PG 生成（LoadChannels 回填），config.yaml 种子路径为 0。
type Channel struct {
	ID          int64   `yaml:"-" json:"id"`
	Provider    string  `yaml:"provider" json:"provider"`
	Name        string  `yaml:"name" json:"name"`
	BalanceType string  `yaml:"balance_type" json:"balance_type"` // balance(余额) | quota(余量百分比)
	BalanceURL  string  `yaml:"balance_url" json:"balance_url"`
	ModelsURL   string  `yaml:"models_url" json:"models_url"` // 拉取模型列表的接口
	Enabled     bool    `yaml:"enabled" json:"enabled"`
	AuthMode    string  `yaml:"auth_mode" json:"auth_mode"` // 上游认证 header：bearer(默认) | x_api_key
	Preset      Preset  `yaml:"preset" json:"preset"`       // 协议端点 + 价格预设（面板可视化）
	Models      []Model `yaml:"models" json:"models"`
}

// Model 一个客户端可用的模型，含多协议路由。
type Model struct {
	Name     string           `yaml:"name" json:"name"`
	Provider string           `yaml:"provider" json:"provider"` // 继承自渠道
	Routes   map[string]Route `yaml:"routes" json:"routes"`
	Pricing  Pricing          `yaml:"pricing" json:"pricing"`
	Enabled  bool             `yaml:"enabled" json:"enabled"`
	// ContextLength 上下文长度（可选）：/v1/models 元数据暴露，客户端据此裁剪上下文
	ContextLength int `yaml:"context_length" json:"context_length,omitempty"`
}

// Route 一条上游转发规则。Upstream 存完整 URL（不拼接，因各供应商路径结构不同）。
type Route struct {
	Upstream string `yaml:"upstream" json:"upstream"`
	Model    string `yaml:"model" json:"model"` // 上游 model 名
	Usage    string `yaml:"usage" json:"usage"` // 计量提取器：anthropic | responses | chat_completions
}

// Pricing 每模型单价（元 / 1M token）。
type Pricing struct {
	InputPerM      float64 `yaml:"input_per_m" json:"input_per_m"`
	OutputPerM     float64 `yaml:"output_per_m" json:"output_per_m"`
	CacheReadPerM  float64 `yaml:"cache_read_per_m" json:"cache_read_per_m"`   // 缓存命中单价
	CacheWritePerM float64 `yaml:"cache_write_per_m" json:"cache_write_per_m"` // 缓存写入单价
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	// 模型 provider 继承自渠道
	for i := range cfg.Channels {
		for j := range cfg.Channels[i].Models {
			cfg.Channels[i].Models[j].Provider = cfg.Channels[i].Provider
		}
	}
	return &cfg, nil
}

// FindModel 按客户端 model 名查配置，只认启用的渠道 + 启用的模型。
// 同名模型允许存在多个渠道，但客户端请求只路由到「启用渠道中第一个匹配」的那个——
// 即通过启停渠道来控制「花哪家的钱」，客户端无感知。
func (c *Config) FindModel(name string) *Model {
	for i := range c.Channels {
		if !c.Channels[i].Enabled {
			continue
		}
		for j := range c.Channels[i].Models {
			if c.Channels[i].Models[j].Enabled && c.Channels[i].Models[j].Name == name {
				return &c.Channels[i].Models[j]
			}
		}
	}
	return nil
}

// FindChannel 按 provider 查渠道。
func (c *Config) FindChannel(provider string) *Channel {
	for i := range c.Channels {
		if c.Channels[i].Provider == provider {
			return &c.Channels[i]
		}
	}
	return nil
}

// FindRoute 按客户端请求 path 匹配上游路由。
func (m *Model) FindRoute(clientPath string) (*Route, bool) {
	r, ok := m.Routes[clientPath]
	if !ok {
		return nil, false
	}
	return &r, true
}
