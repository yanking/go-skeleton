// Package transport 装配服务的对外传输层：同进程内的 gRPC 与 HTTP 双协议。
// 它只提供领域无关的机制（双端口、gateway 环回、健康检查、通用拦截器、优雅停机），
// 具体服务的 pb 注册与自有拦截器由调用方经 Config 传入。
package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"

	"github.com/yanking/go-skeleton/pkg/app"
)

// Telemetry 可观测性提供方。pkg/telemetry.Telemetry 靠结构化接口天然满足，
// 故本包不 import pkg/telemetry——依赖只朝一个方向，谁也不必知道对方存在。
type Telemetry interface {
	TracerProvider() trace.TracerProvider
	MeterProvider() metric.MeterProvider
	Propagator() propagation.TextMapPropagator
}

// Config 传输层装配参数。
type Config struct {
	// Service 服务名，用于日志标注；必填。
	Service string
	// GRPCAddr 纯 gRPC 监听地址，如 :9090；必填。
	GRPCAddr string
	// HTTPAddr HTTP/JSON 监听地址，如 :8080；必填。
	HTTPAddr string
	// RegisterGRPC 注册本服务的 pb Server 实现；在 Serve 之前由 MustNew 调用。
	// 收进 Config 而非事后暴露 *grpc.Server，是因为 Serve 之后再 RegisterService 会 panic——
	// 这个顺序约束不该压给使用方。
	RegisterGRPC func(*grpc.Server)
	// RegisterGateway 直接填 protoc-gen-grpc-gateway 生成的 RegisterXxxHandler，可多个。
	RegisterGateway []GatewayRegistrar
	// Interceptors 服务自有的一元拦截器（如鉴权），插在通用拦截器之后、handler 之前，
	// 即架构约定的「鉴权」位：recovery → 日志 → 本字段 → handler。
	Interceptors []grpc.UnaryServerInterceptor
	// Telemetry 可观测性提供方，nil 时完全不挂 StatsHandler（零开销）。
	Telemetry Telemetry
	// Logger 传输层日志，nil 时用 slog.Default()。
	Logger *slog.Logger
}

// Transport 持有已装配好的传输层组件。
type Transport struct {
	logger   *slog.Logger
	grpc     *grpcServer
	loopback *loopbackConn
	http     *httpServer
	grpcLn   net.Listener
	httpLn   net.Listener
}

// MustNew 装配传输层：占好两个端口、注册健康检查与业务实现。
// 端口占不到、配置非法都直接 panic——起不来就死，不留「端口没起来但进程还活着」的状态。
func MustNew(ctx context.Context, cfg Config) *Transport {
	if cfg.Service == "" {
		panic(errors.New("装配 Transport: Service 不能为空"))
	}
	if cfg.GRPCAddr == "" {
		panic(errors.New("装配 Transport: GRPCAddr 不能为空"))
	}
	if cfg.HTTPAddr == "" {
		panic(errors.New("装配 Transport: HTTPAddr 不能为空"))
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	grpcLn := mustListen(cfg.GRPCAddr, "gRPC")
	httpLn := mustListen(cfg.HTTPAddr, "HTTP")

	mux := newGatewayMux()
	loopback := mustLoopbackConn(cfg, grpcLn)
	for _, register := range cfg.RegisterGateway {
		if err := register(ctx, mux, loopback.conn); err != nil {
			panic(fmt.Errorf("装配 Transport: 注册 gateway handler: %w", err))
		}
	}

	return &Transport{
		logger:   logger,
		grpc:     newGRPCServer(cfg, grpcLn, logger),
		loopback: loopback,
		http:     newHTTPServer(mux, httpLn, logger),
		grpcLn:   grpcLn,
		httpLn:   httpLn,
	}
}

// Components 返回排好序的传输组件，直接 append 进 app.New 即可。
// 顺序即拉起顺序，其逆序即停机顺序——调用方无需理解为什么，也没有排错的机会。
func (t *Transport) Components() []app.Component {
	return []app.Component{t.grpc, t.loopback, t.http}
}

// GRPCAddr 返回 gRPC 实际监听的地址；配置里写 :0 时可用它取到真实端口。
func (t *Transport) GRPCAddr() string { return t.grpcLn.Addr().String() }

// HTTPAddr 返回 HTTP 实际监听的地址。
func (t *Transport) HTTPAddr() string { return t.httpLn.Addr().String() }

// mustListen 在装配期占好端口，占不到当场 panic。
func mustListen(addr, name string) net.Listener {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic(fmt.Errorf("装配 Transport: 监听 %s 端口 %s: %w", name, addr, err))
	}
	return ln
}
