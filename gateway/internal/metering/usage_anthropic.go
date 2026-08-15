package metering

import "encoding/json"

// AnthropicExtractor 提取 anthropic 协议 usage。
// 字段语义（实测）：input_tokens 是纯输入（不含缓存），缓存是顶层独立字段。
type AnthropicExtractor struct{}

type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (AnthropicExtractor) ExtractNonStream(body []byte) Usage {
	var resp struct {
		Usage anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}
	}
	return normalizeAnthropic(resp.Usage)
}

func (AnthropicExtractor) ExtractStreamEvent(data []byte) (Usage, bool) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return Usage{}, false
	}
	switch ev.Type {
	case "message_start": // input + cache
		return normalizeAnthropic(ev.Message.Usage), true
	case "message_delta": // output（最后一个）
		return normalizeAnthropic(ev.Usage), true
	}
	return Usage{}, false
}

func normalizeAnthropic(u anthropicUsage) Usage {
	return Usage{
		InputTokens:      u.InputTokens, // 已不含缓存
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}
