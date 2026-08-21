package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/google/go-cmp/cmp"

	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/telemetry"

	"github.com/yanking/go-skeleton/pkg/transport"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func TestMustNewPanicsOnInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  transport.Config
	}{
		{name: "Service 为空", cfg: transport.Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"}},
		{name: "GRPCAddr 为空", cfg: transport.Config{Service: "user", HTTPAddr: "127.0.0.1:0"}},
		{name: "HTTPAddr 为空", cfg: transport.Config{Service: "user", GRPCAddr: "127.0.0.1:0"}},
		{name: "地址占不到端口", cfg: transport.Config{Service: "user", GRPCAddr: "无效地址", HTTPAddr: "127.0.0.1:0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("want panic, got 正常返回")
				}
			}()
			tt.cfg.Logger = discardLogger()
			transport.MustNew(context.Background(), tt.cfg)
		})
	}
}

// runTransport 用 pkg/app 真实拉起传输层——这既是被测目标，也顺带验证组件契约。
// 返回的 stop 触发停机并等 Run 返回。
func runTransport(t *testing.T, tp *transport.Transport) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := app.New(app.Config{Logger: discardLogger(), StopTimeout: 5 * time.Second}, tp.Components()...)

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("app.Run 返回错误: %v", err)
		}
	}
}

// newTransport 造一个监听随机端口的 Transport，cfg 中未填的必填项由此补齐。
func newTransport(t *testing.T, cfg transport.Config) *transport.Transport {
	t.Helper()
	cfg.Service = "user"
	cfg.GRPCAddr = "127.0.0.1:0"
	cfg.HTTPAddr = "127.0.0.1:0"
	if cfg.Logger == nil {
		cfg.Logger = discardLogger()
	}
	return transport.MustNew(context.Background(), cfg)
}

func dialGRPC(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("连接 gRPC %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestGRPCServesHealthCheck(t *testing.T) {
	tp := newTransport(t, transport.Config{})
	defer runTransport(t, tp)()

	resp, err := grpc_health_v1.NewHealthClient(dialGRPC(t, tp.GRPCAddr())).
		Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Health/Check 失败: %v", err)
	}
	if got, want := resp.GetStatus(), grpc_health_v1.HealthCheckResponse_SERVING; got != want {
		t.Errorf("健康状态 got %v, want %v", got, want)
	}
}

// httpGet 发一个 GET 并返回状态码与响应体。
func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	// 端口在装配期就已 bind，没人 accept 时连接会挂住，故必须有超时。
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestHTTPServesHealthz(t *testing.T) {
	tp := newTransport(t, transport.Config{})
	defer runTransport(t, tp)()

	code, _ := httpGet(t, "http://"+tp.HTTPAddr()+"/healthz")
	if want := http.StatusOK; code != want {
		t.Errorf("GET /healthz 状态码 got %d, want %d", code, want)
	}
}

func TestGatewayLoopbackReachesGRPC(t *testing.T) {
	// 往 gateway mux 上挂一条自定义路由，经环回 ClientConn 打到本进程的 gRPC health。
	// 这一条证明双协议真的接上了，而不是两个端口各干各的。
	tp := newTransport(t, transport.Config{
		RegisterGateway: []transport.GatewayRegistrar{
			func(_ context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
				return mux.HandlePath(http.MethodGet, "/loopback",
					func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
						resp, err := grpc_health_v1.NewHealthClient(conn).
							Check(r.Context(), &grpc_health_v1.HealthCheckRequest{})
						if err != nil {
							http.Error(w, err.Error(), http.StatusInternalServerError)
							return
						}
						_, _ = io.WriteString(w, resp.GetStatus().String())
					})
			},
		},
	})
	defer runTransport(t, tp)()

	code, body := httpGet(t, "http://"+tp.HTTPAddr()+"/loopback")
	if code != http.StatusOK || body != "SERVING" {
		t.Errorf("环回请求 got %d %q, want 200 %q", code, body, "SERVING")
	}
}

// panicMethod 是「/test.Panic/Boom」的方法描述，形状照 protoc-gen-go-grpc 生成的代码写：
// 先解码、再把真正的业务函数交给拦截器。这样 panic 才发生在拦截器链内部，
// 也才能验证 recovery 真的挡在了正确的位置。
var panicMethod = grpc.ServiceDesc{
	ServiceName: "test.Panic",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Boom",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(grpc_health_v1.HealthCheckRequest)
			if err := dec(in); err != nil {
				return nil, err
			}
			boom := func(context.Context, any) (any, error) { panic("测试用的 panic") }
			if interceptor == nil {
				return boom(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Panic/Boom"}, boom)
		},
	}},
}

