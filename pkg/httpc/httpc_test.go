// httpc 的导出行为集中在 New 与 Client 的请求方法，按 go-style 用 _test 包做黑盒测试。
package httpc_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/yanking/go-skeleton/pkg/httpc"
)

// capture 测试服务器收到的请求要素。
type capture struct {
	method      string
	contentType string
	token       string
	traceparent string
	body        string
}

// newServer 起一个记录请求要素的测试服务器，固定返回 status 与 resp。
func newServer(t *testing.T, status int, resp string) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.method, c.body = r.Method, string(raw)
		c.contentType = r.Header.Get("Content-Type")
		c.token = r.Header.Get("X-Token")
		c.traceparent = r.Header.Get("Traceparent")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestRequestShaping(t *testing.T) {
	header := map[string]string{"X-Token": "t1"}
	tests := []struct {
		name       string
		call       func(c *httpc.Client, url string) (int, string, error)
		wantMethod string
		wantCT     string
		wantBody   string
	}{
		{
			name: "PostJSON 带体",
			call: func(c *httpc.Client, url string) (int, string, error) {
				return c.PostJSON(context.Background(), url, header, map[string]any{"a": 1}, 0)
			},
			wantMethod: "POST", wantCT: "application/json", wantBody: `{"a":1}`,
		},
		{
			name: "PostJSON 空体",
			call: func(c *httpc.Client, url string) (int, string, error) {
				return c.PostJSON(context.Background(), url, header, nil, 0)
			},
			wantMethod: "POST", wantCT: "application/json", wantBody: "",
		},
		{
			name: "PostJSON 显式 Content-Type 覆盖默认",
			call: func(c *httpc.Client, url string) (int, string, error) {
				h := map[string]string{"X-Token": "t1", "Content-Type": "application/vnd.x+json"}
				return c.PostJSON(context.Background(), url, h, map[string]any{"a": 1}, 0)
			},
			wantMethod: "POST", wantCT: "application/vnd.x+json", wantBody: `{"a":1}`,
		},
		{
			name: "PostForm 表单体",
			call: func(c *httpc.Client, url string) (int, string, error) {
				form := neturl.Values{"a": {"1"}, "b": {"x y&z"}}
				return c.PostForm(context.Background(), url, header, form, 0)
			},
			wantMethod: "POST", wantCT: "application/x-www-form-urlencoded", wantBody: "a=1&b=x+y%26z",
		},
		{
			name: "Post 原文体带自定义 Content-Type",
			call: func(c *httpc.Client, url string) (int, string, error) {
				h := map[string]string{"X-Token": "t1", "Content-Type": "application/x-www-form-urlencoded"}
				return c.Post(context.Background(), url, h, []byte("a=1&b=2"), 0)
			},
			wantMethod: "POST", wantCT: "application/x-www-form-urlencoded", wantBody: "a=1&b=2",
		},
		{
			name: "Post 原文体不设 Content-Type",
			call: func(c *httpc.Client, url string) (int, string, error) {
				return c.Post(context.Background(), url, header, []byte("raw"), 0)
			},
			wantMethod: "POST", wantCT: "", wantBody: "raw",
		},
		{
			name: "Get",
			call: func(c *httpc.Client, url string) (int, string, error) {
				return c.Get(context.Background(), url, header, 0)
			},
			wantMethod: "GET", wantCT: "", wantBody: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := newServer(t, 201, `{"ok":true}`)
			code, body, err := tt.call(httpc.New(httpc.Config{}), srv.URL)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if code != 201 || body != `{"ok":true}` {
				t.Fatalf("code, body = %d, %q", code, body)
			}
			if got.method != tt.wantMethod || got.contentType != tt.wantCT || got.body != tt.wantBody {
				t.Fatalf("下游收到 method=%q ct=%q body=%q, want %q %q %q",
					got.method, got.contentType, got.body, tt.wantMethod, tt.wantCT, tt.wantBody)
			}
			if got.token != "t1" {
				t.Fatalf("自定义头未透传: %q", got.token)
			}
		})
	}
}

func TestMarshalError(t *testing.T) {
	if _, _, err := httpc.New(httpc.Config{}).PostJSON(context.Background(), "http://127.0.0.1:0", nil, make(chan int), 0); err == nil {
		t.Fatal("不可序列化的 body 应报错")
	}
}

