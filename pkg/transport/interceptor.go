package transport

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// recoveryInterceptor 把 handler 里的 panic 兜成 Internal 错误。
// 没有它，任意一个业务 panic 都会带走整个进程——gRPC 自身不做 recover。
// 兜住不等于掩盖：panic 值与完整堆栈以 Error 级打出来，只是不泄露给客户端。
// 它排在日志之后、服务自有拦截器之前：既能覆盖鉴权等后续拦截器与 handler 的 panic，
// 又能让外层的日志拦截器看到被转换后的 Internal，从而照常留下访问日志。
func recoveryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(ctx, "handler panic",
					"method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "内部错误")
			}
		}()
		return handler(ctx, req)
	}
}

// healthMethodPrefix gRPC 健康检查协议的方法前缀。
const healthMethodPrefix = "/grpc.health.v1.Health/"

// loggingInterceptor 为每个 RPC 打一条出口日志：方法、状态码、耗时。
// 请求出入口只在这里打，业务代码不重复打「进入 / 退出」。
//
// 分级按状态码：OK 是正常里程碑（Info）；Internal / Unknown / DataLoss 是服务端自己出了
// 问题、需人介入（Error）；其余多为客户端传参不对一类可预期失败（Warn），不该拿去告警。
// 健康检查降到 Debug——探针每几秒来一次，打 Info 会把真实日志淹没。
func loggingInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)

		logger.LogAttrs(ctx, levelOf(code, info.FullMethod), "RPC 完成",
			slog.String("method", info.FullMethod),
			slog.String("code", code.String()),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
		return resp, err
	}
}

// levelOf 决定单条 RPC 日志的级别，规则见 loggingInterceptor 的注释。
func levelOf(code codes.Code, fullMethod string) slog.Level {
	if strings.HasPrefix(fullMethod, healthMethodPrefix) {
		return slog.LevelDebug
	}
	switch code {
	case codes.OK:
		return slog.LevelInfo
	case codes.Internal, codes.Unknown, codes.DataLoss:
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// validateInterceptor 执行 proto 注解里声明的 protovalidate 校验规则。
// 规则写在 .proto 上、由本拦截器统一执行，service 层只做注解表达不了的业务校验。
// 它排在链尾（最靠近 handler）：鉴权等自有拦截器先跑，校验失败才不会浪费在未授权的请求上。
func validateInterceptor(validator protovalidate.Validator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		msg, ok := req.(proto.Message)
		if !ok {
			return handler(ctx, req)
		}
		if err := validator.Validate(msg); err != nil {
			return nil, invalidArgument(err)
		}
		return handler(ctx, req)
	}
}

// invalidArgument 把校验失败转成带 errdetails 的 InvalidArgument。
// 违规明细回给调用方是有意的：这类错误是它自己能修好的，说清楚哪个字段不对才有意义。
// 这与「未预期错误不泄露内部信息」不冲突——那条针对的是服务端自身的故障。
func invalidArgument(err error) error {
	st := status.New(codes.InvalidArgument, "参数校验失败")

	var valErr *protovalidate.ValidationError
	if !errors.As(err, &valErr) {
		return st.Err()
	}
	detailed, detailErr := st.WithDetails(valErr.ToProto())
	if detailErr != nil {
		return st.Err()
	}
	return detailed.Err()
}
