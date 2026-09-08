package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
)

// IsStream 按响应 Content-Type 判定 SSE 流。
func IsStream(h http.Header) bool {
	return bytes.Contains([]byte(h.Get("Content-Type")), []byte("event-stream"))
}

// RelayStream 逐行转发 SSE：原样写出每行 + 换行并 Flush；
// data: JSON 载荷旁路喂给 feed（计量监听，失败只影响计量不影响转发）。
// 客户端写失败（已断开）时立即停止读取上游——配合上游请求绑定客户端
// context，断开会同时取消上游请求。
func RelayStream(w io.Writer, body io.Reader, feed func(data []byte)) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 允许 1MB 长行
	for scanner.Scan() {
		line := scanner.Bytes()
		if _, err := w.Write(line); err != nil {
			return
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		if feed != nil {
			if d, ok := SSEData(line); ok {
				feed(d)
			}
		}
	}
}

// SSEData 剥离 SSE 的 "data:" 前缀，返回 JSON 载荷；[DONE] 与空行不算载荷。
func SSEData(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil, false
	}
	d := bytes.TrimSpace(line[len("data:"):])
	if len(d) == 0 || string(d) == "[DONE]" {
		return nil, false
	}
	return d, true
}
