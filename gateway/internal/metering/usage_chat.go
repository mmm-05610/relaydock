package metering

import "encoding/json"

// ChatCompletionsExtractor 提取 chat completions 协议 usage。
// 字段语义：prompt_tokens 是总输入，缓存藏 prompt_tokens_details.cached_tokens。
// 流式默认不带 usage（需 stream_options.include_usage），故流式不识别（靠 fallback）。
type ChatCompletionsExtractor struct{}

type chatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (ChatCompletionsExtractor) ExtractNonStream(body []byte) Usage {
	var resp struct {
		Usage chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}
	}
	input := resp.Usage.PromptTokens - resp.Usage.PromptTokensDetails.CachedTokens
	if input < 0 {
		input = 0
	}
	return Usage{
		InputTokens:     input,
		OutputTokens:    resp.Usage.CompletionTokens,
		CacheReadTokens: resp.Usage.PromptTokensDetails.CachedTokens,
	}
}

// 流式：chat completions 默认不带 usage，返回 false（不识别）。
func (ChatCompletionsExtractor) ExtractStreamEvent([]byte) (Usage, bool) {
	return Usage{}, false
}
