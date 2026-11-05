// Package transport 构造服务的对外传输组件 Server：装配期监听端口（起不来就死），
// 实现 app.Component（Start 阻塞服务、Stop 优雅停机），并承担错误处理约定的
// 「出口层」职责——把 errcode 的业务字段翻译为 gRPC status / HTTP 响应，
// 原始错误链留在服务端日志。
//
// 三种构造器对应三种形态，共用同一份 Config，各字段按形态必填或必空：
//   - NewRPCServer：纯 gRPC，只开 grpc_addr 一个端口；
//   - NewRPCRESTServer：gRPC + gateway 双端口。HTTP 侧是 grpc-gateway 从同一份
//     proto 契约转译出的代理出口：把 HTTP/JSON 请求翻成 gRPC 调用，经环回连接打回
//     本进程的 gRPC 端口，拦截链对两种协议同样生效，业务代码只写一遍；
//   - NewRESTServer：直连 REST，只开 http_addr 一个端口，没有 gRPC 服务器——pb
//     生成的 RegisterXxxHandlerServer（经 WithDirectGateway）把 handler 实现直接
//     注册进 gateway 路由，请求只做一次 JSON 编解码、直接函数调用 handler，不经
//     环回、不做第二份 proto 编解码，高 QPS 或大报文的 HTTP 出口用它。代价见
//     WithDirectGateway：流式方法一律 Unimplemented；没有 gRPC 端口，东西向调用方
//     与 grpcurl 无处可去；客户端断连不会取消处理中的请求（生成代码以注册 ctx
//     为请求 ctx 的父，取消只随停机信号传导）。
//
// 两个端口各自监听：gRPC 与 HTTP 的暴露范围、mesh 协议标注（Istio 的端口名 /
// appProtocol 是按端口生效的）、网络策略往往不同——常见形态就是 gRPC 只走东西向、
// HTTP 才对外。
//
// gRPC 侧拦截链按注册顺序组装、先注册者在最外层，业务 handler 位于最内：errcode
// 出口翻译固定为链首（仓库级约定，后续选项只能追加、顶不掉它；漏翻译的错误在此
// 兜成通用内部错误、ctx 超时/取消兜成 10005/10006、故障类 status 只换消息，原文
// 只进访问日志）；其后是访问日志（WithLogger 才挂：成功 Info、失败 Warn，记方法、
// 来源地址、状态码与耗时，trace_id 由日志钩子附带）与鉴权（WithAuthenticator 才挂：
// health/reflection 前缀豁免——拦探针会让编排系统误判实例死亡；策略由服务注入，
// FixedBearerAuth 做固定 token 比对、空 token 全拒绝），再后是 WithUnaryInterceptor /
// WithStreamInterceptor 传入的自定义拦截器；otel 遥测另经 StatsHandler 挂载
// （WithTracerProvider 才挂）。经 gateway 环回的 HTTP 流量穿过同一条链。
//
// 直连 REST 形态没有 gRPC 拦截器可挂，等价链路经 runtime.WithMiddlewares 组装在
// 每条 gateway 路由上，链序相同（访问日志在外、鉴权在内）：出口翻译由 mux 的错误
// 处理器承担，与环回形态共用同一渲染，客户端契约一致；鉴权策略收到的 method 参数
// 是 HTTP 路径而非 gRPC 方法名（细节见 middleware.go）。otel 经 otelhttp 包住整个
// mux（WithTracerProvider 才挂），入站 W3C traceparent 也因此得以提取续链——环回
// 形态的 HTTP 端口没有这层提取。/healthz 与 WithMount 挂载点不经 middleware，对
// 探针与文档页保持免鉴权。
//
// 用法：
//
//	srv := transport.NewRPCRESTServer(ctx, c.Transport,
//	    transport.WithLogger(logger),
//	    transport.WithAuthenticator(transport.FixedBearerAuth(c.Auth.Token)),
//	    transport.WithService(func(s *grpc.Server) {
//	        xxxv1.RegisterXxxServiceServer(s, handler.NewGRPC(svc))
//	    }),
//	    transport.WithGateway(xxxv1.RegisterXxxServiceHandler),
//	    transport.WithMount("/docs", openapi.Handler()),
//	)
package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// HTTP 出口的两个恒定安全默认，不设配置面。
//
// 刻意不设 ReadTimeout / WriteTimeout：它们是覆盖整个请求/响应周期的硬上限，
// 而 gateway 会把 proto 里的服务端流式方法转译成长连接响应——加了就是按秒切流。
// 需要限制单次业务耗时的，用 ctx 超时或部署侧的网关。
const (
	// defaultReadHeaderTimeout 请求头读取上限，挡住只发头不发完的慢连接。
	defaultReadHeaderTimeout = 10 * time.Second
	// defaultIdleTimeout keep-alive 连接的空闲上限，到点回收，避免闲置连接堆积
	// 占住文件描述符。不设则退化为 ReadTimeout，而后者按上面的理由是 0（无上限）。
	defaultIdleTimeout = 120 * time.Second
)

