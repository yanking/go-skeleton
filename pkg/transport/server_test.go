package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/yanking/go-skeleton/pkg/errcode"
)

func TestNewServerPanics(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		opts []Option
	}{
		{name: "grpc_addr 为空", cfg: Config{}},
		{
			name: "只配 http_addr 没有 grpc",
			cfg:  Config{HTTPAddr: "127.0.0.1:0"},
			opts: []Option{WithGateway(noopGateway)},
		},
		{
			name: "配了 http_addr 却没有 gateway",
			cfg:  Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"},
		},
		{
			name: "挂载点没有 HTTP 出口",
			cfg:  Config{GRPCAddr: "127.0.0.1:0"},
			opts: []Option{WithMount("/docs", http.NewServeMux())},
		},
		{
			name: "挂载模式占用根路径",
			cfg:  Config{GRPCAddr: "127.0.0.1:0"},
			opts: []Option{WithGateway(noopGateway), WithMount("/", http.NewServeMux())},
		},
		{
			name: "挂载模式不以 / 开头",
			cfg:  Config{GRPCAddr: "127.0.0.1:0"},
			opts: []Option{WithGateway(noopGateway), WithMount("docs", http.NewServeMux())},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("want panic, got 正常返回")
				}
			}()
			NewServer(context.Background(), tt.cfg, tt.opts...)
		})
	}
}

// 纯 gRPC 形态：不配 http_addr 即不监听 HTTP，只有 gRPC 一个端口。
func TestServerGRPCOnly(t *testing.T) {
	srv := NewServer(context.Background(), Config{GRPCAddr: "127.0.0.1:0"})
	if srv.http != nil || srv.httpLn != nil || srv.conn != nil {
		t.Error("未配 http_addr 不应装配 HTTP 出口")
	}
	if name := srv.Name(); name != "transport" {
		t.Errorf("Name got %q, want %q", name, "transport")
	}

	served := start(t, srv)
	if got := checkHealth(t, dial(t, srv)); got != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("health 状态 got %v, want SERVING", got)
	}
	stop(t, srv, served)
}

// 双协议形态：gRPC 与 HTTP 各自监听一个端口，HTTP 侧只是 gateway 代理——请求经环回
// 打回本进程 gRPC，因此两种协议穿过同一条拦截链、共用同一套错误出口。
func TestServerDualProtocol(t *testing.T) {
	docs := http.NewServeMux()
	docs.HandleFunc("/docs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "docs-root")
	})
	docs.HandleFunc("/docs/spec.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "docs-subtree")
	})

	srv := NewServer(context.Background(), Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"},
		WithService(registerEcho),
		WithGateway(echoGateway),
		WithMount("/docs", docs),
	)
	served := start(t, srv)
	defer stop(t, srv, served)

	// gRPC 端口一路。
	if got := checkHealth(t, dial(t, srv)); got != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("gRPC health 状态 got %v, want SERVING", got)
	}

	base := "http://" + srv.httpLn.Addr().String()
	tests := []struct {
		name, path, wantBody string
		wantStatus           int
	}{
		{name: "探针", path: "/healthz", wantStatus: http.StatusOK},
		{name: "挂载点精确模式", path: "/docs", wantStatus: http.StatusOK, wantBody: "docs-root"},
		{name: "挂载点子树模式", path: "/docs/spec.json", wantStatus: http.StatusOK, wantBody: "docs-subtree"},
		// 走 gateway 路由 → 环回连接 → 本进程 gRPC → echo 服务，整条链路真实往返。
		{name: "gateway 经环回打回本进程 gRPC", path: "/v1/ping", wantStatus: http.StatusOK, wantBody: "SERVING"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := get(t, base+tt.path)
			if code != tt.wantStatus {
				t.Errorf("状态 got %d, want %d (body %q)", code, tt.wantStatus, body)
			}
			if tt.wantBody != "" && !strings.Contains(body, tt.wantBody) {
				t.Errorf("body got %q, want 含 %q", body, tt.wantBody)
			}
		})
	}

	// errcode 的 HTTP 侧出口：业务码与消息进 JSON，HTTP 状态按 gRPC 码映射，
	// 原始 cause（"底层连接失败"）绝不外泄。
	t.Run("errcode 经 gateway 渲染为统一 JSON", func(t *testing.T) {
		code, body := get(t, base+"/v1/fail")
		if code != http.StatusNotFound {
			t.Errorf("状态 got %d, want 404 (body %q)", code, body)
		}
		var got struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("body 非 JSON: %q", body)
		}
		if got.Code != errcode.ErrNotFound.Code || got.Message != errcode.ErrNotFound.Message {
			t.Errorf("JSON got %+v, want code=%d message=%q",
				got, errcode.ErrNotFound.Code, errcode.ErrNotFound.Message)
		}
		if strings.Contains(body, cause) {
			t.Errorf("原始错误不得外泄, got %q", body)
		}
	})
}

