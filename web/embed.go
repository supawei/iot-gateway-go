// Package web 内嵌前端构建产物(dist/),供网关进程直接提供管理界面,
// 部署时无需额外的静态服务器或反向代理。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler 返回服务内嵌前端静态资源的 http.Handler。
// 采用 SPA 回退:请求路径对应的静态文件不存在时返回 index.html,
// 使 vue-router 的 history 路由(如 /devices)刷新后仍能正确加载。
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("web: embed dist failed: " + err.Error())
	}
	indexHTML, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic("web: embed index.html failed: " + err.Error())
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "index.html" {
			writeIndex(w, indexHTML)
			return
		}
		if f, err := sub.Open(name); err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// 静态资源不存在:SPA 回退到 index.html
		writeIndex(w, indexHTML)
	})
}

// writeIndex 返回 index.html;禁缓存,避免 SPA 升级后浏览器沿用旧入口引用过期资源。
func writeIndex(w http.ResponseWriter, indexHTML []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(indexHTML)
}