// Config 传输装配参数，三种构造器共用，字段按形态必填或必空（见各构造器）。
type Config struct {
	// GRPCAddr gRPC 监听地址（如 ":9090"）。NewRPCServer / NewRPCRESTServer 必填；
	// NewRESTServer 必须为空——该形态没有 gRPC 端口。
	GRPCAddr string `yaml:"grpc_addr"`
	// HTTPAddr gateway 出口的监听地址（如 ":8080"）。NewRPCRESTServer（环回转译）与
	// NewRESTServer（直连）必填；NewRPCServer 必须为空——该形态不开 HTTP。
	HTTPAddr string `yaml:"http_addr"`
}

// Server 服务的对外传输出口，实现 app.Component。grpc 与 http 哪些生效由所选
// 构造器决定：直连 REST 形态没有 gRPC 服务器，grpc/grpcLn/conn 为 nil；纯 gRPC
// 形态没有 HTTP 出口，http/httpLn 为 nil。
type Server struct {
	grpc   *grpc.Server
	grpcLn net.Listener

	// 以下字段仅在开启 HTTP 出口时非 nil，nil 即纯 gRPC 形态。
	http   *http.Server
	httpLn net.Listener
	// conn 是环回连接，仅 NewRPCRESTServer 形态非 nil；直连形态无环回。
	conn *grpc.ClientConn
}

// NewRPCServer 构造纯 gRPC 形态：只监听 grpc_addr，不开 HTTP。此刻即「起不来就该死」
// 的装配期：端口占用、配置与选项自相矛盾一律 panic。全部注册在返回前完成，
// 调用方拿到的即可运行状态。
func NewRPCServer(cfg Config, opts ...Option) *Server {
	o := newOptions(opts)
	validateRPC(cfg, o)

	grpcLn, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 gRPC %s: %w", cfg.GRPCAddr, err))
	}

	s := &Server{grpc: newGRPCServer(o), grpcLn: grpcLn}
	for _, register := range o.services {
		register(s.grpc)
	}
	return s
}

// NewRPCRESTServer 构造双端口形态：gRPC 走 grpc_addr，gateway 转译的 HTTP 出口走
// http_addr，HTTP 流量经环回连接打回本进程 gRPC、穿过与原生 gRPC 相同的拦截链。
// ctx 用于 gateway 路由注册；装配期语义同 NewRPCServer。
func NewRPCRESTServer(ctx context.Context, cfg Config, opts ...Option) *Server {
	o := newOptions(opts)
	validateRPCREST(cfg, o)

	grpcLn, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 gRPC %s: %w", cfg.GRPCAddr, err))
	}

	s := &Server{grpc: newGRPCServer(o), grpcLn: grpcLn}
	for _, register := range o.services {
		register(s.grpc)
	}
	s.attachHTTP(ctx, cfg.HTTPAddr, o)
	return s
}

