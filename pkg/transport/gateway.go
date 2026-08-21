package transport

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// GatewayRegistrar 是 protoc-gen-grpc-gateway 生成的 RegisterXxxHandler 的签名，
// 调用方把生成函数原样填进 Config.RegisterGateway 即可。
type GatewayRegistrar func(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error

// loopbackConn 是 gateway 到本进程 gRPC 端口的环回连接，属资源型组件（无常驻循环）。
//
// 为什么走环回而不用 RegisterXxxHandlerServer 进程内直连：直连会绕过整条 gRPC 拦截器链，
// 鉴权、日志就得在 HTTP 侧再实现一遍。环回让横切关注点只写一次。
type loopbackConn struct {
	conn *grpc.ClientConn
}

// mustLoopbackConn 建立到本进程 gRPC 端口的连接。
func mustLoopbackConn(cfg Config, grpcLn net.Listener) *loopbackConn {
	// passthrough 跳过 DNS 解析——目标是 IP 字面量，没必要走解析器。
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	// 环回这一跳也要埋点，否则 HTTP 请求的链路会在进入 gRPC 之前断掉。
	if cfg.Telemetry != nil {
		opts = append(opts, grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(cfg.Telemetry.TracerProvider()),
			otelgrpc.WithMeterProvider(cfg.Telemetry.MeterProvider()),
			otelgrpc.WithPropagators(cfg.Telemetry.Propagator()),
		)))
	}

	conn, err := grpc.NewClient("passthrough:///"+loopbackTarget(grpcLn.Addr()), opts...)
	if err != nil {
		panic(fmt.Errorf("装配 Transport: 建立 gateway 环回连接: %w", err))
	}
	return &loopbackConn{conn: conn}
}

// loopbackTarget 由 gRPC 监听地址推出环回拨号目标。
// 监听 :9090 时 Addr() 给的是 [::]:9090，通配地址不能直接拨，须换成回环地址；
// 绑到具体 IP 时则沿用该 IP，否则环回会打到一个根本没在监听的地址上。
func loopbackTarget(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return addr.String()
	}
	ip := tcp.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(tcp.Port))
}

// Name 实现 app.Component。
func (c *loopbackConn) Name() string { return "gateway-conn" }

// Start 实现 app.Component。资源型组件没有常驻循环，直接返回。
func (c *loopbackConn) Start(context.Context) error { return nil }

// Stop 关闭环回连接。它排在 HTTP 之后、gRPC 之前停：HTTP 已不再接新请求，
// 关掉它才不会在 gRPC GracefulStop 期间还有新的环回调用打进去。
func (c *loopbackConn) Stop(context.Context) error { return c.conn.Close() }