func TestRecoveryTurnsPanicIntoInternal(t *testing.T) {
	tp := newTransport(t, transport.Config{
		RegisterGRPC: func(s *grpc.Server) { s.RegisterService(&panicMethod, nil) },
	})
	defer runTransport(t, tp)()

	conn := dialGRPC(t, tp.GRPCAddr())
	err := conn.Invoke(context.Background(), "/test.Panic/Boom",
		&grpc_health_v1.HealthCheckRequest{}, &grpc_health_v1.HealthCheckResponse{})

	if got, want := status.Code(err), codes.Internal; got != want {
		t.Errorf("panic 应转成 %v，got %v (err=%v)", want, got, err)
	}
	// 进程还得活着：紧接着的正常请求必须照常工作。
	if _, err := grpc_health_v1.NewHealthClient(conn).
		Check(context.Background(), &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Errorf("panic 之后服务应仍可用, got %v", err)
	}
}

// records 解析缓冲区里的 JSON 日志行。
func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("解析日志行 %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

// findRPCLog 找出拦截器打的那条 RPC 日志。
func findRPCLog(t *testing.T, buf *bytes.Buffer, method string) map[string]any {
	t.Helper()
	for _, record := range records(t, buf) {
		if record["msg"] == "RPC 完成" && record["method"] == method {
			return record
		}
	}
	t.Fatalf("未找到 %s 的 RPC 日志，实际日志:\n%s", method, buf.String())
	return nil
}

func TestLoggingInterceptorRecordsFailedRPC(t *testing.T) {
	var buf bytes.Buffer
	tp := newTransport(t, transport.Config{
		Logger:       slog.New(slog.NewJSONHandler(&buf, nil)),
		RegisterGRPC: func(s *grpc.Server) { s.RegisterService(&panicMethod, nil) },
	})
	defer runTransport(t, tp)()

	conn := dialGRPC(t, tp.GRPCAddr())
	_ = conn.Invoke(context.Background(), "/test.Panic/Boom",
		&grpc_health_v1.HealthCheckRequest{}, &grpc_health_v1.HealthCheckResponse{})

	got := findRPCLog(t, &buf, "/test.Panic/Boom")
	if want := "ERROR"; got["level"] != want {
		t.Errorf("Internal 错误应打 %s 级, got %v", want, got["level"])
	}
	if want := "Internal"; got["code"] != want {
		t.Errorf("code got %v, want %v", got["code"], want)
	}
	if _, ok := got["duration_ms"]; !ok {
		t.Errorf("RPC 日志缺少 duration_ms 字段, got %v", got)
	}
}

func TestHealthCheckLoggedAtDebug(t *testing.T) {
	// 探针每几秒来一次，打 Info 会把真实日志淹没，故降到 Debug。
	var buf bytes.Buffer
	tp := newTransport(t, transport.Config{
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)), // 默认 Info 级，Debug 不会输出
	})
	defer runTransport(t, tp)()

	if _, err := grpc_health_v1.NewHealthClient(dialGRPC(t, tp.GRPCAddr())).
		Check(context.Background(), &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Fatalf("Health/Check 失败: %v", err)
	}

	for _, record := range records(t, &buf) {
		if record["msg"] == "RPC 完成" {
			t.Errorf("健康检查不该在 Info 级留下 RPC 日志, got %v", record)
		}
	}
}

func TestServiceInterceptorsAreInvoked(t *testing.T) {
	calls := 0
	tp := newTransport(t, transport.Config{
		Interceptors: []grpc.UnaryServerInterceptor{
			func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
				calls++
				return handler(ctx, req)
			},
		},
	})
	defer runTransport(t, tp)()

	if _, err := grpc_health_v1.NewHealthClient(dialGRPC(t, tp.GRPCAddr())).
		Check(context.Background(), &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Fatalf("Health/Check 失败: %v", err)
	}
	if calls != 1 {
		t.Errorf("服务自有拦截器应被调用一次, got %d", calls)
	}
}