// NewRESTServer 构造直连 REST 形态：只监听 http_addr，没有 gRPC 服务器——handler
// 实现经 WithDirectGateway 直接注册进 gateway 路由（能力与代价见 WithDirectGateway）。
// 装配期语义同 NewRPCServer。传给本函数的 ctx 成为直连路由的注册 ctx，按 gateway
// 生成代码的约定它是每个请求 handler ctx 的父——ctx 取消（停机信号）会连带取消
// 处理中的请求，应传 App 的根 ctx。
func NewRESTServer(ctx context.Context, cfg Config, opts ...Option) *Server {
	o := newOptions(opts)
	validateREST(cfg, o)

	s := &Server{}
	s.attachDirectHTTP(ctx, cfg.HTTPAddr, o)
	return s
}

// validateRPC 把「装配起来必然出错」的组合挡在监听端口之前：纯 gRPC 形态。
func validateRPC(cfg Config, o *options) {
	switch {
	case cfg.GRPCAddr == "":
		panic(errors.New("装配 transport: grpc_addr 不能为空——双端口形态用 NewRPCRESTServer"))
	case cfg.HTTPAddr != "":
		panic(errors.New("装配 transport: NewRPCServer 是纯 gRPC 形态却配了 http_addr——双端口形态用 NewRPCRESTServer"))
	case len(o.gateways) > 0:
		panic(errors.New("装配 transport: WithGateway 属双端口形态（NewRPCRESTServer），纯 gRPC 形态没有 HTTP 出口"))
	case len(o.directGateways) > 0:
		panic(errors.New("装配 transport: WithDirectGateway 属直连 REST 形态（NewRESTServer），纯 gRPC 形态没有 HTTP 出口"))
	case len(o.mounts) > 0:
		panic(errors.New("装配 transport: 挂载点需要 HTTP 出口——双端口形态用 NewRPCRESTServer，直连形态用 NewRESTServer"))
	}
}

// validateRPCREST 同 validateRPC：双端口形态。
func validateRPCREST(cfg Config, o *options) {
	switch {
	case cfg.GRPCAddr == "":
		panic(errors.New("装配 transport: grpc_addr 不能为空"))
	case cfg.HTTPAddr == "":
		panic(errors.New("装配 transport: NewRPCRESTServer 是双端口形态，http_addr 必配——纯 gRPC 形态用 NewRPCServer"))
	case len(o.gateways) == 0:
		panic(errors.New("装配 transport: 配了 http_addr 却没有 WithGateway，HTTP 端口上不会有任何路由"))
	case len(o.directGateways) > 0:
		panic(errors.New("装配 transport: WithDirectGateway 属直连 REST 形态（NewRESTServer），环回形态的路由用 WithGateway"))
	}
	validateMounts(o.mounts)
}

// validateREST 同 validateRPC：直连 REST 形态。没有 gRPC 服务器，gRPC 侧的选项
// 在这里都是「装配了也无处生效」，一律挡下而非静默忽略。
func validateREST(cfg Config, o *options) {
	switch {
	case cfg.HTTPAddr == "":
		panic(errors.New("装配 transport: NewRESTServer 须配 http_addr"))
	case cfg.GRPCAddr != "":
		panic(errors.New("装配 transport: NewRESTServer 是纯 HTTP 形态却配了 grpc_addr——双端口形态用 NewRPCRESTServer"))
	case len(o.directGateways) == 0:
		panic(errors.New("装配 transport: 没有 WithDirectGateway，HTTP 端口上不会有任何路由"))
	case len(o.gateways) > 0:
		panic(errors.New("装配 transport: WithGateway 属环回双端口形态（NewRPCRESTServer），直连路由用 WithDirectGateway"))
	case len(o.services) > 0:
		panic(errors.New("装配 transport: WithService 注册 gRPC 服务实现，NewRESTServer 没有 gRPC 服务器——handler 经 WithDirectGateway 直接注册"))
	case len(o.unary) > 0 || len(o.stream) > 0:
		panic(errors.New("装配 transport: 自定义 gRPC 拦截器在直连形态没有挂载点——鉴权与访问日志由本包经 middleware 提供，其余横切关注点包在 WithDirectGateway 的路由层实现"))
	case len(o.grpcOptions) > 0:
		panic(errors.New("装配 transport: WithGRPCServerOption 在直连形态没有 gRPC 服务器可配"))
	}
	validateMounts(o.mounts)
}

