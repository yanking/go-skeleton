// httpc 的导出行为集中在 New 与 Client 的请求方法，按 go-style 用 _test 包做黑盒测试。
package httpc_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
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