func TestNetworkError(t *testing.T) {
	srv, _ := newServer(t, 200, "")
	srv.Close()
	code, _, err := httpc.New(httpc.Config{}).Get(context.Background(), srv.URL, nil, 0)
	if err == nil || code != 0 {
		t.Fatalf("网络错应报 err 且 code=0, got code=%d err=%v", code, err)
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	t.Run("timeout 传 0 用 Config 缺省", func(t *testing.T) {
		c := httpc.New(httpc.Config{Timeout: 50 * time.Millisecond})
		if _, _, err := c.Get(context.Background(), srv.URL, nil, 0); err == nil {
			t.Fatal("超时应报错")
		}
	})
	t.Run("每调用 timeout 覆盖缺省", func(t *testing.T) {
		c := httpc.New(httpc.Config{Timeout: 10 * time.Second})
		if _, _, err := c.Get(context.Background(), srv.URL, nil, 50*time.Millisecond); err == nil {
			t.Fatal("缺省宽松时每调用值应生效并超时")
		}
	})
}

func TestTracing(t *testing.T) {
	t.Run("注入 TracerProvider 即埋出站 span 并透传 traceparent", func(t *testing.T) {
		recorder := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
		srv, got := newServer(t, 200, "{}")

		if _, _, err := httpc.New(httpc.Config{TracerProvider: tp}).PostJSON(context.Background(), srv.URL, nil, nil, 0); err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.traceparent == "" {
			t.Fatal("下游未收到 traceparent 头")
		}
		spans := recorder.Ended()
		if len(spans) != 1 || spans[0].SpanKind() != oteltrace.SpanKindClient {
			t.Fatalf("应记录 1 个 client span, got %d 个", len(spans))
		}
	})
	t.Run("不注入则无 traceparent", func(t *testing.T) {
		srv, got := newServer(t, 200, "{}")
		if _, _, err := httpc.New(httpc.Config{}).Get(context.Background(), srv.URL, nil, 0); err != nil {
			t.Fatalf("err = %v", err)
		}
		if got.traceparent != "" {
			t.Fatalf("未注入 provider 不应出现 traceparent: %q", got.traceparent)
		}
	})
}

// TestMaxBodyBytes 响应体上限：超限即报错，且不返回截断内容——截断的报文喂给
// 调用方解析会得出错误结论，报错比静默截断安全。
func TestMaxBodyBytes(t *testing.T) {
	tests := []struct {
		name     string
		limit    int64
		respSize int
		wantErr  bool
	}{
		{"恰好等于上限", 1024, 1024, false},
		{"超出上限一字节", 1024, 1025, true},
		{"负值即不设上限", -1, 512 << 10, false},
		{"缺省上限不误伤正常报文", 0, 1 << 20, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(make([]byte, tt.respSize))
			}))
			defer srv.Close()

			c := httpc.New(httpc.Config{MaxBodyBytes: tt.limit})
			_, body, err := c.Get(context.Background(), srv.URL, nil, 0)
			if tt.wantErr {
				if err == nil {
					t.Fatal("响应体超限却没报错")
				}
				if body != "" {
					t.Errorf("超限时不得返回内容，拿到 %d 字节", len(body))
				}
				return
			}
			if err != nil {
				t.Fatalf("未超限却报错: %v", err)
			}
			if len(body) != tt.respSize {
				t.Errorf("响应体 %d 字节，期望 %d", len(body), tt.respSize)
			}
		})
	}
}

// TestConnectionReuse 并发出站后连接应留在池里复用。共享 http.DefaultTransport 时
// MaxIdleConnsPerHost 只有 2，并发出站的绝大多数连接用完即弃，等于每笔请求一次握手。
func TestConnectionReuse(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = true
		mu.Unlock()
		time.Sleep(20 * time.Millisecond) // 让并发真正重叠，逼出多条连接
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := httpc.New(httpc.Config{})
	burst := func() {
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := c.Get(context.Background(), srv.URL, nil, 0); err != nil {
					t.Errorf("请求: %v", err)
				}
			}()
		}
		wg.Wait()
	}

	burst()
	mu.Lock()
	first := len(conns)
	mu.Unlock()

	burst()
	mu.Lock()
	total := len(conns)
	mu.Unlock()
	if total > first {
		t.Errorf("第二轮新建了 %d 条连接（共 %d，第一轮 %d）：空闲连接没留住，每笔请求都在重新握手", total-first, total, first)
	}
}

// TestDisableKeepAlives 关掉连接复用后每次请求都是新连接。代付只收 GET 的渠道靠它
// 兜住 net/http 在复用连接上自动重放 GET 的行为（见 adapter.Adapter.PayoutOrder）。
func TestDisableKeepAlives(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns[r.RemoteAddr] = true
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := httpc.New(httpc.Config{DisableKeepAlives: true})
	for range 3 {
		if _, _, err := c.Get(context.Background(), srv.URL, nil, 0); err != nil {
			t.Fatalf("请求: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(conns) != 3 {
		t.Errorf("关掉 keep-alive 后应有 3 条独立连接，实际 %d", len(conns))
	}
}

// logRecorder 收集 slog 记录的手写 handler。
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec.Clone())
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

// only 取唯一一条记录,数量不为 1 即判失败——出站一次只该有一条日志。
func (r *logRecorder) only(t *testing.T) slog.Record {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) != 1 {
		t.Fatalf("应恰好记 1 条出站日志, got %d 条", len(r.records))
	}
	return r.records[0]
}

// attrs 把记录的属性摊成 map,值取字符串形态便于断言。
func attrs(rec slog.Record) map[string]string {
	m := make(map[string]string, rec.NumAttrs())
	rec.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	return m
}

