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
// 造出任意 metadata 键（见 incomingHeaderMatcher）。所以 metadata 默认只适合承载
// 「需要被校验的凭证」，不能用来传「已经被上游认证过的身份」。例外只有一种：本服务
// 只给受信的上游调（挂了固定凭证鉴权，如 FixedBearerAuth），且不开 HTTP 出口——
// 这时到得了业务代码的调用都已验过凭证，也没有 Grpc-Metadata-* 头可以伪造键，
// 才能采信上游经 metadata 带来的身份；两个前提缺一，身份就必须由本进程自己算出来放进 ctx。
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
// 返回非 nil error 即拒绝（错误应为 errcode 或 status 形态，出口层会翻译）。
//
// 放行时返回的 ctx 取代原 ctx 交给后续链路与 handler——策略因此也是注入已认证
// 身份的地方：解析凭证与构造身份本是同一次工作，拆成「鉴权」与「取身份」两个钩子
// 必然解析两遍，两边还可能算出不一致的结果。身份的类型与 ctx 键由服务自己定义
// （pkg 不含业务概念），策略里 context.WithValue 塞进去即可：
//
//	func NewAuth(repo UserRepo) transport.Authenticator {
//	    return func(ctx context.Context, _ string) (context.Context, error) {
//	        token, ok := transport.BearerToken(ctx)
//	        if !ok {
//	            return nil, errcode.ErrUnauthenticated
//	        }
//	        u, err := repo.ByToken(ctx, token)
//	        if err != nil {
//	            return nil, errcode.Wrap(err, errcode.ErrUnauthenticated)
//	        }
//	        return WithUser(ctx, u), nil // WithUser 是服务自己的 ctx 键
//	    }
//	}
//
// 放行但无身份可注入时返回传入的 ctx；返回 nil ctx 等同于返回原 ctx（由 authorize
// 兜住），不会把 nil 传给 handler。豁免的方法与路径不经策略，handler 因此必须
// 容忍「ctx 里没有身份」——拿身份的那一侧要判存在，不能假定一定有。
type Authenticator func(ctx context.Context, fullMethod string) (context.Context, error)

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
	return func(ctx context.Context, _ string) (context.Context, error) {
		// 空 token 是「配置漏填」而非「本服务不要凭证」，必须在比对之前拦掉：
		// 客户端发一个空的 Bearer 凭证就会与空配置比中，那正是本策略要防的裸奔。
		if token == "" {
			return nil, errcode.ErrUnauthenticated
		}
		got, ok := BearerToken(ctx)
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			return nil, errcode.ErrUnauthenticated
		}
		// 固定凭证只证明「调用方持有本服务的 token」，没有身份可注入，原样返回。
		return ctx, nil
	}
}

// unaryAuth gRPC Unary 鉴权拦截器。
func unaryAuth(a Authenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := authorize(ctx, a, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// streamAuth gRPC Stream 鉴权拦截器，放行规则与 unaryAuth 一致。
func streamAuth(a Authenticator) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := authorize(ss.Context(), a, info.FullMethod)
		if err != nil {
			return err
		}
		if ctx != ss.Context() {
			ss = &authStream{ServerStream: ss, ctx: ctx}
		}
		return handler(srv, ss)
	}
}

// authStream 把鉴权策略返回的 ctx 带给流式 handler。grpc.ServerStream 的 ctx 由
// grpc-go 持有、没有替换入口，只能包一层覆写 Context()——策略没动 ctx 时不包，
// 不给未用到身份注入的服务凭空加一层间接。
type authStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authStream) Context() context.Context { return s.ctx }

// authorize 跑豁免判断与鉴权策略，返回要交给后续链路的 ctx。
// 策略放行却返回 nil ctx 时退回传入的 ctx：那是最容易写出的疏漏（return nil, nil），
// 让它等价于「什么也不注入」，而不是把 nil ctx 传下去在 handler 里炸开。
func authorize(ctx context.Context, a Authenticator, fullMethod string) (context.Context, error) {
	for _, prefix := range publicMethodPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return ctx, nil
		}
	}
	next, err := a(ctx, fullMethod)
	if err != nil {
		return nil, err
	}
	if next == nil {
		return ctx, nil
	}
	return next, nil
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
// code 记出口翻译后线上实际发出的码（本拦截器在翻译之内，看到的是原始错误），
// 否则 errcode 与裸错误在日志里一律是 Unknown，分不开客户端取消与服务端故障；err 仍记原始错误。
func logAccess(ctx context.Context, logger *slog.Logger, method string, elapsed time.Duration, err error) {
	attrs := []any{
		"rpc", method,
		"peer", peerAddr(ctx),
		"code", status.Code(toStatusErr(err)).String(),
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
