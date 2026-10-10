// Package webui embeds a small local workspace UI alongside the chat API.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// 前端文件会编译进 Go 二进制，部署时无需单独拷贝静态目录。
//
//go:embed all:static
var assets embed.FS

var content, _ = fs.Sub(assets, "static")

type handler struct {
	api   http.Handler
	oauth http.Handler
}

// New 创建同时托管嵌入式页面与后端 API 的 Handler。
func New(api http.Handler, oauth ...http.Handler) http.Handler {
	h := handler{api: api}
	if len(oauth) > 0 {
		h.oauth = oauth[0]
	}
	return h
}

// ServeHTTP 由 Go 的 net/http 按请求调用：静态路径读取嵌入资源，其余路径交给 API handler。
func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/auth/openai/") {
		if h.oauth == nil {
			http.NotFound(w, r)
			return
		}
		h.oauth.ServeHTTP(w, r)
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/index.html" && !strings.HasPrefix(r.URL.Path, "/assets/") {
		h.api.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if strings.Contains(name, "..") || strings.Contains(name, "\\") {
		http.NotFound(w, r)
		return
	}
	if name != "index.html" && name != "assets/app.css" && name != "assets/app.js" && name != "assets/mark.svg" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, content, name)
}