func newLogger() (*slog.Logger, *logRecorder) {
	rec := &logRecorder{}
	return slog.New(rec), rec
}

// TestOutboundLog 出站日志:每次出站恰好一条,成功 Info、失败 Warn,且 url 只留
// scheme://host/path——query 与 userinfo 常携签名、密钥,原样进日志就是凭证泄露。
func TestOutboundLog(t *testing.T) {
	t.Run("成功记 Info 且 url 剥掉 query", func(t *testing.T) {
		srv, _ := newServer(t, 200, "{}")
		logger, rec := newLogger()
		c := httpc.New(httpc.Config{Logger: logger})

		url := srv.URL + "/v1/order?sign=SECRETSIGN&key=SECRETKEY"
		if _, _, err := c.PostJSON(context.Background(), url, nil, nil, 0); err != nil {
			t.Fatalf("err = %v", err)
		}

		got := rec.only(t)
		if got.Level != slog.LevelInfo {
			t.Errorf("成功应记 Info, got %v", got.Level)
		}
		a := attrs(got)
		if want := srv.URL + "/v1/order"; a["url"] != want {
			t.Errorf("url = %q, want %q", a["url"], want)
		}
		if strings.Contains(a["url"], "SECRET") {
			t.Errorf("url 泄露了 query 里的凭证: %q", a["url"])
		}
		if a["method"] != http.MethodPost || a["status"] != "200" {
			t.Errorf("method/status = %q/%q, want POST/200", a["method"], a["status"])
		}
		if _, ok := a["cost_ms"]; !ok {
			t.Error("缺 cost_ms")
		}
		if _, ok := a["err"]; ok {
			t.Errorf("成功不该带 err: %q", a["err"])
		}
	})

	t.Run("url 剥掉 userinfo", func(t *testing.T) {
		srv, _ := newServer(t, 200, "{}")
		logger, rec := newLogger()
		c := httpc.New(httpc.Config{Logger: logger})

		u, err := neturl.Parse(srv.URL + "/pay")
		if err != nil {
			t.Fatalf("解析 URL: %v", err)
		}
		u.User = neturl.UserPassword("merchant", "SECRETPASS")
		if _, _, err := c.Get(context.Background(), u.String(), nil, 0); err != nil {
			t.Fatalf("err = %v", err)
		}

		a := attrs(rec.only(t))
		if strings.Contains(a["url"], "SECRETPASS") || strings.Contains(a["url"], "merchant") {
			t.Errorf("url 泄露了 userinfo: %q", a["url"])
		}
		if want := srv.URL + "/pay"; a["url"] != want {
			t.Errorf("url = %q, want %q", a["url"], want)
		}
	})

	t.Run("非 2xx 记 Warn", func(t *testing.T) {
		srv, _ := newServer(t, 502, "bad gateway")
		logger, rec := newLogger()
		c := httpc.New(httpc.Config{Logger: logger})

		if _, _, err := c.Get(context.Background(), srv.URL, nil, 0); err != nil {
			t.Fatalf("非 2xx 不该报 err, got %v", err)
		}

		got := rec.only(t)
		if got.Level != slog.LevelWarn {
			t.Errorf("非 2xx 应记 Warn, got %v", got.Level)
		}
		if a := attrs(got); a["status"] != "502" {
			t.Errorf("status = %q, want 502", a["status"])
		}
	})

	t.Run("网络错记 Warn 且 status 为 0", func(t *testing.T) {
		srv, _ := newServer(t, 200, "")
		srv.Close()
		logger, rec := newLogger()
		c := httpc.New(httpc.Config{Logger: logger})

		if _, _, err := c.Get(context.Background(), srv.URL, nil, 0); err == nil {
			t.Fatal("网络错应报 err")
		}

		got := rec.only(t)
		if got.Level != slog.LevelWarn {
			t.Errorf("网络错应记 Warn, got %v", got.Level)
		}
		a := attrs(got)
		if a["status"] != "0" {
			t.Errorf("status = %q, want 0", a["status"])
		}
		if a["err"] == "" {
			t.Error("网络错应带 err")
		}
	})

	t.Run("不注入 Logger 即不打日志", func(t *testing.T) {
		srv, _ := newServer(t, 200, "{}")
		if _, _, err := httpc.New(httpc.Config{}).Get(context.Background(), srv.URL, nil, 0); err != nil {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("响应体绝不进日志", func(t *testing.T) {
		srv, _ := newServer(t, 200, `{"card_no":"6222021234567890"}`)
		logger, rec := newLogger()
		c := httpc.New(httpc.Config{Logger: logger})

		if _, _, err := c.PostJSON(context.Background(), srv.URL, nil, map[string]any{"phone": "13800138000"}, 0); err != nil {
			t.Fatalf("err = %v", err)
		}

		for k, v := range attrs(rec.only(t)) {
			if strings.Contains(v, "6222021234567890") || strings.Contains(v, "13800138000") {
				t.Errorf("字段 %s 泄露了报文内容: %q", k, v)
			}
		}
	})
}