func TestTelemetryProducesServerSpan(t *testing.T) {
	var spans bytes.Buffer
	tel := telemetry.MustNew(context.Background(), telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterStdout,
		Writer:   &spans,
		Logger:   discardLogger(),
	})
	// pkg/telemetry.Telemetry 靠结构化接口满足 transport.Telemetry，
	// 所以 pkg/transport 不 import pkg/telemetry。
	tp := newTransport(t, transport.Config{Telemetry: tel})
	stop := runTransport(t, tp)

	if _, err := grpc_health_v1.NewHealthClient(dialGRPC(t, tp.GRPCAddr())).
		Check(context.Background(), &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Fatalf("Health/Check 失败: %v", err)
	}

	stop()
	if err := tel.Stop(context.Background()); err != nil {
		t.Fatalf("关闭 telemetry: %v", err)
	}

	if got := spans.String(); !strings.Contains(got, "grpc.health.v1.Health/Check") {
		t.Errorf("未见 gRPC 服务端 span\n实际输出:\n%s", got)
	}
}

// slowStarted 在 slowMethod 的 handler 真正开始执行时收到通知。
var slowStarted = make(chan struct{}, 1)

// slowMethod 是「/test.Slow/Wait」，handler 里睡 300ms，用于验证停机时在途请求不被切断。
var slowMethod = grpc.ServiceDesc{
	ServiceName: "test.Slow",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Wait",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(grpc_health_v1.HealthCheckRequest)
			if err := dec(in); err != nil {
				return nil, err
			}
			wait := func(context.Context, any) (any, error) {
				slowStarted <- struct{}{}
				time.Sleep(300 * time.Millisecond)
				return &grpc_health_v1.HealthCheckResponse{
					Status: grpc_health_v1.HealthCheckResponse_SERVING,
				}, nil
			}
			if interceptor == nil {
				return wait(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Slow/Wait"}, wait)
		},
	}},
}

func TestGracefulStopLetsInflightRPCFinish(t *testing.T) {
	tp := newTransport(t, transport.Config{
		RegisterGRPC: func(s *grpc.Server) { s.RegisterService(&slowMethod, nil) },
	})
	stop := runTransport(t, tp)

	conn := dialGRPC(t, tp.GRPCAddr())
	result := make(chan error, 1)
	go func() {
		result <- conn.Invoke(context.Background(), "/test.Slow/Wait",
			&grpc_health_v1.HealthCheckRequest{}, new(grpc_health_v1.HealthCheckResponse))
	}()

	<-slowStarted // 确保请求已经进到 handler 里，再触发停机
	stop()

	if err := <-result; err != nil {
		t.Errorf("停机期间的在途请求应正常完成, got %v", err)
	}
}

func TestComponentsOrder(t *testing.T) {
	// 逆序即停机顺序：停 HTTP → 关环回 ClientConn → GracefulStop gRPC。
	// 顺序由本包排好，调用方 append 进 app 即可，没有排错的机会。
	tp := newTransport(t, transport.Config{})
	var got []string
	for _, c := range tp.Components() {
		got = append(got, c.Name())
	}
	want := []string{"grpc", "gateway-conn", "http"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("组件顺序不符 (-want +got):\n%s", diff)
	}
}

func TestShutdownOrderKeepsInflightHTTPAlive(t *testing.T) {
	// 停机顺序的真正意义：HTTP 先停（Shutdown 会等在途请求跑完），
	// 之后才关环回 ClientConn。顺序若反过来，环回先断，正在经 gateway 转发的
	// HTTP 请求会当场拿到 connection closed——这条用行为而非名字来锁住它。
	tp := newTransport(t, transport.Config{
		RegisterGRPC: func(s *grpc.Server) { s.RegisterService(&slowMethod, nil) },
		RegisterGateway: []transport.GatewayRegistrar{
			func(_ context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
				return mux.HandlePath(http.MethodGet, "/slow",
					func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
						resp := new(grpc_health_v1.HealthCheckResponse)
						if err := conn.Invoke(r.Context(), "/test.Slow/Wait",
							&grpc_health_v1.HealthCheckRequest{}, resp); err != nil {
							http.Error(w, err.Error(), http.StatusInternalServerError)
							return
						}
						_, _ = io.WriteString(w, resp.GetStatus().String())
					})
			},
		},
	})
	stop := runTransport(t, tp)

	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() {
		code, body := httpGet(t, "http://"+tp.HTTPAddr()+"/slow")
		done <- result{code, body}
	}()

	<-slowStarted // 请求已穿过 HTTP → 环回 → gRPC，此刻触发停机
	stop()

	got := <-done
	if got.code != http.StatusOK || got.body != "SERVING" {
		t.Errorf("停机期间在途的 HTTP 请求应正常完成, got %d %q", got.code, got.body)
	}
}
