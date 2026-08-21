package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"buf.build/go/protovalidate"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// mustValidator 造 protovalidate 校验器。它要编译 proto 里的 CEL 表达式，
// 编译不过说明注解写错了，属装配期错误，当场 panic。
func mustValidator() protovalidate.Validator {
	validator, err := protovalidate.New()
	if err != nil {
		panic(fmt.Errorf("装配 Transport: 构造参数校验器: %w", err))
	}
	return validator
}

// grpcServer 是 gRPC 服务端的 app.Component 适配器。
type grpcServer struct {
	srv    *grpc.Server
	ln     net.Listener
	logger *slog.Logger
}

// newGRPCServer 造 gRPC server：注册健康检查协议，再交给调用方注册业务实现。
func newGRPCServer(cfg Config, ln net.Listener, logger *slog.Logger) *grpcServer {
	// 日志必须在 recovery 外层。反过来的话，handler 的 panic 会从日志拦截器内部穿过去，
	// 它 handler() 之后的记录代码根本没机会执行——凡是 panic 的 RPC 都不会留下访问日志，
	// 错误率被静默低估。放在外层则 recovery 已把 panic 转成 Internal，日志照常记到。
	// 代价是日志拦截器自身的 panic 不被兜住，但那是本包自己的几行代码，不接触用户输入。
	interceptors := append([]grpc.UnaryServerInterceptor{
		loggingInterceptor(logger),
		recoveryInterceptor(logger),
	}, cfg.Interceptors...)
	// 参数校验排在链尾，最靠近 handler——鉴权等自有拦截器先跑完再校验。
	interceptors = append(interceptors, validateInterceptor(mustValidator()))

	opts := []grpc.ServerOption{grpc.ChainUnaryInterceptor(interceptors...)}
	// 观测不在拦截器链上：otelgrpc 是 stats.Handler，先于整条链触发，
	// 日志拦截器因此总能拿到已开启的 span。provider 显式注入，不依赖全局。
	if cfg.Telemetry != nil {
		opts = append(opts, grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(cfg.Telemetry.TracerProvider()),
			otelgrpc.WithMeterProvider(cfg.Telemetry.MeterProvider()),
			otelgrpc.WithPropagators(cfg.Telemetry.Propagator()),
		)))
	}

	srv := grpc.NewServer(opts...)

	// gRPC Health v1 复用 grpc-go 官方实现，不进 api/。整进程只报一个总状态。
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)

	if cfg.RegisterGRPC != nil {
		cfg.RegisterGRPC(srv)
	}
	return &grpcServer{srv: srv, ln: ln, logger: logger}
}

// Name 实现 app.Component。
func (s *grpcServer) Name() string { return "grpc" }

// Start 阻塞运行，直到 Stop 被调用或监听器出错。
func (s *grpcServer) Start(context.Context) error { return s.srv.Serve(s.ln) }

// Stop 优雅停机：先等在途 RPC 跑完，宽限期耗尽则强制切断。
// GracefulStop 自己不看 ctx，故必须在这里把它和 ctx 赛跑，否则一个挂住的流式 RPC
// 会让整个停机流程卡死，连带后面的组件全都停不掉。
func (s *grpcServer) Stop(ctx context.Context) error {
	stopped := make(chan struct{})
	go func() {
		s.srv.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		s.logger.Warn("gRPC 宽限期耗尽，强制终止在途连接", "component", s.Name())
		s.srv.Stop()
		<-stopped // Stop 会让 GracefulStop 立刻返回
		return nil
	}
}