// 鉴权对两种协议同时生效：HTTP 的 Authorization 头经 gateway 转成 metadata，
// 与原生 gRPC 调用穿过同一个拦截器；探针与 reflection 恒定放行。
func TestServerAuthCoversBothProtocols(t *testing.T) {
	srv := NewServer(context.Background(), Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0"},
		WithAuthenticator(FixedBearerAuth("tok")),
		WithService(registerEcho),
		WithGateway(echoGateway),
	)
	served := start(t, srv)
	defer stop(t, srv, served)

	// 探针带不上凭证，拦它等于让编排系统误判实例死亡。
	if got := checkHealth(t, dial(t, srv)); got != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("探针应免鉴权, got %v", got)
	}

	t.Run("gRPC 业务方法凭证错误被拒", func(t *testing.T) {
		err := dial(t, srv).Invoke(context.Background(), echoPing,
			&grpc_health_v1.HealthCheckRequest{}, &grpc_health_v1.HealthCheckResponse{})
		if status.Code(err) != errcode.ErrUnauthenticated.Status {
			t.Errorf("got %v, want %v", status.Code(err), errcode.ErrUnauthenticated.Status)
		}
	})

	base := "http://" + srv.httpLn.Addr().String()
	t.Run("HTTP 带对凭证放行", func(t *testing.T) {
		if code, body := getWithAuth(t, base+"/v1/ping", "Bearer tok"); code != http.StatusOK {
			t.Errorf("got %d %q, want 200", code, body)
		}
	})
	t.Run("HTTP 凭证错误被拒且带业务码", func(t *testing.T) {
		code, body := getWithAuth(t, base+"/v1/ping", "Bearer wrong")
		if code != http.StatusUnauthorized {
			t.Errorf("got %d %q, want 401", code, body)
		}
		if !strings.Contains(body, fmt.Sprint(errcode.ErrUnauthenticated.Code)) {
			t.Errorf("body 应含业务码 %d, got %q", errcode.ErrUnauthenticated.Code, body)
		}
	})
}

