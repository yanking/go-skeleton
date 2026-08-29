package transport

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// ServiceRegistrar 把 pb 服务实现注册进 gRPC 服务器，形如：
//
//	transport.WithService(func(s *grpc.Server) {
//	    xxxv1.RegisterXxxServiceServer(s, handler.NewGRPC(svc))
//	})
type ServiceRegistrar func(*grpc.Server)

// GatewayRegistrar 注册 pb 的 gateway 路由，签名与 grpc-gateway 生成的
// RegisterXxxHandler 一致——生成函数可直接传进 WithGateway，无须包装。
type GatewayRegistrar func(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error

// Option 传输装配选项，语义见各 With 函数。
type Option func(*options)

// mount 一个 HTTP 挂载点。
type mount struct {
	pattern string
	handler http.Handler
}

// options 汇总全部选项，零值即「只有 gRPC、无追踪无日志无鉴权」。
type options struct {
	tracerProvider trace.TracerProvider
	logger         *slog.Logger
	authenticator  Authenticator
	services       []ServiceRegistrar
	gateways       []GatewayRegistrar
	mounts         []mount
	unary          []grpc.UnaryServerInterceptor
	stream         []grpc.StreamServerInterceptor
	grpcOptions    []grpc.ServerOption
}

func newOptions(opts []Option) *options {
	o := &options{}
	for _, apply := range opts {
		apply(o)
	}
	return o
}

// WithTracerProvider 注入链路追踪，不给则不埋 span。
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProvider = tp }
}

// WithLogger 注入访问日志输出，不给则不挂访问日志拦截器。
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithAuthenticator 注入鉴权策略，不给则不挂鉴权拦截器；探针与 reflection 恒定放行。
func WithAuthenticator(a Authenticator) Option {
	return func(o *options) { o.authenticator = a }
}

// WithService 注册一个 pb 服务实现，可重复调用以注册多个服务。
func WithService(register ServiceRegistrar) Option {
	return func(o *options) { o.services = append(o.services, register) }
}

// WithGateway 注册一个 pb 的 gateway 路由，可重复调用以注册多个服务。
// 它声明「本服务有 HTTP 转译能力」，实际监听与否由配置的 http_addr 决定：
// 两者缺一即装配期报错（配了端口没路由）或不开 HTTP（有能力但本次部署不暴露）。
func WithGateway(register GatewayRegistrar) Option {
	return func(o *options) { o.gateways = append(o.gateways, register) }
}

// WithMount 在 gateway 之外挂一个 HTTP 处理器（如 /docs 文档出口）。
// pattern 须以 / 开头且不能是根路径——根路径归 gateway。精确与子树两个匹配模式
// 由本包一并注册，调用方无须自己补 pattern+"/"。需要 HTTP 出口，故必须同时 WithGateway。
func WithMount(pattern string, h http.Handler) Option {
	return func(o *options) { o.mounts = append(o.mounts, mount{pattern: pattern, handler: h}) }
}

// WithUnaryInterceptor 追加自定义 Unary 拦截器，挂在内置链（出口翻译 → 访问日志 →
// 鉴权）之内、handler 之外，按传入顺序执行。
func WithUnaryInterceptor(interceptors ...grpc.UnaryServerInterceptor) Option {
	return func(o *options) { o.unary = append(o.unary, interceptors...) }
}

// WithStreamInterceptor 追加自定义 Stream 拦截器，位置与顺序同 WithUnaryInterceptor。
func WithStreamInterceptor(interceptors ...grpc.StreamServerInterceptor) Option {
	return func(o *options) { o.stream = append(o.stream, interceptors...) }
}

// WithGRPCServerOption 透传原生 grpc.ServerOption（消息体上限、keepalive 参数、
// TLS 凭证等），本包不为每个 gRPC 旋钮再包一层选项。
func WithGRPCServerOption(opts ...grpc.ServerOption) Option {
	return func(o *options) { o.grpcOptions = append(o.grpcOptions, opts...) }
}
