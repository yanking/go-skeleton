package transport

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// ServiceRegistrar 把 pb 服务实现注册进 gRPC 服务器，形如：
//
//	transport.WithService(func(s *grpc.Server) {
//	    xxxv1.RegisterXxxServiceServer(s, handler.NewGRPC(svc))
//	})
type ServiceRegistrar func(*grpc.Server)

// GatewayRegistrar 注册 pb 的 gateway 路由（环回转译），签名与 grpc-gateway 生成的
// RegisterXxxHandler 一致——生成函数可直接传进 WithGateway，无须包装。
type GatewayRegistrar func(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error

// DirectRegistrar 注册 pb 的直连路由，签名与 grpc-gateway 生成的 RegisterXxxHandlerServer
// 的前两参一致——生成函数经闭包直接传入：
//
//	transport.WithDirectGateway(func(ctx context.Context, mux *runtime.ServeMux) error {
//	    return xxxv1.RegisterXxxHandlerServer(ctx, mux, handler.NewGRPC(svc))
//	})
type DirectRegistrar func(ctx context.Context, mux *runtime.ServeMux) error

// Option 传输装配选项，语义见各 With 函数。
type Option func(*options)

// mount 一个 HTTP 挂载点。
type mount struct {
	pattern string
	handler http.Handler
}

// ReadinessCheck 一个依赖就绪检查，返回非 nil 即本实例尚不能接流量。
// 实现应当轻量且带 ctx 超时：就绪探针每隔几秒打一次，检查自己卡住会让
// 实例被误判为不可用。
type ReadinessCheck func(ctx context.Context) error

// options 汇总全部选项，零值即「只有 gRPC、无追踪无日志无鉴权」。
type options struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	readiness      []ReadinessCheck
	logger         *slog.Logger
	authenticator  Authenticator
	services       []ServiceRegistrar
	gateways       []GatewayRegistrar
	directGateways []DirectRegistrar
	middlewares    []runtime.Middleware
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

// WithMeterProvider 注入指标 Provider，不给则不埋请求指标。
//
// 它带来的是 RED 三件套——请求量、错误率、延迟分位（otelgrpc 的
// rpc.server.* 与 otelhttp 的 http.server.*）。不注入的话 instrumentation
// 回落到 otel 的 no-op 全局：不报错，只是这些指标静默消失，而「接口变慢了、
// 错误率涨了」恰恰只能靠它们定位。
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) { o.meterProvider = mp }
}

