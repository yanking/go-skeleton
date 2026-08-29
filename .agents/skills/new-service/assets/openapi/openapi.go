// Package openapi 聚合 buf 从 proto 生成的 OpenAPI v3 文档并提供只读出口：
// /docs 是阅读页（Redoc 渲染，脚本经 CDN 加载并钉死版本——离线环境渲染页
// 不可用时仍可直取 spec），/docs/<svc>/v1/x.openapi.json 为各服务契约原文。
// spec 由 `make proto` 生成、经 go:embed 进二进制——文档与部署版本天然一致，
// 服务 HTTP 出口把 Handler() 挂到 /docs 即可。
package openapi

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
)

//go:embed */v1/*.json
var specs embed.FS

//go:embed docs.html
var docsHTML []byte

// Handler 返回文档出口：/docs 阅读页、/docs/manifest.json 服务清单、
// /docs/<svc>/v1/x.openapi.json 契约原文。
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(docsHTML)
	})
	mux.HandleFunc("/docs/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		names, err := fs.Glob(specs, "*/v1/*.json")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(names)
	})
	mux.Handle("/docs/", http.StripPrefix("/docs/", http.FileServer(http.FS(specs))))
	return mux
}
