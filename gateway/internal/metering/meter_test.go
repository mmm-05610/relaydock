package metering

import "testing"

// 以下 JSON 样例均为 curl 实测的真实响应，验证 extractor 字段语义。

func TestAnthropicNonStream(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":87,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":30}}`)
	u := AnthropicExtractor{}.ExtractNonStream(body)
	if u.InputTokens != 87 || u.OutputTokens != 30 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Fatalf("got %+v", u)
	}
}

func TestAnthropicCache(t *testing.T) {
	// MiniMax anthropic 实测（有缓存命中 142）
	body := []byte(`{"usage":{"input_tokens":25,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":142}}`)
	u := AnthropicExtractor{}.ExtractNonStream(body)
	if u.InputTokens != 25 || u.CacheReadTokens != 142 || u.OutputTokens != 1 {
		t.Fatalf("got %+v", u)
	}
}

func TestAnthropicStreamEvents(t *testing.T) {
	m := NewMeter()
	acc := m.NewStreamAccumulator("anthropic")
	acc.Feed([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":87,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`))
	acc.Feed([]byte(`{"type":"message_delta","usage":{"output_tokens":30}}`))
	u := acc.Usage()
	if u.InputTokens != 87 || u.OutputTokens != 30 {
		t.Fatalf("got %+v", u)
	}
}

func TestResponsesNonStream(t *testing.T) {
	// MiniMax responses 实测：input_tokens=167 总输入，cached=128
	body := []byte(`{"usage":{"input_tokens":167,"output_tokens":2,"total_tokens":169,"input_tokens_details":{"cached_tokens":128}}}`)
	u := ResponsesExtractor{}.ExtractNonStream(body)
	if u.InputTokens != 39 || u.CacheReadTokens != 128 || u.OutputTokens != 2 {
		t.Fatalf("got %+v", u)
	}
}

func TestResponsesStreamIncomplete(t *testing.T) {
	// 实测截断时终止事件是 response.incomplete
	m := NewMeter()
	acc := m.NewStreamAccumulator("responses")
	acc.Feed([]byte(`{"type":"response.incomplete","response":{"usage":{"input_tokens":87,"input_tokens_details":{"cached_tokens":0},"output_tokens":30}}}`))
	u := acc.Usage()
	if u.InputTokens != 87 || u.OutputTokens != 30 {
		t.Fatalf("got %+v", u)
	}
}

func TestChatCompletionsNonStream(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":40}}}`)
	u := ChatCompletionsExtractor{}.ExtractNonStream(body)
	if u.InputTokens != 60 || u.OutputTokens != 50 || u.CacheReadTokens != 40 {
		t.Fatalf("got %+v", u)
	}
}

func TestCost(t *testing.T) {
	p := Pricing{InputPerM: 3, OutputPerM: 6, CacheReadPerM: 0.025, CacheWritePerM: 3}
	u := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000}
	// 3 + 6 + 0.025 + 3
	want := 3 + 6 + 0.025 + 3.0
	got := Cost(u, p)
	if got < want-0.0001 || got > want+0.0001 {
		t.Fatalf("cost got %f want %f", got, want)
	}
}

// TestCostChargesCacheSeparately 缓存读/写使用各自独立单价；
// input/output 为零时不贡献成本（缓存不是输入价的固定倍数）。
func TestCostChargesCacheSeparately(t *testing.T) {
	p := Pricing{CacheReadPerM: 0.025, CacheWritePerM: 3}
	u := Usage{CacheReadTokens: 200_000, CacheWriteTokens: 100_000}
	want := 200_000/1e6*0.025 + 100_000/1e6*3 // 0.305
	got := Cost(u, p)
	if got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("cost got %f want %f", got, want)
	}
}

// TestResponsesStreamCompleted 正常结束的终止事件是 response.completed，
// usage 语义与 incomplete 一致：input_tokens 是总输入（含缓存），
// 纯输入 = 总输入 - 缓存命中，二者不重复计费。
func TestResponsesStreamCompleted(t *testing.T) {
	m := NewMeter()
	acc := m.NewStreamAccumulator("responses")
	acc.Feed([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":167,"input_tokens_details":{"cached_tokens":128},"output_tokens":30}}}`))
	u := acc.Usage()
	if u.InputTokens != 39 || u.CacheReadTokens != 128 || u.OutputTokens != 30 {
		t.Fatalf("got %+v, want input=39 cache_read=128 output=30", u)
	}
}

// TestAnthropicStreamCacheWrite anthropic 顶层 cache 字段语义：message_start
// 的 message.usage 带 cache_creation/cache_read（顶层独立字段，input_tokens
// 不含缓存），message_delta 带 output。归一化后四类 token 各自独立。
func TestAnthropicStreamCacheWrite(t *testing.T) {
	m := NewMeter()
	acc := m.NewStreamAccumulator("anthropic")
	acc.Feed([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":500,"cache_creation_input_tokens":1000,"cache_read_input_tokens":200,"output_tokens":0}}}`))
	acc.Feed([]byte(`{"type":"message_delta","usage":{"output_tokens":30}}`))
	u := acc.Usage()
	if u.InputTokens != 500 || u.CacheWriteTokens != 1000 || u.CacheReadTokens != 200 || u.OutputTokens != 30 {
		t.Fatalf("got %+v", u)
	}
}

// TestStreamAccumulatorLaterEventOverrides 同一字段在后续事件中非零时覆盖而非累加
// （anthropic 的最后一条 message_delta 是最终 output 值），零值不覆盖已有值。
func TestStreamAccumulatorLaterEventOverrides(t *testing.T) {
	m := NewMeter()
	acc := m.NewStreamAccumulator("anthropic")
	acc.Feed([]byte(`{"type":"message_delta","usage":{"output_tokens":10}}`))
	acc.Feed([]byte(`{"type":"message_delta","usage":{"output_tokens":30}}`))
	if got := acc.Usage().OutputTokens; got != 30 {
		t.Fatalf("output = %d, want 30 (last event wins)", got)
	}
	acc.Feed([]byte(`{"type":"message_delta"}`)) // usage 缺失 = 全零，不应清零
	if got := acc.Usage().OutputTokens; got != 30 {
		t.Fatalf("output = %d, want 30 (zero must not override)", got)
	}
}

// TestChatCompletionsStreamNotRecognized 锁定现状：chat_completions 流式默认不带
// usage（需 stream_options.include_usage），当前 extractor 不识别任何流式事件 →
// accumulator 保持零 → 调用方（handleProxy）退回估算。垃圾输入也不 panic。
func TestChatCompletionsStreamNotRecognized(t *testing.T) {
	m := NewMeter()
	acc := m.NewStreamAccumulator("chat_completions")
	acc.Feed([]byte(`{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`))
	acc.Feed([]byte(`not-json-garbage`))
	if u := acc.Usage(); u != (Usage{}) {
		t.Fatalf("got %+v, want zero usage (chat stream events not recognized today)", u)
	}
}

// TestMeterNonStreamUnparseableInput 无 usage / 非法 JSON / 未知协议：
// 不 panic，返回零 Usage（调用方据此退回估算并标记 unmetered）。
func TestMeterNonStreamUnparseableInput(t *testing.T) {
	m := NewMeter()
	cases := []struct {
		name  string
		proto string
		body  []byte
	}{
		{"empty body", "anthropic", nil},
		{"garbage json", "anthropic", []byte("not-json")},
		{"no usage field", "anthropic", []byte(`{"id":"x","content":[]}`)},
		{"garbage json", "responses", []byte("}{")},
		{"no usage field", "responses", []byte(`{"id":"resp_1","output":[]}`)},
		{"garbage json", "chat_completions", []byte("[")},
		{"no usage field", "chat_completions", []byte(`{"choices":[]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name+"_"+tc.proto, func(t *testing.T) {
			got := m.MeterNonStream(tc.proto, tc.body)
			if got != (Usage{}) {
				t.Fatalf("got %+v, want zero usage", got)
			}
		})
	}
	t.Run("unknown protocol", func(t *testing.T) {
		got := m.MeterNonStream("nope", []byte(`{"usage":{"input_tokens":5}}`))
		if got != (Usage{}) {
			t.Fatalf("got %+v, want zero usage", got)
		}
	})
}

// TestEstimateInputTokens 字符/3 估算（流式拿不到 usage 时的兜底）。
func TestEstimateInputTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"ab", 0},     // 不足 3 字符向下取整
		{"abcdef", 2}, // 6 runes / 3
		{"你好世界", 1},   // 4 runes / 3 → 1
	}
	for _, tc := range cases {
		if got := EstimateInputTokens(tc.in); got != tc.want {
			t.Fatalf("EstimateInputTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
