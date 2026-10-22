package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/codes"

	"github.com/yanking/go-skeleton/pkg/errcode"
)

func TestTranslate(t *testing.T) {
	business := errcode.New(40001, "参数不合法：金额须为正数", codes.InvalidArgument)
	tests := []struct {
		name       string
		err        error
		wantStatus int
		want       ErrorBody
	}{
		{name: "业务码原样输出", err: business, wantStatus: http.StatusBadRequest, want: ErrorBody{40001, "参数不合法：金额须为正数"}},
		{name: "Wrap 过的业务码", err: errcode.Wrap(errors.New("底层原因"), business), wantStatus: http.StatusBadRequest, want: ErrorBody{40001, "参数不合法：金额须为正数"}},
		{name: "未认证是 401", err: errcode.ErrUnauthenticated, wantStatus: http.StatusUnauthorized, want: ErrorBody{10004, "未认证"}},
		{name: "故障类码只给通用消息", err: errcode.New(20099, "连不上 db-1:5432", codes.Unavailable), wantStatus: http.StatusServiceUnavailable, want: ErrorBody{20099, "内部错误"}},
		{name: "ctx 超时", err: context.DeadlineExceeded, wantStatus: http.StatusGatewayTimeout, want: ErrorBody{10005, "请求超时"}},
		{name: "ctx 取消", err: context.Canceled, wantStatus: 499, want: ErrorBody{10006, "请求已取消"}},
		{name: "普通错误是内部错误", err: errors.New("dial tcp 10.0.0.1:5432"), wantStatus: http.StatusInternalServerError, want: ErrorBody{10003, "内部错误"}},
		{name: "漏填状态的 errcode 按内部错误兜底", err: errcode.Code{Code: 20050, Message: "x"}, wantStatus: http.StatusInternalServerError, want: ErrorBody{20050, "内部错误"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := translate(tt.err)
			if status != tt.wantStatus || body != tt.want {
				t.Errorf("translate = %d %+v, want %d %+v", status, body, tt.wantStatus, tt.want)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Error("addr 为空应报错")
	}
	if err := (Config{Addr: ":0"}).Validate(); err != nil {
		t.Errorf("addr 齐全不应报错: %v", err)
	}
}

func TestNewPanicsOnBadAssembly(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s：应在装配期 panic", name)
			}
		}()
		f()
	}
	mustPanic("addr 为空", func() { New(context.Background(), Config{}) })
	s := New(context.Background(), Config{Addr: "127.0.0.1:0"})
	defer s.Stop(context.Background())
	mustPanic("端口已被占用", func() { New(context.Background(), Config{Addr: s.Addr()}) })
}

// syncBuffer 是并发安全的日志缓冲：访问日志在服务协程里写，断言在测试协程里读。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// entries 把 JSON 日志逐行解出来。
func (b *syncBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志不是 JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

type echo struct {
	Name string `json:"name"`
}

func TestServer(t *testing.T) {
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	docs := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "docs:"+r.URL.Path) })
	s := New(context.Background(), Config{Addr: "127.0.0.1:0"},
		WithLogger(logger),
		WithTracerProvider(tp),
		WithMount("/docs", docs),
		WithRoutes(func(r gin.IRouter) {
			r.GET("/ok", Handle(func(*gin.Context) (echo, error) { return echo{Name: "x"}, nil }))
			r.POST("/echo", Handle(func(c *gin.Context) (echo, error) {
				var req echo
				err := Bind(c, &req)
				return req, err
			}))
			r.GET("/fail", Handle(func(*gin.Context) (echo, error) {
				return echo{}, errcode.Wrap(errors.New("行锁等待超时"), errcode.New(40002, "订单状态不允许此操作", codes.InvalidArgument))
			}))
			r.GET("/panic", Handle(func(*gin.Context) (echo, error) { panic("boom") }))
			r.GET("/items/:id", Handle(func(*gin.Context) (echo, error) { return echo{}, nil }))
		}),
	)
	done := make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()
	base := "http://" + s.Addr()

	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	cases := []struct {
		method, path, body string
		wantStatus         int
		wantBody           string
	}{
		{"GET", "/healthz", "", 200, "ok"},
		{"GET", "/ok", "", 200, `{"name":"x"}`},
		{"POST", "/echo", `{"name":"y","unknown":1}`, 200, `{"name":"y"}`},
		{"POST", "/echo", "", 200, `{"name":""}`},
		{"POST", "/echo", `{"name":`, 400, `{"code":10001,"message":"参数错误"}`},
		{"GET", "/fail", "", 400, `{"code":40002,"message":"订单状态不允许此操作"}`},
		{"GET", "/panic", "", 500, `{"code":10003,"message":"内部错误"}`},
		{"GET", "/items/42", "", 200, `{"name":""}`},
		{"GET", "/docs", "", 200, "docs:/docs"},
		{"GET", "/docs/order/v1/order.openapi.json", "", 200, "docs:/docs/order/v1/order.openapi.json"},
	}
	for _, c := range cases {
		status, body := do(c.method, c.path, c.body)
		if status != c.wantStatus || body != c.wantBody {
			t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, status, body, c.wantStatus, c.wantBody)
		}
	}

	// 访问日志：成功 Info、失败 Warn，失败带业务码与原始错误；字段齐全。
	var failEntry, okEntry map[string]any
	for _, e := range logs.entries(t) {
		switch e["path"] {
		case "/fail":
			failEntry = e
		case "/ok":
			okEntry = e
		}
	}
	if okEntry == nil || okEntry["level"] != "INFO" || okEntry["route"] != "/ok" || okEntry["status"] != float64(200) || okEntry["code"] != nil {
		t.Errorf("成功请求的访问日志不对: %v", okEntry)
	}
	if failEntry == nil || failEntry["level"] != "WARN" || failEntry["status"] != float64(400) ||
		failEntry["code"] != float64(40002) || !strings.Contains(failEntry["err"].(string), "行锁等待超时") {
		t.Errorf("失败请求的访问日志不对: %v", failEntry)
	}
	for _, key := range []string{"method", "route", "path", "peer", "status", "duration"} {
		if _, ok := failEntry[key]; !ok {
			t.Errorf("访问日志缺字段 %s: %v", key, failEntry)
		}
	}

	// 链路追踪：span 按路由模板命名，不按实际路径。
	var named bool
	for _, sp := range exporter.GetSpans() {
		if sp.Name == "GET /items/:id" {
			named = true
		}
	}
	if !named {
		t.Error("没有按路由模板命名的 span（GET /items/:id）")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("正常停机时 Start 应返回 nil: %v", err)
	}
}
