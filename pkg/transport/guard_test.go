package transport

// 出口层防护的回归网：panic 兜底、请求体上限、处理超时、停机期对在途请求的通知。
// 这几条都是「平时看不出、出事才知道没有」的东西，故各自钉一条用例。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const panicText = "probe: handler 里的空指针"

// panicDesc 一个必然 panic 的 gRPC 方法。
var panicDesc = grpc.ServiceDesc{
	ServiceName: "test.v1.Panic",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Boom",
		Handler: func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := &grpc_health_v1.HealthCheckRequest{}
			if err := dec(in); err != nil {
				return nil, err
			}
			handler := func(context.Context, any) (any, error) { panic(panicText) }
			if interceptor == nil {
				return handler(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{FullMethod: "/test.v1.Panic/Boom"}, handler)
		},
	}},
	Metadata: "test/v1/panic.proto",
}

// TestGRPCPanicRecovered 钉住 gRPC 侧的 panic 兜底：没有它，一次 handler 里的
// 空指针就打死整个进程——两个端口上全部在途请求一起断，可重放的载荷等于一个
// 远程 kill 开关。
func TestGRPCPanicRecovered(t *testing.T) {
	var buf safeBuffer
	srv := NewRPCServer(Config{GRPCAddr: "127.0.0.1:0"},
		WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))),
		WithService(func(s *grpc.Server) { s.RegisterService(&panicDesc, nil) }))
	served := start(t, srv)
	conn := dial(t, srv)

	out := &grpc_health_v1.HealthCheckResponse{}
	err := conn.Invoke(context.Background(), "/test.v1.Panic/Boom",
		&grpc_health_v1.HealthCheckRequest{}, out)
	if err == nil {
		t.Fatal("panic 的方法应返回错误")
	}

	// 进程还活着：同一连接上的后续调用照常得到响应。
	if got := checkHealth(t, conn); got != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Errorf("panic 之后服务应仍在服务，health got %v", got)
	}
	// panic 文本不出客户端，码是通用内部错误。
	st := status.Convert(err)
	if strings.Contains(st.Message(), panicText) {
		t.Errorf("panic 文本不应出客户端: %q", st.Message())
	}
	if st.Code() != codes.Internal {
		t.Errorf("panic 应翻成 Internal，got %v", st.Code())
	}
	// 堆栈进结构化日志，且访问日志记得到这次失败（recovery 在日志之内）。
	logged := buf.String()
	if !strings.Contains(logged, "handler panic") || !strings.Contains(logged, "stack") {
		t.Errorf("panic 应记结构化日志含堆栈，got %s", logged)
	}
	if !strings.Contains(logged, "/test.v1.Panic/Boom") {
		t.Errorf("访问日志应记到这次请求，got %s", logged)
	}

	stop(t, srv, served)
}

// guardGateway 注册验证防护用的直连路由。
func guardGateway(_ context.Context, mux *runtime.ServeMux) error {
	// panic 在写出响应之前：可以渲染统一错误体。
	if err := mux.HandlePath("GET", "/v1/panic",
		func(http.ResponseWriter, *http.Request, map[string]string) { panic(panicText) }); err != nil {
		return err
	}
	// panic 在写出响应之后：只能断连，不能再追加错误体。
	if err := mux.HandlePath("GET", "/v1/panic-after-write",
		func(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
			_, _ = io.WriteString(w, "partial")
			panic(panicText)
		}); err != nil {
		return err
	}
	// 读完请求体并回报字节数，用于验证体上限。
	if err := mux.HandlePath("POST", "/v1/echo-body",
		func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				_, outbound := runtime.MarshalerForRequest(mux, r)
				runtime.HTTPError(r.Context(), mux, outbound, w, r, err)
				return
			}
			_, _ = io.WriteString(w, strconv.FormatInt(n, 10))
		}); err != nil {
		return err
	}
	// 一直等到 ctx 结束，用于验证处理超时真的给请求 ctx 设了 deadline。
	return mux.HandlePath("GET", "/v1/slow",
		func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
			<-r.Context().Done()
			_, outbound := runtime.MarshalerForRequest(mux, r)
			runtime.HTTPError(r.Context(), mux, outbound, w, r, r.Context().Err())
		})
}