// validateMounts 校验挂载模式：以 / 开头且不能是根路径——根路径归 gateway。
func validateMounts(mounts []mount) {
	for _, m := range mounts {
		if !strings.HasPrefix(m.pattern, "/") || strings.TrimSuffix(m.pattern, "/") == "" {
			panic(fmt.Errorf("装配 transport: 挂载模式 %q 非法，须以 / 开头且不能是根路径（根路径归 gateway）", m.pattern))
		}
	}
}

// newGRPCServer 按选项装好拦截链与内置服务。
func newGRPCServer(o *options) *grpc.Server {
	// 出口层错误翻译是仓库级约定，作为链首固定挂上；Chain*Interceptor 多次调用是
	// 追加语义，后面的注入项与自定义拦截器都不会顶掉它。
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(errCodeToStatus),
		grpc.ChainStreamInterceptor(errCodeToStatusStream),
	}
	if o.tracerProvider != nil {
		opts = append(opts, grpc.StatsHandler(
			otelgrpc.NewServerHandler(otelgrpc.WithTracerProvider(o.tracerProvider))))
	}
	if o.logger != nil {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(unaryAccessLog(o.logger)),
			grpc.ChainStreamInterceptor(streamAccessLog(o.logger)))
	}
	if o.authenticator != nil {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(unaryAuth(o.authenticator)),
			grpc.ChainStreamInterceptor(streamAuth(o.authenticator)))
	}
	if len(o.unary) > 0 {
		opts = append(opts, grpc.ChainUnaryInterceptor(o.unary...))
	}
	if len(o.stream) > 0 {
		opts = append(opts, grpc.ChainStreamInterceptor(o.stream...))
	}
	opts = append(opts, o.grpcOptions...)

	srv := grpc.NewServer(opts...)
	healthpb.RegisterHealthServer(srv, health.NewServer()) // 探针用
	reflection.Register(srv)                               // 本地调试用
	return srv
}

// attachHTTP 装上环回转译出口：独立监听、环回连接、路由树。
func (s *Server) attachHTTP(ctx context.Context, addr string, o *options) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 HTTP %s: %w", addr, err))
	}
	s.httpLn = ln

	// 环回拨回本进程的 gRPC 端口，HTTP 流量因此同样穿过全部拦截器。
	// grpc.NewClient 惰性建连，装配期不触网。
	conn, err := grpc.NewClient(loopbackAddr(s.grpcLn), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		panic(fmt.Errorf("装配 transport: 拨号 gateway 环回: %w", err))
	}
	s.conn = conn

	gwMux := runtime.NewServeMux(
		runtime.WithErrorHandler(gatewayErrorHandler),
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
	)
	for _, register := range o.gateways {
		if err := register(ctx, gwMux, conn); err != nil {
			panic(fmt.Errorf("装配 transport: 注册 gateway 路由: %w", err))
		}
	}

	s.http = &http.Server{
		Handler:           mountMux(gwMux, o.mounts),
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}
}

// attachDirectHTTP 装上直连 REST 出口：gateway 路由直接注册 handler 实现，无 gRPC
// 服务器、无环回；鉴权与访问日志经 middleware 挂在每条路由上（链序见包注释），
// 错误出口由 mux 的错误处理器承担。
func (s *Server) attachDirectHTTP(ctx context.Context, addr string, o *options) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 HTTP %s: %w", addr, err))
	}
	s.httpLn = ln

	// 链序与 gRPC 侧一致：访问日志在外、鉴权在内（先注册者在外层，
	// runtime.chainMiddlewares 的组装语义）；出口翻译是错误处理器，天然在最外。
	var middlewares []runtime.Middleware
	if o.logger != nil {
		middlewares = append(middlewares, directAccessLog(o.logger))
	}
	if o.authenticator != nil {
		middlewares = append(middlewares, directAuth(o.authenticator))
	}
	gwMux := runtime.NewServeMux(
		runtime.WithErrorHandler(gatewayErrorHandler),
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
		runtime.WithMiddlewares(middlewares...),
	)
	for _, register := range o.directGateways {
		if err := register(ctx, gwMux); err != nil {
			panic(fmt.Errorf("装配 transport: 注册直连路由: %w", err))
		}
	}

	var handler http.Handler = mountMux(gwMux, o.mounts)
	if o.tracerProvider != nil {
		handler = otelhttp.NewHandler(handler, "http",
			otelhttp.WithTracerProvider(o.tracerProvider),
			otelhttp.WithPropagators(directPropagators),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			}))
	}
	s.http = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}
}

