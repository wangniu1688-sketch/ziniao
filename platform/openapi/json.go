package openapi

import (
	"encoding/json"
	"net/http"
)

// WriteJSON 写 JSON 响应（供 openapi 包与 gateway 复用，避免循环依赖）。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSON(w http.ResponseWriter, v any) error {
	return json.NewEncoder(w).Encode(v)
}
