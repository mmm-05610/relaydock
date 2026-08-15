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