// TestDirectPanicRecovered 钉住直连形态的 panic 兜底。net/http 本来就会在连接层
// recover（进程不死），缺的是「故障看得见」：客户端原先拿到裸 EOF 而非 500，
// slog 里一条记录都没有，堆栈走标准 log 的 stderr 纯文本。
func TestDirectPanicRecovered(t *testing.T) {
	var buf safeBuffer
	srv := NewRESTServer(context.Background(), Config{HTTPAddr: "127.0.0.1:0"},
		WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))),
		WithDirectGateway(guardGateway))
	served := start(t, srv)
	base := "http://" + srv.httpLn.Addr().String()

	code, body := get(t, base+"/v1/panic")
	if code != http.StatusInternalServerError {
		t.Errorf("状态码 got %d, want 500", code)
	}
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("响应体应是统一错误体, got %q", body)
	}
	if payload.Code != 10003 {
		t.Errorf("错误码 got %d, want 10003", payload.Code)
	}
	if strings.Contains(payload.Message, panicText) {
		t.Errorf("panic 文本不应出客户端: %q", payload.Message)
	}

	logged := buf.String()
	if !strings.Contains(logged, "handler panic") || !strings.Contains(logged, "stack") {
		t.Errorf("panic 应记结构化日志含堆栈，got %s", logged)
	}
	// 访问日志记得到这次请求（recovery 在访问日志之内）。
	if !strings.Contains(logged, "/v1/panic") {
		t.Errorf("访问日志应记到这次请求，got %s", logged)
	}

	stop(t, srv, served)
}

// TestDirectPanicAfterWriteAborts 钉住「先写后崩」的处理：已经写出响应头之后不能
// 再渲染错误体——两段 JSON 拼在一起客户端解析不出来，而访问日志还会把它记成当初
// 那个成功状态码（错误被吞成成功，比不 recover 更糟）。此时断连是唯一诚实的信号。
func TestDirectPanicAfterWriteAborts(t *testing.T) {
	srv := NewRESTServer(context.Background(), Config{HTTPAddr: "127.0.0.1:0"},
		WithDirectGateway(guardGateway))
	served := start(t, srv)

	resp, err := (&http.Client{Timeout: 5 * time.Second}).
		Get("http://" + srv.httpLn.Addr().String() + "/v1/panic-after-write")
	if err == nil {
		defer resp.Body.Close()
		if _, readErr := io.ReadAll(resp.Body); readErr == nil {
			t.Error("先写后崩应当断连，客户端不该读到完整响应")
		}
	}

	stop(t, srv, served)
}

// TestRequestBodyLimit 钉住请求体上限：不设的话几个并发的大报文就能把进程打到
// OOM，而 gRPC 端口有 grpc-go 的默认值兜底、HTTP 端口原先什么都没有。
func TestRequestBodyLimit(t *testing.T) {
	const limit = 1024
	srv := NewRESTServer(context.Background(),
		Config{HTTPAddr: "127.0.0.1:0", MaxRequestBytes: limit},
		WithDirectGateway(guardGateway))
	served := start(t, srv)
	base := "http://" + srv.httpLn.Addr().String() + "/v1/echo-body"
	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("上限之内照常读完", func(t *testing.T) {
		resp, err := client.Post(base, "application/octet-stream", strings.NewReader(strings.Repeat("x", limit)))
		if err != nil {
			t.Fatalf("请求: %v", err)
		}
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		if string(got) != strconv.Itoa(limit) {
			t.Errorf("读到的字节数 got %s, want %d", got, limit)
		}
	})

	t.Run("超限即报错", func(t *testing.T) {
		resp, err := client.Post(base, "application/octet-stream", strings.NewReader(strings.Repeat("x", limit*4)))
		if err != nil {
			return // 连接被切也算挡住了
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("超限请求不应成功, got %d %q", resp.StatusCode, body)
		}
	})

	stop(t, srv, served)
}

