package transport

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/yanking/go-skeleton/pkg/errcode"
)

// TokenFromMetadata 从 gRPC metadata 取指定键的首个值。
// gateway 会把 HTTP 头转成 metadata，故 HTTP 流量同样可取。
//
// 取到的值一律当外部输入看待：HTTP 侧的调用方可以用 Grpc-Metadata-<键> 头凭空
// 造出任意 metadata 键（见 incomingHeaderMatcher）。所以 metadata 只适合承载
// 「需要被校验的凭证」，不能用来传「已经被上游认证过的身份」——那种值必须由本
// 进程的鉴权拦截器自己算出来放进 ctx，不能读客户端给的。
func TokenFromMetadata(ctx context.Context, key string) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get(key)
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// BearerToken 从 metadata 的 authorization 取 Bearer 凭证。
func BearerToken(ctx context.Context) (string, bool) {
	raw, ok := TokenFromMetadata(ctx, "authorization")
	if !ok {
		return "", false
	}
	return strings.CutPrefix(raw, "Bearer ")
}

// Authenticator 鉴权策略：由服务注入（pkg 不含业务概念）。
// 返回非 nil 即拒绝（错误应为 errcode 或 status 形态，出口层会翻译），nil 放行。
type Authenticator func(ctx context.Context, fullMethod string) error

// publicMethodPrefixes 无需鉴权的方法前缀：health 探针与 reflection——
// 探针带不上凭证，拦它们等于让编排系统误判实例死亡。
var publicMethodPrefixes = []string{
	"/grpc.health.v1.",
	"/grpc.reflection.v1.",
	"/grpc.reflection.v1alpha.",
}

// FixedBearerAuth 固定 Bearer token 比对策略：token 为空则全部拒绝
// （fail-closed，只有探针放行），避免配置缺省时把 API 裸奔上线。
// 比对用 subtle.ConstantTimeCompare，耗时不随匹配上的前缀长度变化。
func FixedBearerAuth(token string) Authenticator {
	return func(ctx context.Context, _ string) error {
		// 空 token 是「配置漏填」而非「本服务不要凭证」，必须在比对之前拦掉：
		// 客户端发一个空的 Bearer 凭证就会与空配置比中，那正是本策略要防的裸奔。
		if token == "" {
			return errcode.ErrUnauthenticated
		}
		got, ok := BearerToken(ctx)
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			return errcode.ErrUnauthenticated
		}
		return nil
	}
}

// unaryAuth gRPC Unary 鉴权拦截器。
func unaryAuth(a Authenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authorize(ctx, a, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// streamAuth gRPC Stream 鉴权拦截器，放行规则与 unaryAuth 一致。
func streamAuth(a Authenticator) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authorize(ss.Context(), a, info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func authorize(ctx context.Context, a Authenticator, fullMethod string) error {
	for _, prefix := range publicMethodPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return nil
		}
	}
	return a(ctx, fullMethod)
}

// unaryAccessLog gRPC Unary 访问日志拦截器：成功 Info、失败 Warn，
// 记方法、来源地址、状态码与耗时；trace_id 由 slog 的 ctx 钩子自动附带。
// 经 gateway 环回进来的 HTTP 流量同样经过这里，一套日志两种协议。
func unaryAccessLog(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		begin := time.Now()
		resp, err := handler(ctx, req)
		logAccess(ctx, logger, info.FullMethod, time.Since(begin), err)
		return resp, err
	}
}

// streamAccessLog gRPC Stream 访问日志：在流结束后记录整个生命周期。
func streamAccessLog(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		begin := time.Now()
		err := handler(srv, ss)
		logAccess(ss.Context(), logger, info.FullMethod, time.Since(begin), err)
		return err
	}
}

// logAccess 输出一条访问日志。code 成功失败都记（成功即 OK）——字段稳定存在，
// 下游才能统一按它做检索、告警与成功率 SLI；只在失败时才有的字段做不了分母。
func logAccess(ctx context.Context, logger *slog.Logger, method string, elapsed time.Duration, err error) {
	attrs := []any{
		"rpc", method,
		"peer", peerAddr(ctx),
		"code", status.Code(err).String(),
		"duration", elapsed.String(),
	}
	if err != nil {
		logger.WarnContext(ctx, "rpc 访问", append(attrs, "err", err)...)
		return
	}
	logger.InfoContext(ctx, "rpc 访问", attrs...)
}

// peerAddr 取对端地址；取不到时给 "unknown" 而非某个合法 IP——
// 占位值长得像真地址，排查时会被当成实际来源。
func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}
