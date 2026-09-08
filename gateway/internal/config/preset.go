package config

// Preset 渠道预设（前端可视化配置，替代硬编码的 PROVIDER_BASE/PROVIDER_PRICING）。
// 存 PG channels.preset（JSONB）。为空时前端退化到硬编码字典（兼容旧部署）。
type Preset struct {
	Endpoints map[string]string     `json:"endpoints,omitempty"` // proto -> 上游端点 URL
	Pricing   map[string]PresetItem `json:"pricing,omitempty"`   // model name -> 价格
}

// PresetItem 单个模型的价格预设（元 / 1M token）。
type PresetItem struct {
	InputPerM      float64 `json:"input_per_m"`
	OutputPerM     float64 `json:"output_per_m"`
	CacheReadPerM  float64 `json:"cache_read_per_m"`
	CacheWritePerM float64 `json:"cache_write_per_m"`
}
