package metering

// Usage 归一化后的用量结构。成本计算只看这个，不碰协议。
type Usage struct {
	InputTokens      int64 // 纯输入（不含缓存）
	OutputTokens     int64
	CacheReadTokens  int64 // 缓存命中
	CacheWriteTokens int64 // 缓存创建
}

// Pricing 每模型单价（元 / 1M token）。
type Pricing struct {
	InputPerM      float64
	OutputPerM     float64
	CacheReadPerM  float64
	CacheWritePerM float64
}

// Cost 计算成本（含缓存计价）。
func Cost(u Usage, p Pricing) float64 {
	return float64(u.InputTokens)/1e6*p.InputPerM +
		float64(u.CacheReadTokens)/1e6*p.CacheReadPerM +
		float64(u.CacheWriteTokens)/1e6*p.CacheWritePerM +
		float64(u.OutputTokens)/1e6*p.OutputPerM
}

// UsageExtractor 把原始响应归一化成 Usage。每协议一个实现。
type UsageExtractor interface {
	// ExtractNonStream 从完整响应 body 提取 usage。
	ExtractNonStream(body []byte) Usage
	// ExtractStreamEvent 从单个 SSE data 行 JSON 提取部分 usage；第二个返回值表示是否命中 usage 事件。
	ExtractStreamEvent(data []byte) (Usage, bool)
}

// Meter 计量编排。
type Meter struct {
	extractors map[string]UsageExtractor
}

func NewMeter() *Meter {
	return &Meter{extractors: map[string]UsageExtractor{
		"anthropic":        AnthropicExtractor{},
		"responses":        ResponsesExtractor{},
		"chat_completions": ChatCompletionsExtractor{},
	}}
}

// MeterNonStream 非流式：完整响应 -> Usage。
func (m *Meter) MeterNonStream(usageProto string, body []byte) Usage {
	if ext, ok := m.extractors[usageProto]; ok {
		return ext.ExtractNonStream(body)
	}
	return Usage{}
}

// StreamAccumulator 流式：逐事件累积 usage。
type StreamAccumulator struct {
	ext UsageExtractor
	acc Usage
}

func (m *Meter) NewStreamAccumulator(usageProto string) *StreamAccumulator {
	ext, ok := m.extractors[usageProto]
	if !ok {
		ext = noopExtractor{}
	}
	return &StreamAccumulator{ext: ext}
}

// Feed 喂入一个 SSE data 行的 JSON，累积非零字段。
func (s *StreamAccumulator) Feed(data []byte) {
	u, ok := s.ext.ExtractStreamEvent(data)
	if !ok {
		return
	}
	if u.InputTokens > 0 {
		s.acc.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		s.acc.OutputTokens = u.OutputTokens
	}
	if u.CacheReadTokens > 0 {
		s.acc.CacheReadTokens = u.CacheReadTokens
	}
	if u.CacheWriteTokens > 0 {
		s.acc.CacheWriteTokens = u.CacheWriteTokens
	}
}

func (s *StreamAccumulator) Usage() Usage { return s.acc }

type noopExtractor struct{}

func (noopExtractor) ExtractNonStream([]byte) Usage           { return Usage{} }
func (noopExtractor) ExtractStreamEvent([]byte) (Usage, bool) { return Usage{}, false }