// mountMux 组外层路由树：/healthz 探针（HTTP 形态下探针走这里——没有 gRPC health
// 服务可打）、各挂载点、根路径归 gateway。精确与子树两个模式都要注册，
// 否则子路径会落进根路径的 gateway。
func mountMux(gwMux http.Handler, mounts []mount) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, mt := range mounts {
		pattern := strings.TrimSuffix(mt.pattern, "/")
		mux.Handle(pattern, mt.handler)
		mux.Handle(pattern+"/", mt.handler)
	}
	mux.Handle("/", gwMux)
	return mux
}

// Name 实现 app.Component。
func (s *Server) Name() string { return "transport" }

// Start 实现 app.Component：阻塞服务，直到 Stop 或某一路出错。
// 停机期各 Serve 的正常收尾由 closedOnStop 过滤，不上报给 app。
func (s *Server) Start(context.Context) error {
	switch {
	case s.grpc == nil: // 直连 REST 形态：只有 HTTP 一路
		return closedOnStop(s.http.Serve(s.httpLn))
	case s.http == nil: // 纯 gRPC 形态：只有 gRPC 一路
		return closedOnStop(s.grpc.Serve(s.grpcLn))
	}

	// 双端口形态：两条 Serve 各自阻塞在自己的 listener 上；缓冲等于路数，
	// 故 Start 因某一路出错提前返回时，另一路的写入也不会阻塞在这里。
	errs := make(chan error, 2)
	go func() { errs <- s.grpc.Serve(s.grpcLn) }()
	go func() { errs <- s.http.Serve(s.httpLn) }()
	for range cap(errs) {
		if err := closedOnStop(<-errs); err != nil {
			return err
		}
	}
	return nil
}

// Stop 实现 app.Component：按依赖方向排空在途流量。双端口形态顺序不可换——HTTP
// 请求经环回打到本进程 gRPC，先关环回或先停 gRPC 都会打断在途请求。
// 各 Server 的 Shutdown/GracefulStop 会关掉自己的 listener，新连接随之被拒。
func (s *Server) Stop(ctx context.Context) error {
	var errs []error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("停止 HTTP: %w", err))
		}
		if s.conn != nil {
			if err := s.conn.Close(); err != nil {
				errs = append(errs, fmt.Errorf("关闭 gateway 环回连接: %w", err))
			}
		}
	}
	if s.grpc != nil {
		if err := s.stopGRPC(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopGRPC 宽限期内等在途 RPC 收尾，耗尽则强制断开——不肯返回的 GracefulStop
// 会拖死整个停机序列。
func (s *Server) stopGRPC(ctx context.Context) error {
	stopped := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		s.grpc.Stop()
		<-stopped
		return fmt.Errorf("停止 gRPC: 宽限期耗尽，已强制断开: %w", ctx.Err())
	}
}

// closedOnStop 过滤停机期各 Serve 的正常收尾错误：listener 被关闭、两个 Server
// 各自的「已关闭」哨兵——都是 Stop 的预期结果，不是故障。
func closedOnStop(err error) error {
	switch {
	case err == nil,
		errors.Is(err, grpc.ErrServerStopped),
		errors.Is(err, http.ErrServerClosed),
		errors.Is(err, net.ErrClosed):
		return nil
	}
	return err
}

// loopbackAddr 环回拨号地址（127.0.0.1:port）。监听 ":0" 随机端口的场景也能拿到真实端口。
func loopbackAddr(ln net.Listener) string {
	if t, ok := ln.Addr().(*net.TCPAddr); ok {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(t.Port))
	}
	return ln.Addr().String()
}
