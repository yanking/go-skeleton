package transport

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
)

// readHeaderTimeout 读请求头的超时，防止慢速连接占着 goroutine 不放。
const readHeaderTimeout = 10 * time.Second

// 元端点路径。新增须经用户批准并同步 architecture.md 的元端点清单（宪法第一条）。
const (
	healthzPath     = "/healthz"
	openapiJSONPath = "/openapi.json"
	docsPath        = "/docs"
)

// scalarVersion 是接口文档阅读器的版本，也是全仓库该版本的唯一事实源：
// Makefile 的 make docs 用 sed 读这一行来下载同版本的离线包，保证本地看到的
// 与服务端渲染的是同一个东西。改这里即同时改掉两边，不会漂移。
// 不能省成裸 URL——CDN 会把它解析到 @latest，Scalar 发新版就跟着变。
const scalarVersion = "1.66.1"

// docsPage 是一张只有一个 script 标签的页面，把 /openapi.json 交给浏览器端的阅读器渲染。
// 阅读器脚本走 CDN：单文件 3.8MB，打进二进制让每个服务都背着它不值得；
// 代价是无外网时 /docs 打不开——那时用 make docs 生成离线页，或直接取 /openapi.json。
const docsPage = `<!doctype html>
<html lang="zh">
<head><meta charset="utf-8"><title>接口文档</title></head>
<body>
<script id="api-reference" data-url="` + openapiJSONPath + `"></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@` + scalarVersion + `/dist/browser/standalone.js"></script>
</body>
</html>`

// newGatewayMux 造 grpc-gateway 的 mux 并挂上元端点。
func newGatewayMux(cfg Config) *runtime.ServeMux {
	mux := runtime.NewServeMux()
	// /healthz 是基础设施元端点（宪法第一条例外），不进 openapi/，也不做业务鉴权。
	// 它只表示「进程活着」（liveness）；「是否可接流量」（readiness）看 gRPC Health v1。
	handle(mux, healthzPath, healthz)

	// 不传文档就彻底不注册这两个端点——「关闭」应当是不存在，而不是存在但返回空。
	if cfg.OpenAPI != nil {
		handle(mux, openapiJSONPath, serveJSON(cfg.OpenAPI))
		handle(mux, docsPath, serveHTML([]byte(docsPage)))
	}
	return mux
}

// handle 注册一个 GET 元端点，注册失败即 panic——路径写错属装配期错误。
func handle(mux *runtime.ServeMux, path string, h runtime.HandlerFunc) {
	if err := mux.HandlePath(http.MethodGet, path, h); err != nil {
		panic(fmt.Errorf("装配 Transport: 注册元端点 %s: %w", path, err))
	}
}

// serveJSON 原样返回一段 JSON。
func serveJSON(body []byte) runtime.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// serveHTML 原样返回一段 HTML。
func serveHTML(body []byte) runtime.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}
}

func healthz(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"SERVING"}`)
}

// httpServer 是 HTTP/JSON 网关的 app.Component 适配器。
type httpServer struct {
	srv    *http.Server
	ln     net.Listener
	logger *slog.Logger
}

func newHTTPServer(mux *runtime.ServeMux, ln net.Listener, logger *slog.Logger) *httpServer {
	return &httpServer{
		srv:    &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout},
		ln:     ln,
		logger: logger,
	}
}

// Name 实现 app.Component。
func (s *httpServer) Name() string { return "http" }

// Start 阻塞运行，直到 Stop 被调用或监听器出错。
func (s *httpServer) Start(context.Context) error { return s.srv.Serve(s.ln) }

// Stop 优雅停机：不再接新连接，等在途请求跑完；ctx 到期则强制关闭剩余连接。
func (s *httpServer) Stop(ctx context.Context) error { return s.srv.Shutdown(ctx) }
