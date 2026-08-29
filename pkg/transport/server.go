// Package transport 构造服务的对外传输组件 Server：装配期监听端口（起不来就死），
// 实现 app.Component（Start 阻塞服务、Stop 优雅停机），并承担错误处理约定的
// 「出口层」职责——把 errcode 的业务字段翻译为 gRPC status / HTTP 响应，
// 原始错误链留在服务端日志。
//
// gRPC 是唯一的业务协议，HTTP 侧只是 grpc-gateway 从同一份 proto 契约转译出的
// 代理出口：它把 HTTP/JSON 请求翻成 gRPC 调用，经环回连接打回本进程的 gRPC 端口，
// 因此拦截链（出口翻译、访问日志、鉴权、otel）对两种协议同样生效，业务代码只写一遍。
//
// 两个端口各自监听：gRPC 与 HTTP 的暴露范围、mesh 协议标注（Istio 的端口名 /
// appProtocol 是按端口生效的）、网络策略往往不同——常见形态就是 gRPC 只走东西向、
// HTTP 才对外。HTTP 端口配了才开，同一份二进制因此能按部署环境选择纯 gRPC 或双协议。
//
// 拦截链按注册顺序组装、先注册者在最外层，业务 handler 位于最内：errcode 出口翻译固定为
// 链首（仓库级约定，后续选项只能追加、顶不掉它）；其后是访问日志（WithLogger 才挂：成功
// Info、失败 Warn，记方法、来源地址、状态码与耗时，trace_id 由日志钩子附带）与鉴权
// （WithAuthenticator 才挂：health/reflection 前缀豁免——拦探针会让编排系统误判实例死亡；
// 策略由服务注入，FixedBearerAuth 做固定 token 比对、空 token 全拒绝），再后是
// WithUnaryInterceptor / WithStreamInterceptor 传入的自定义拦截器；otel 遥测另经
// StatsHandler 挂载（WithTracerProvider 才挂）。经 gateway 环回的 HTTP 流量穿过同一条链。
//
// 用法：
//
//	srv := transport.NewServer(ctx, c.Transport,
//	    transport.WithLogger(logger),
//	    transport.WithAuthenticator(transport.FixedBearerAuth(c.Auth.Token)),
//	    transport.WithService(func(s *grpc.Server) {
//	        xxxv1.RegisterXxxServiceServer(s, handler.NewGRPC(svc))
//	    }),
//	    transport.WithGateway(xxxv1.RegisterXxxServiceHandler), // 配了 http_addr 才生效
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

// Config 传输装配参数。
type Config struct {
	// GRPCAddr gRPC 监听地址（如 ":9090"），必填。
	GRPCAddr string `yaml:"grpc_addr"`
	// HTTPAddr gateway 转译出口的监听地址（如 ":8080"）。留空即不开 HTTP 出口，
	// 同一份二进制因此可按部署环境选择纯 gRPC 或双协议；配了它就必须有 WithGateway。
	HTTPAddr string `yaml:"http_addr"`
}

// Server 服务的对外传输出口，实现 app.Component。
type Server struct {
	grpc   *grpc.Server
	grpcLn net.Listener

	// 以下字段仅在开启 HTTP 出口时非 nil，nil 即纯 gRPC 形态。
	http   *http.Server
	httpLn net.Listener
	conn   *grpc.ClientConn
}

// NewServer 构造传输出口。此刻即「起不来就该死」的装配期：端口占用、gateway 路由
// 注册失败、配置与选项自相矛盾一律 panic。全部注册在返回前完成，调用方拿到的即可运行状态。
func NewServer(ctx context.Context, cfg Config, opts ...Option) *Server {
	o := newOptions(opts)
	validate(cfg, o)

	grpcLn, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 gRPC %s: %w", cfg.GRPCAddr, err))
	}

	s := &Server{grpc: newGRPCServer(o), grpcLn: grpcLn}
	for _, register := range o.services {
		register(s.grpc)
	}
	if cfg.HTTPAddr != "" {
		s.attachHTTP(ctx, cfg.HTTPAddr, o)
	}
	return s
}

// validate 把「装配起来必然出错」的组合挡在监听端口之前。
func validate(cfg Config, o *options) {
	switch {
	case cfg.GRPCAddr == "":
		panic(errors.New("装配 transport: grpc_addr 不能为空——HTTP 出口只是 gateway 代理，不能独立存在"))
	case cfg.HTTPAddr != "" && len(o.gateways) == 0:
		panic(errors.New("装配 transport: 配了 http_addr 却没有 WithGateway，HTTP 端口上不会有任何路由"))
	case len(o.mounts) > 0 && len(o.gateways) == 0:
		panic(errors.New("装配 transport: 挂载点需要 HTTP 出口，请一并 WithGateway"))
	}
	for _, m := range o.mounts {
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

// attachHTTP 装上 gateway 转译出口：独立监听、环回连接、路由树。
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, mt := range o.mounts {
		// 精确与子树两个模式都要注册，否则子路径会落进根路径的 gateway。
		pattern := strings.TrimSuffix(mt.pattern, "/")
		mux.Handle(pattern, mt.handler)
		mux.Handle(pattern+"/", mt.handler)
	}
	mux.Handle("/", gwMux)

	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}
}

// Name 实现 app.Component。
func (s *Server) Name() string { return "transport" }

// Start 实现 app.Component：阻塞服务，直到 Stop 或某一路出错。
// 停机期各 Serve 的正常收尾由 closedOnStop 过滤，不上报给 app。
func (s *Server) Start(context.Context) error {
	if s.http == nil {
		return closedOnStop(s.grpc.Serve(s.grpcLn))
	}

	// 两条 Serve 各自阻塞在自己的 listener 上；缓冲等于路数，
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

// Stop 实现 app.Component：按依赖方向排空在途流量。顺序不可换——HTTP 请求经环回
// 打到本进程 gRPC，先关环回或先停 gRPC 都会打断在途请求。
// 各 Server 的 Shutdown/GracefulStop 会关掉自己的 listener，新连接随之被拒。
func (s *Server) Stop(ctx context.Context) error {
	var errs []error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("停止 HTTP: %w", err))
		}
		if err := s.conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("关闭 gateway 环回连接: %w", err))
		}
	}
	if err := s.stopGRPC(ctx); err != nil {
		errs = append(errs, err)
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