// WithReadinessCheck 追加依赖就绪检查，可重复调用；全部通过才算就绪。
//
// 检查结果由 HTTP 的 /readyz 与 gRPC health 服务共用。典型用法是 ping 数据库：
//
//	transport.WithReadinessCheck(func(ctx context.Context) error {
//	    return db.WithContext(ctx).Exec("SELECT 1").Error
//	})
//
// 这是「装配期不 ping 库」那个取舍的另一半：不 ping 是因为 DB 比进程晚就绪是
// 常态，而连不上库的实例不该接流量——两件事由这里分开表达。
func WithReadinessCheck(checks ...ReadinessCheck) Option {
	return func(o *options) { o.readiness = append(o.readiness, checks...) }
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

// WithGateway 注册一个 pb 的 gateway 路由（环回转译），可重复调用以注册多个服务，
// 供 NewRPCRESTServer 使用——它与该构造器必配的 http_addr 一起声明「双端口形态」。
// 直连注册（NewRESTServer）用 WithDirectGateway。
func WithGateway(register GatewayRegistrar) Option {
	return func(o *options) { o.gateways = append(o.gateways, register) }
}

// WithDirectGateway 注册一个 pb 的直连路由，可重复调用以注册多个服务，供
// NewRESTServer 使用：生成函数 RegisterXxxHandlerServer 直接调用 handler 实现，
// 不经 gRPC 服务器、不经环回。直连形态不支持流式方法——生成代码对流式方法一律回
// Unimplemented（gateway 直连模式不支持，grpc/grpc-go#906），契约里有流式方法的
// 服务用 NewRPCRESTServer。
func WithDirectGateway(register DirectRegistrar) Option {
	return func(o *options) { o.directGateways = append(o.directGateways, register) }
}

// WithMiddleware 追加自定义 HTTP 中间件，挂在内置链（访问日志 → 鉴权）之内、
// handler 之外，按传入顺序由外到内执行，可重复调用以追加多个——位置与 gRPC 侧的
// WithUnaryInterceptor 一致，故同一套横切关注点换形态时的相对次序不变。
//
// 中间件由 ServeMux.Handle 在路由注册时挂上，只包住 gateway 路由：/healthz 与
// WithMount 挂载点不经此，探针与文档页因此保持免鉴权。
//
// 只有 NewRESTServer 接受它——该形态没有 gRPC 拦截器可挂。环回双端口形态不开这个
// 口子：那里的横切关注点写成 gRPC 拦截器经 WithUnaryInterceptor 注入，HTTP 与 gRPC
// 两种协议一并覆盖，而 HTTP 专用中间件只盖住一半，是会静默漏掉东西向流量的陷阱。
func WithMiddleware(middlewares ...runtime.Middleware) Option {
	return func(o *options) { o.middlewares = append(o.middlewares, middlewares...) }
}

// WithMount 在 gateway 之外挂一个 HTTP 处理器（如 /docs 文档出口）。
// pattern 须以 / 开头且不能是根路径——根路径归 gateway。精确与子树两个匹配模式
// 由本包一并注册，调用方无须自己补 pattern+"/"。需要 HTTP 出口，故只有
// NewRPCRESTServer 与 NewRESTServer 接受它。
func WithMount(pattern string, h http.Handler) Option {
	return func(o *options) { o.mounts = append(o.mounts, mount{pattern: pattern, handler: h}) }
}

// WithUnaryInterceptor 追加自定义 Unary 拦截器，挂在内置链（出口翻译 → 访问日志 →
// 鉴权）之内、handler 之外，按传入顺序执行。gRPC 拦截器随 gRPC 服务器存在，
// 故只有 NewRPCServer 与 NewRPCRESTServer 接受它。
func WithUnaryInterceptor(interceptors ...grpc.UnaryServerInterceptor) Option {
	return func(o *options) { o.unary = append(o.unary, interceptors...) }
}

// WithStreamInterceptor 追加自定义 Stream 拦截器，位置与顺序同 WithUnaryInterceptor。
func WithStreamInterceptor(interceptors ...grpc.StreamServerInterceptor) Option {
	return func(o *options) { o.stream = append(o.stream, interceptors...) }
}

// WithGRPCServerOption 透传原生 grpc.ServerOption（TLS 凭证、改写本包设的消息体
// 上限与 keepalive 参数等），本包不为每个 gRPC 旋钮再包一层选项。只有 NewRPCServer
// 与 NewRPCRESTServer 接受它——NewRESTServer 没有 gRPC 服务器。
//
// 不要传 grpc.UnaryInterceptor / grpc.StreamInterceptor（非 Chain 形态）：grpc-go
// 会把它们预置到全部 Chain 拦截器之前，于是它跑在本包的出口翻译之外，裸错误原文
// 直通客户端——「出口翻译固定为链首」这条不变量就此被绕开。自定义拦截器一律用
// WithUnaryInterceptor / WithStreamInterceptor，它们挂在链内的正确位置。
//
// 本包挡不住这件事：grpc.ServerOption 是不透明的函数类型，装配期分辨不出传进来的
// 是哪一个选项。别处的错配都是装配期 panic，这里只能靠约定——纪律上的这处不一致
// 是已知的，写在这里而不是假装不存在。
func WithGRPCServerOption(opts ...grpc.ServerOption) Option {
	return func(o *options) { o.grpcOptions = append(o.grpcOptions, opts...) }
}