// TestHandlerTimeout 钉住处理超时真的落到请求 ctx 上：客户端不传 deadline 时，
// 没有它 handler 可以一直跑下去，下游卡住时请求无限堆积、没有自然泄压点。
func TestHandlerTimeout(t *testing.T) {
	srv := NewRESTServer(context.Background(),
		Config{HTTPAddr: "127.0.0.1:0", HandlerTimeout: 50 * time.Millisecond},
		WithDirectGateway(guardGateway))
	served := start(t, srv)

	begin := time.Now()
	code, body := get(t, "http://"+srv.httpLn.Addr().String()+"/v1/slow")
	elapsed := time.Since(begin)

	if elapsed > 3*time.Second {
		t.Errorf("handler 应在超时后很快返回，实际耗时 %v", elapsed)
	}
	if code == http.StatusOK {
		t.Errorf("超时的请求不应成功, got %d %q", code, body)
	}
	// 超时走通用码 10005，客户端据此决定是否重试。
	var payload struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("响应体应是统一错误体, got %q", body)
	}
	if payload.Code != 10005 {
		t.Errorf("超时错误码 got %d, want 10005", payload.Code)
	}

	stop(t, srv, served)
}

// TestHandlerTimeoutDisabled 负值即不限制：流式转译与长轮询类出口需要它。
func TestHandlerTimeoutDisabled(t *testing.T) {
	cfg := Config{HandlerTimeout: -1}
	if got := cfg.handlerTimeout(); got != 0 {
		t.Errorf("负值应表示不限制（0），got %v", got)
	}
	if got := (Config{}).handlerTimeout(); got != defaultHandlerTimeout {
		t.Errorf("零值应取缺省，got %v", got)
	}
	if got := (Config{MaxRequestBytes: -1}).maxRequestBytes(); got != 0 {
		t.Errorf("体上限负值应表示不限制（0），got %v", got)
	}
	if got := (Config{}).maxRequestBytes(); got != defaultMaxRequestBytes {
		t.Errorf("体上限零值应取缺省，got %v", got)
	}
}

// TestStopNotifiesInflightHandlers 钉住停机宽限期耗尽后在途 handler 收到取消信号。
// 没有这条通路时，Shutdown 超时返回后 handler 仍在跑、还能写出 200，直到进程退出
// 时在任意一行被硬切——「已扣款未记账」正是这么来的。
func TestStopNotifiesInflightHandlers(t *testing.T) {
	inflight := make(chan struct{})
	canceled := make(chan struct{})
	srv := NewRESTServer(context.Background(), Config{HTTPAddr: "127.0.0.1:0", HandlerTimeout: -1},
		WithDirectGateway(func(_ context.Context, mux *runtime.ServeMux) error {
			return mux.HandlePath("GET", "/v1/hold",
				func(w http.ResponseWriter, r *http.Request, _ map[string]string) {
					close(inflight)
					<-r.Context().Done()
					close(canceled)
					_, _ = io.WriteString(w, "done")
				})
		}))
	served := start(t, srv)

	go func() {
		_, _ = (&http.Client{Timeout: 10 * time.Second}).
			Get("http://" + srv.httpLn.Addr().String() + "/v1/hold")
	}()
	<-inflight // 请求已进 handler

	// 宽限期给得极短：Shutdown 会超时返回，此时应通知在途 handler。
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := srv.Stop(ctx); err == nil {
		t.Error("在途请求未完成时 Stop 应报宽限期耗尽")
	}

	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Error("宽限期耗尽后在途 handler 应收到 ctx 取消")
	}

	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Error("Start 未在 3s 内返回")
	}
}

// safeBuffer 并发安全的日志缓冲：日志在 handler goroutine 写、断言在测试
// goroutine 读，裸 bytes.Buffer 在 -race 下是数据竞争。
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