func TestClosedOnStop(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "无错误", err: nil},
		{name: "gRPC 已停止", err: grpc.ErrServerStopped},
		{name: "HTTP 已关闭", err: http.ErrServerClosed},
		{name: "listener 已关闭", err: fmt.Errorf("accept tcp: %w", net.ErrClosed)},
		{name: "真实故障原样上报", err: boom, want: boom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := closedOnStop(tt.err); !errors.Is(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// ---- 测试替身：手写一个最小 gRPC 服务，只为在 pkg 内造出「非探针」的业务方法 ----
// 未注册的方法不会经过拦截器（grpc-go 直接回 Unimplemented），故鉴权与错误出口
// 这类拦截链行为必须挂在真实注册的方法上才测得到。用 health 的 pb 类型当载荷，
// 不引入任何业务 pb——pkg 不允许出现业务概念。

const (
	echoPing = "/test.v1.Echo/Ping"
	echoFail = "/test.v1.Echo/Fail"
	cause    = "底层连接失败"
)

var echoDesc = grpc.ServiceDesc{
	ServiceName: "test.v1.Echo",
	HandlerType: (*any)(nil), // 实现传 nil，RegisterService 因此跳过类型检查
	Methods: []grpc.MethodDesc{
		{MethodName: "Ping", Handler: echoHandler(echoPing, nil)},
		{MethodName: "Fail", Handler: echoHandler(echoFail, errcode.Wrap(errors.New(cause), errcode.ErrNotFound))},
	},
	Metadata: "test/v1/echo.proto",
}

// echoHandler 生成一个固定行为的方法处理器：ret 为 nil 时回 SERVING，否则回该错误。
func echoHandler(fullMethod string, ret error) func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
	return func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := &grpc_health_v1.HealthCheckRequest{}
		if err := dec(in); err != nil {
			return nil, err
		}
		handler := func(context.Context, any) (any, error) {
			if ret != nil {
				return nil, ret
			}
			return &grpc_health_v1.HealthCheckResponse{
				Status: grpc_health_v1.HealthCheckResponse_SERVING,
			}, nil
		}
		if interceptor == nil {
			return handler(ctx, in)
		}
		return interceptor(ctx, in, &grpc.UnaryServerInfo{FullMethod: fullMethod}, handler)
	}
}

func registerEcho(s *grpc.Server) { s.RegisterService(&echoDesc, nil) }

// noopGateway 不注册路由，只用于选项校验类用例。
func noopGateway(context.Context, *runtime.ServeMux, *grpc.ClientConn) error { return nil }

// echoGateway 手写两条等价于 pb 生成代码的路由：AnnotateContext 把 HTTP 头按
// incomingHeaderMatcher 转成 gRPC metadata（生成代码的必经一步，鉴权头靠它过桥），
// 调用走环回连接，错误交给 mux 配置的错误处理器——查找路径与生成代码完全一致。
func echoGateway(_ context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
	route := func(path, fullMethod string) error {
		return mux.HandlePath("GET", path, func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
			ctx, err := runtime.AnnotateContext(r.Context(), mux, r, fullMethod)
			if err != nil {
				runtime.HTTPError(r.Context(), mux, &runtime.JSONPb{}, w, r, err)
				return
			}
			out := &grpc_health_v1.HealthCheckResponse{}
			if err := conn.Invoke(ctx, fullMethod, &grpc_health_v1.HealthCheckRequest{}, out); err != nil {
				runtime.HTTPError(ctx, mux, &runtime.JSONPb{}, w, r, err)
				return
			}
			_, _ = io.WriteString(w, out.Status.String())
		})
	}
	if err := route("/v1/ping", echoPing); err != nil {
		return err
	}
	return route("/v1/fail", echoFail)
}

// ---- 测试工具 ----

func start(t *testing.T, srv *Server) <-chan error {
	t.Helper()
	served := make(chan error, 1)
	go func() { served <- srv.Start(context.Background()) }()
	return served
}

func stop(t *testing.T, srv *Server, served <-chan error) {
	t.Helper()
	if err := srv.Stop(context.Background()); err != nil {
		t.Errorf("Stop got %v, want nil", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Start 收尾异常, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Stop 之后 Start 未在 5s 内返回")
	}
}

func dial(t *testing.T, srv *Server) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(srv.grpcLn.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("拨号: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func checkHealth(t *testing.T, conn *grpc.ClientConn) grpc_health_v1.HealthCheckResponse_ServingStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health 查询: %v (status=%v)", err, status.Code(err))
	}
	return resp.Status
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	return getWithAuth(t, url, "")
}

func getWithAuth(t *testing.T, url, authorization string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应: %v", err)
	}
	return resp.StatusCode, strings.TrimSpace(string(body))
}
