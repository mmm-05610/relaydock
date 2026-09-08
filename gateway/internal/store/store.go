package store

import (
	"time"

	"gateway/internal/config"
)

// UsageLog 一条用量记录（对应 usage_logs 表）。
type UsageLog struct {
	ID               int64     `json:"id"`
	KeyID            int64     `json:"key_id"`
	Model            string    `json:"model"`
	UpstreamModel    string    `json:"upstream_model"`
	Protocol         string    `json:"protocol"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	Cost             float64   `json:"cost"`
	LatencyMs        int64     `json:"latency_ms"`
	Status           int       `json:"status"`
	Error            string    `json:"error"`
	RequestID        string    `json:"request_id"`
	Unmetered        bool      `json:"unmetered"`
	ChannelID        int64     `json:"channel_id"` // 最终承载响应的渠道（0 = 内存模式未归因）
	AccountID        int64     `json:"account_id"` // 最终承载响应的上游账号（<=0 = 隐式账号，落库为 NULL）
	Attempts         int       `json:"attempts"`   // 上游尝试次数（含首次）
	CreatedAt        time.Time `json:"created_at"`
}

// UpstreamAccount 一份上游账号（渠道下的独立凭据 + 并发容量），对应 upstream_accounts 表。
type UpstreamAccount struct {
	ID             int64
	ChannelID      int64
	Name           string
	EncryptedKey   []byte
	KeyFingerprint string
	MaxConcurrency int64 // 0 = 不限
	Enabled        bool
}

// AccountStore 上游账号的增删改查（凭据密文由调用方加密）。
type AccountStore interface {
	ListUpstreamAccounts() ([]UpstreamAccount, error)
	CreateUpstreamAccount(a *UpstreamAccount) error // 回填 a.ID
	UpdateUpstreamAccount(a UpstreamAccount) error
	DeleteUpstreamAccount(id int64) error
	AccountUsageStats(days int) ([]AccountUsage, error) // 按账号聚合最近 N 天用量（PG）
}

// AccountUsage 账号级用量聚合（usage_logs 按 account_id 分组）。
type AccountUsage struct {
	AccountID   int64   `json:"account_id"`
	Requests    int64   `json:"requests"`
	Cost        float64 `json:"cost"`
	Tokens      int64   `json:"tokens"`
	AvgAttempts float64 `json:"avg_attempts"`
	Errors      int64   `json:"errors"`
}

// UpstreamStore 上游 key 的加密存取。
type UpstreamStore interface {
	SetUpstreamKey(provider string, encrypted []byte) error
	GetUpstreamKey(provider string) ([]byte, error)
}

// ChannelStore 渠道 + 模型的增删改查。
type ChannelStore interface {
	LoadChannels() ([]config.Channel, error)
	CreateChannel(ch config.Channel) error
	UpdateChannel(ch config.Channel) error
	DeleteChannel(provider string) error
	CreateModel(provider string, m config.Model) error
	UpdateModel(provider string, m config.Model) error
	DeleteModel(provider, modelName string) error
}

// UsageStore 用量记录 + 聚合查询。
type UsageStore interface {
	InsertUsageLog(log UsageLog) error
	GetUsageStats() (UsageStats, error)
	GetDashboard() (DashboardStats, error)
	GetTimeseries(r TimeRange) ([]TimeseriesPoint, error)
	GetGrouped(by string, r TimeRange) ([]GroupedUsage, error)
	QueryLogs(filter LogFilter) ([]UsageLog, error)
}

// TimeRange 用量查询的时间范围（三种方式取一）。
type TimeRange struct {
	Days int    // >0：最近 N 天（相对 PG current_date，含今天）
	From string // 自定义起止（YYYY-MM-DD，含），与 To 配对
	To   string // 自定义结束（YYYY-MM-DD，含）
}

// LogFilter 日志查询筛选。
type LogFilter struct {
	KeyID  int64
	Model  string
	Status int // 0 = 不限
	Limit  int
	Offset int
}

// UsageStats 用量统计（汇总 + 按模型 + 按 key）。
type UsageStats struct {
	TotalCost     float64      `json:"total_cost"`
	TotalTokens   int64        `json:"total_tokens"`
	TotalRequests int64        `json:"total_requests"`
	ByModel       []ModelUsage `json:"by_model"`
	ByKey         []KeyUsage   `json:"by_key"`
}

// ModelUsage 按模型聚合。
type ModelUsage struct {
	Model        string  `json:"model"`
	Cost         float64 `json:"cost"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	Requests     int64   `json:"requests"`
	SuccessRate  float64 `json:"success_rate"` // 0-1，成功(status<400)占比
}

// KeyUsage 按虚拟 key 聚合。
type KeyUsage struct {
	KeyID    int64   `json:"key_id"`
	Name     string  `json:"name"`
	Owner    string  `json:"owner"`
	Cost     float64 `json:"cost"`
	Tokens   int64   `json:"tokens"`
	Requests int64   `json:"requests"`
}

// DashboardStats 概览聚合。
type DashboardStats struct {
	TodayCost       float64    `json:"today_cost"`
	TodayTokens     int64      `json:"today_tokens"`
	TodayRequests   int64      `json:"today_requests"`
	SuccessRate     float64    `json:"success_rate"`
	AvgLatencyMs    float64    `json:"avg_latency_ms"`
	CacheReadTokens int64      `json:"cache_read_tokens"`
	CacheHitRate    float64    `json:"cache_hit_rate"` // 缓存命中率 = cache_read / (input + cache_read)
	RecentLogs      []UsageLog `json:"recent_logs"`
}

// TimeseriesPoint 按天聚合点。
type TimeseriesPoint struct {
	Date     string  `json:"date"`
	Cost     float64 `json:"cost"`
	Tokens   int64   `json:"tokens"`
	Requests int64   `json:"requests"`
}

// GroupedUsage 按维度聚合。
type GroupedUsage struct {
	Group           string  `json:"group"`
	Cost            float64 `json:"cost"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	Tokens          int64   `json:"tokens"`
	Requests        int64   `json:"requests"`
	SuccessRate     float64 `json:"success_rate"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	AvgLatencyMs    float64 `json:"avg_latency_ms"`
}
