package transport

// 出口契约的回归网：非业务路径的错误体形状、凭证通道的唯一性、链路在 HTTP
// 边界的续传。三条都是「客户端或排障侧能观察到、服务端自己不报错」的东西。

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// 入站 trace 上下文的固定值，两个续传用例共用。
const (
	inboundTraceID = "0af7651916cd43dd8448eb211c80319c"
	inboundSpanID  = "b7ad6b7169203331"
)

// TestGRPCTraceContinuation 钉住 gRPC 服务端提取入站 traceparent。
//
// 这条曾经完全不工作，而且无声无息：otelgrpc 的默认传播器取自 otel 全局，本仓
// 刻意不设全局，那个默认值是空 composite——于是东西向调用链在每一个 gRPC 边界
// 都断成孤立的单服务 trace。span 照样产出、trace_id 照样生成，只是每跳都是新的，
// 两侧都不报错。
func TestGRPCTraceContinuation(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	srv := NewRPCServer(Config{GRPCAddr: "127.0.0.1:0", DrainDelay: -1},
		WithTracerProvider(tp), WithService(registerEcho))
	served := start(t, srv)

	// 手工注入 traceparent metadata，等价于上游 otelgrpc 客户端的行为。
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"traceparent", "00-"+inboundTraceID+"-"+inboundSpanID+"-01")
	out := &grpc_health_v1.HealthCheckResponse{}
	if err := dial(t, srv).Invoke(ctx, echoPing, &grpc_health_v1.HealthCheckRequest{}, out); err != nil {
		t.Fatalf("调用: %v", err)
	}

	stop(t, srv, served)

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("应有 span 产出")
	}
	for _, span := range spans {
		if got := span.SpanContext.TraceID().String(); got != inboundTraceID {
			t.Errorf("span %q 的 trace_id = %s，期望续上入站的 %s（链路在 gRPC 边界断了）",
				span.Name, got, inboundTraceID)
		}
	}
}

// TestRoutingErrorsCarryBusinessCode 钉住非业务路径的错误体：路由不存在、方法不
// 允许、请求解析失败原先一律落进 10003 内部错误，客户端按 code 分支时分不开
// 「路径写错了」与「服务端崩了」；405 的 HTTP 状态还会被 gRPC 码映射改写成 501。
func TestRoutingErrorsCarryBusinessCode(t *testing.T) {
	srv := NewRESTServer(context.Background(), Config{HTTPAddr: "127.0.0.1:0", DrainDelay: -1},
		WithDirectGateway(guardGateway))
	served := start(t, srv)
	base := "http://" + srv.httpLn.Addr().String()

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   int
	}{
		{name: "路径不存在", method: http.MethodGet, path: "/v1/nope", wantStatus: http.StatusNotFound, wantCode: 10002},
		{name: "方法不允许", method: http.MethodDelete, path: "/v1/panic", wantStatus: http.StatusMethodNotAllowed, wantCode: 10001},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, base+c.path, nil)
			if err != nil {
				t.Fatalf("构造请求: %v", err)
			}
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatalf("请求: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != c.wantStatus {
				t.Errorf("HTTP 状态 got %d, want %d", resp.StatusCode, c.wantStatus)
			}
			var payload struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatalf("响应体应是统一错误体: %v", err)
			}
			if payload.Code != c.wantCode {
				t.Errorf("业务码 got %d, want %d", payload.Code, c.wantCode)
			}
			if payload.Message == "" {
				t.Error("错误体应带中文消息")
			}
		})
	}

	stop(t, srv, served)
}

// TestCredentialSingleChannel 钉住凭证只有一条通道：gateway 的默认头映射会把
// Grpc-Metadata-Authorization 脱掉前缀变成 metadata 的 authorization 键，于是
// 客户端能绕过边缘网关清洗过的真 Authorization 头另送一份；两份并存时校验哪一份
// 还取决于 header map 的遍历顺序（实测 30 次里有 3 次用了伪造的那份）。
func TestCredentialSingleChannel(t *testing.T) {
	const token = "right-token"
	srv := NewRPCRESTServer(context.Background(),
		Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DrainDelay: -1},
		WithAuthenticator(FixedBearerAuth(token)),
		WithService(registerEcho),
		WithGateway(echoGateway))
	served := start(t, srv)
	base := "http://" + srv.httpLn.Addr().String()

	t.Run("真 Authorization 头照常生效", func(t *testing.T) {
		if code, body := getWithAuth(t, base+"/v1/ping", "Bearer "+token); code != http.StatusOK {
			t.Errorf("带正确凭证应放行, got %d %q", code, body)
		}
	})

	t.Run("Grpc-Metadata-Authorization 不再是凭证通道", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, base+"/v1/ping", nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		req.Header.Set("Grpc-Metadata-Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("请求: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			t.Error("Grpc-Metadata-Authorization 不应被当作凭证——那是绕过边缘网关清洗的第二条通道")
		}
	})

	stop(t, srv, served)
}

// TestLoopbackTraceContinuation 钉住环回形态的 HTTP 入口提取入站 traceparent：
// 没有它，外部调用方带着 trace 上下文打 HTTP 端口时本服务另起一条 trace，
// 跨服务链路在本服务的 HTTP 边界断开——而双端口形态正是推荐的对外 HTTP 形态。
func TestLoopbackTraceContinuation(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	srv := NewRPCRESTServer(context.Background(),
		Config{GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", DrainDelay: -1},
		WithTracerProvider(tp),
		WithService(registerEcho),
		WithGateway(echoGateway))
	served := start(t, srv)

	req, err := http.NewRequest(http.MethodGet,
		"http://"+srv.httpLn.Addr().String()+"/v1/ping", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("traceparent", "00-"+inboundTraceID+"-"+inboundSpanID+"-01")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	defer resp.Body.Close()

	stop(t, srv, served)

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("应有 span 产出")
	}
	for _, span := range spans {
		if got := span.SpanContext.TraceID().String(); got != inboundTraceID {
			t.Errorf("span %q 的 trace_id = %s，期望续上入站的 %s（链路在 HTTP 边界断了）",
				span.Name, got, inboundTraceID)
		}
	}
}
