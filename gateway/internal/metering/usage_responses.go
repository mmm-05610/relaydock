package metering

import "encoding/json"

// ResponsesExtractor 提取 responses 协议 usage。
// 字段语义（实测）：input_tokens 是总输入（含缓存），缓存藏 input_tokens_details.cached_tokens。
type ResponsesExtractor struct{}

type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (ResponsesExtractor) ExtractNonStream(body []byte) Usage {
	var resp struct {
		Usage responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}
	}
	return normalizeResponses(resp.Usage)
}

func (ResponsesExtractor) ExtractStreamEvent(data []byte) (Usage, bool) {
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			Usage responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return Usage{}, false
	}
	// 实测：usage 在最后一个事件（completed 正常 / incomplete 截断）的 response.usage
	switch ev.Type {
	case "response.completed", "response.incomplete":
		return normalizeResponses(ev.Response.Usage), true
	}
	return Usage{}, false
}

func normalizeResponses(u responsesUsage) Usage {
	// 纯输入 = 总输入 - 缓存命中
	input := u.InputTokens - u.InputTokensDetails.CachedTokens
	if input < 0 {
		input = 0
	}
	return Usage{
		InputTokens:     input,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.InputTokensDetails.CachedTokens,
	}
}
