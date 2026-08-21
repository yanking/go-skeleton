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

// newGatewayMux 造 grpc-gateway 的 mux 并挂上元端点。
func newGatewayMux() *runtime.ServeMux {
	mux := runtime.NewServeMux()
	// /healthz 是基础设施元端点（宪法第一条例外），不进 openapi/，也不做业务鉴权。
	// 它只表示「进程活着」（liveness）；「是否可接流量」（readiness）看 gRPC Health v1。
	if err := mux.HandlePath(http.MethodGet, "/healthz", healthz); err != nil {
		panic(fmt.Errorf("装配 Transport: 注册 /healthz: %w", err))
	}
	return mux
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
