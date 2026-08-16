package store

import (
	"time"

	"gateway/internal/config"
)

// UsageLog 一条用量记录（对应 usage_logs 表）。
type UsageLog struct {
	ID               int64
	KeyID            int64
	Model            string
	UpstreamModel    string
	Protocol         string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Cost             float64
	LatencyMs        int64
	Status           int
	Error            string
	RequestID        string
	Unmetered        bool
	CreatedAt        time.Time
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
	TotalCost     float64
	TotalTokens   int64
	TotalRequests int64
	ByModel       []ModelUsage
	ByKey         []KeyUsage
}

// ModelUsage 按模型聚合。
type ModelUsage struct {
	Model        string
	Cost         float64
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	Requests     int64
	SuccessRate  float64 // 0-1，成功(status<400)占比
}

// KeyUsage 按虚拟 key 聚合。
type KeyUsage struct {
	KeyID    int64
	Name     string
	Owner    string
	Cost     float64
	Tokens   int64
	Requests int64
}

// DashboardStats 概览聚合。
type DashboardStats struct {
	TodayCost       float64
	TodayTokens     int64
	TodayRequests   int64
	SuccessRate     float64
	AvgLatencyMs    float64
	CacheReadTokens int64
	CacheHitRate    float64 // 缓存命中率 = cache_read / (input + cache_read)
	RecentLogs      []UsageLog
}

// TimeseriesPoint 按天聚合点。
type TimeseriesPoint struct {
	Date     string
	Cost     float64
	Tokens   int64
	Requests int64
}

// GroupedUsage 按维度聚合。
type GroupedUsage struct {
	Group           string
	Cost            float64
	InputTokens     int64
	OutputTokens    int64
	Tokens          int64
	Requests        int64
	SuccessRate     float64
	CacheReadTokens int64
	AvgLatencyMs    float64
}
