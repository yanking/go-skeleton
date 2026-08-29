package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/yanking/go-skeleton/pkg/errcode"
)

func TestFixedBearerAuth(t *testing.T) {
	authn := FixedBearerAuth("tok")
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer tok"))

	if err := authn(ctx, "/user.v1.UserService/GetUser"); err != nil {
		t.Errorf("凭证正确应放行, got %v", err)
	}

	bad := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer wrong"))
	if err := authn(bad, "/user.v1.UserService/GetUser"); !errors.Is(err, errcode.ErrUnauthenticated) {
		t.Errorf("凭证错误应拒绝, got %v", err)
	}

	if err := authn(context.Background(), "/user.v1.UserService/GetUser"); !errors.Is(err, errcode.ErrUnauthenticated) {
		t.Errorf("缺凭证应拒绝, got %v", err)
	}
}

// TestFixedBearerAuthEmptyToken 守住「token 为空则全部拒绝」的 fail-closed 承诺。
// 关键用例是空配置遇上空凭证：两个空串相等，朴素比较会放行——正是配置漏填时
// 把 API 裸奔上线的那条路。
func TestFixedBearerAuthEmptyToken(t *testing.T) {
	authn := FixedBearerAuth("")
	cases := []struct {
		name   string
		header string
	}{
		{"无 authorization 头", ""},
		{"Bearer 后跟空串", "Bearer "},
		{"Bearer 后跟任意值", "Bearer whatever"},
		{"只有 Bearer 无空格", "Bearer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			if c.header != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", c.header))
			}
			if err := authn(ctx, "/user.v1.UserService/GetUser"); !errors.Is(err, errcode.ErrUnauthenticated) {
				t.Errorf("token 为空时应一律拒绝, got %v", err)
			}
		})
	}
}

// TestAccessLogFields 守住访问日志的字段契约：成功与失败都带 code，
// 否则按 code 做检索、告警与成功率 SLI 时成功请求会整批漏掉。
func TestAccessLogFields(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{"成功", nil, "OK"},
		{"失败", errcode.ErrNotFound, "Unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			interceptor := unaryAccessLog(logger)
			_, _ = interceptor(context.Background(), nil,
				&grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"},
				func(context.Context, any) (any, error) { return nil, c.err })

			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("日志不是合法 JSON: %v", err)
			}
			for _, key := range []string{"rpc", "peer", "code", "duration"} {
				if _, ok := got[key]; !ok {
					t.Errorf("访问日志缺 %q 字段: %s", key, buf.String())
				}
			}
			if got["code"] != c.wantCode {
				t.Errorf("code = %v，期望 %q", got["code"], c.wantCode)
			}
		})
	}
}

// TestPeerAddrUnknown 取不到对端时给出的占位值不能像个真地址——
// "0.0.0.0" 在日志里会被当成实际来源。
func TestPeerAddrUnknown(t *testing.T) {
	if got := peerAddr(context.Background()); got != "unknown" {
		t.Errorf("peerAddr 无对端时 = %q，期望 %q", got, "unknown")
	}
}

func TestUnaryAuth(t *testing.T) {
	alwaysReject := func(context.Context, string) error { return errcode.ErrUnauthenticated }
	unaryInfo := func(method string) *grpc.UnaryServerInfo { return &grpc.UnaryServerInfo{FullMethod: method} }
	called := false
	handler := func(context.Context, any) (any, error) { called = true; return "ok", nil }

	t.Run("health 探针绕过鉴权", func(t *testing.T) {
		if _, err := unaryAuth(alwaysReject)(context.Background(), nil,
			unaryInfo("/grpc.health.v1.Health/Check"), handler); err != nil || !called {
			t.Errorf("探针应放行, got err=%v called=%v", err, called)
		}
	})

	t.Run("业务方法凭策略放行或拒绝", func(t *testing.T) {
		called = false
		if _, err := unaryAuth(func(context.Context, string) error { return nil })(
			context.Background(), nil, unaryInfo("/user.v1.UserService/GetUser"), handler); err != nil || !called {
			t.Errorf("策略放行应到 handler, got err=%v called=%v", err, called)
		}
		called = false
		_, err := unaryAuth(alwaysReject)(context.Background(), nil,
			unaryInfo("/user.v1.UserService/GetUser"), handler)
		if !errors.Is(err, errcode.ErrUnauthenticated) || called {
			t.Errorf("策略拒绝应短路, got err=%v called=%v", err, called)
		}
	})
}

// stubStream 只提供 Context 的最小流桩。
type stubStream struct{ grpc.ServerStream }

func (stubStream) Context() context.Context { return context.Background() }

func TestStreamAuth(t *testing.T) {
	alwaysReject := func(context.Context, string) error { return errcode.ErrUnauthenticated }
	called := false
	handler := func(any, grpc.ServerStream) error { called = true; return nil }
	info := &grpc.StreamServerInfo{FullMethod: "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"}

	if err := streamAuth(alwaysReject)(nil, stubStream{}, info, handler); err != nil || !called {
		t.Errorf("reflection 应绕过鉴权, got err=%v", err)
	}

	called = false
	err := streamAuth(alwaysReject)(nil, stubStream{}, &grpc.StreamServerInfo{FullMethod: "/user.v1.S/List"}, handler)
	if !errors.Is(err, errcode.ErrUnauthenticated) || called {
		t.Errorf("业务流方法应被拒, got err=%v called=%v", err, called)
	}
}

func TestUnaryAccessLog(t *testing.T) {
	newLogger := func(buf *bytes.Buffer) *slog.Logger {
		return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	unaryInfo := &grpc.UnaryServerInfo{FullMethod: "/user.v1.UserService/GetUser"}

	t.Run("成功记 Info", func(t *testing.T) {
		var buf bytes.Buffer
		if _, err := unaryAccessLog(newLogger(&buf))(context.Background(), nil, unaryInfo,
			func(context.Context, any) (any, error) { return nil, nil }); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "/user.v1.UserService/GetUser") {
			t.Errorf("访问日志不符, got %q", out)
		}
	})

	t.Run("失败记 Warn 并带状态码", func(t *testing.T) {
		var buf bytes.Buffer
		_, err := unaryAccessLog(newLogger(&buf))(context.Background(), nil, unaryInfo,
			func(context.Context, any) (any, error) { return nil, errcode.ErrNotFound })
		if err == nil {
			t.Fatal("want error")
		}
		out := buf.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, status.Code(errcode.ErrNotFound).String()) {
			t.Errorf("失败日志不符, got %q", out)
		}
	})
}
