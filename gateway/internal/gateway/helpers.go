package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// bearerToken 提取 Authorization: Bearer <token>。
func bearerToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// writeJSON 以 application/json 写响应。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
