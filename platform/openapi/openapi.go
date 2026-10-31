// Package openapi 轻量 OpenAPI 3.0 注册中心：路由声明即文档，服务启动后
// 在 /openapi.json 暴露（交付物规范要求）。
package openapi

import (
	"net/http"
	"sort"
	"sync"
)

// Op 一个操作的文档描述。
type Op struct {
	Method     string // GET/POST/PUT/DELETE
	Path       string // /api/v1/shops
	Tag        string // 模块分组
	Summary    string
	Permission string // 所需权限标识（permission key），空表示公开
	Idempotent bool
	Request    string // 请求体示例类型名
	Response   string
}

var (
	mu   sync.Mutex
	ops  []Op
	seen = map[string]bool{}
)

// Register 注册操作文档；重复注册同一 method+path 会覆盖（幂等）。
func Register(op Op) {
	mu.Lock()
	defer mu.Unlock()
	key := op.Method + " " + op.Path
	if seen[key] {
		for i := range ops {
			if ops[i].Method == op.Method && ops[i].Path == op.Path {
				ops[i] = op
				return
			}
		}
	}
	seen[key] = true
	ops = append(ops, op)
}

// All 返回按路径排序的全部操作。
func All() []Op {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Op, len(ops))
	copy(out, ops)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].Method < out[j].Method
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Document 生成 OpenAPI 3.0 文档对象（可 JSON 序列化）。
func Document(title, version string) map[string]any {
	paths := map[string]any{}
	for _, op := range All() {
		item, ok := paths[op.Path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[op.Path] = item
		}
		operation := map[string]any{
			"summary": op.Summary,
			"tags":    []string{op.Tag},
			"responses": map[string]any{
				"200": map[string]any{"description": "ok"},
				"401": map[string]any{"description": "未认证"},
				"403": map[string]any{"description": "无权限（iam.PolicyEngine 裁决拒绝）"},
			},
		}
		if op.Permission != "" {
			operation["x-required-permission"] = op.Permission
			operation["security"] = []any{map[string]any{"bearerAuth": []string{}}}
		}
		if op.Idempotent {
			operation["x-idempotent"] = true
		}
		item[strLower(op.Method)] = operation
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": title, "version": version},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT"},
			},
		},
		"paths": paths,
	}
}

// Handler 返回 /openapi.json 的 HTTP 处理器。
func Handler(title, version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = writeJSON(w, Document(title, version))
	}
}

func strLower(s string) string {
	out := []rune(s)
	for i := range out {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 32
		}
	}
	return string(out)
}
