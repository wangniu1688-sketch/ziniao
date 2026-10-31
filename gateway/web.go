package gateway

import (
	"embed"
	"net/http"
)

// Web 控制台：由服务端直接托管（零 Node 构建依赖），浏览器打开 http://127.0.0.1:8080 即用。
//
//go:embed web/index.html web/app.js web/style.css web/fp-test.html
var webFS embed.FS

func (s *Server) webRoutes() {
	m := s.mux
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		b, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "web asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(webFS))))
	// 指纹实测页：环境窗口打开后直接看到注入结果（只读页面，不暴露任何业务数据）
	m.HandleFunc("GET /fp-test", func(w http.ResponseWriter, _ *http.Request) {
		b, err := webFS.ReadFile("web/fp-test.html")
		if err != nil {
			http.Error(w, "web asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
}
