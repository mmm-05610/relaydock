package metering

import "unicode/utf8"

// EstimateInputTokens 字符估算（兜底流式中断等拿不到 usage 的场景）。
// 中文约 1 字 ≈ 0.6-1 token，用字符数/3 粗略估算，精度要求低。
func EstimateInputTokens(text string) int64 {
	return int64(utf8.RuneCountInString(text) / 3)
}
