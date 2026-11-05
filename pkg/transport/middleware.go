package transport

// 直连 REST 形态（NewRESTServer）的拦截链：没有 gRPC 拦截器可挂，等价能力经
// runtime.WithMiddlewares 组装在每条 gateway 路由上，链序与 gRPC 侧一致——
// 访问日志在外、鉴权在内，出口翻译由 mux 的错误处理器承担（renderError）。
// middleware 由 ServeMux.Handle 在路由注册时挂上，只包住 gateway 路由：
// /healthz、WithMount 挂载点与未匹配路由的 404 都不经此，与 gRPC 侧
// 「探针豁免、未注册方法不经拦截器」的语义对齐。

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

// directAccessLog 直连形态的访问日志 middleware，字段与 gRPC 侧 logAccess 同构
// （code 用同一套 gRPC 码字表，成功失败都记）；rpc 字段记 HTTP 方法与路径。
// code 取出口翻译后实际发出的码——renderError 经响应写回原（accessWriter）通知
// 本 middleware，成功路径无人记录即 OK。
func directAccessLog(logger *slog.Logger) runtime.Middleware {
	return func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			begin := time.Now()
			state := &accessState{}
			next(&accessWriter{ResponseWriter: w, state: state}, r, pathParams)
			attrs := []any{
				"http", r.Method + " " + r.URL.Path,
				"peer", r.RemoteAddr,
				"code", state.logCode().String(),
				"duration", time.Since(begin).String(),
			}
			if state.err != nil {
				logger.WarnContext(r.Context(), "http 访问", append(attrs, "err", state.err)...)
				return
			}
			logger.InfoContext(r.Context(), "http 访问", attrs...)
		}
	}
}

// directAuth 直连形态的鉴权 middleware，复用 gRPC 侧的 authorize 与 Authenticator
// 策略。与 gRPC 侧的两点差异：Authorization 从 HTTP 头取并就地转成 incoming
// metadata（middleware 在注解之前运行，metadata 注解发生在被包的 handler 里）；
// 策略收到的 method 参数是 HTTP 路径而非 gRPC 方法名——按方法名分支的策略须知
// 此差异。拒绝时按统一错误体渲染，与业务错误同一出口。
func directAuth(a Authenticator) runtime.Middleware {
	return func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			ctx := r.Context()
			if token := r.Header.Get("Authorization"); token != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", token))
			}
			if err := authorize(ctx, a, r.URL.Path); err != nil {
				renderError(w, err)
				return
			}
			next(w, r, pathParams)
		}
	}
}

// accessState 汇一次请求的访问日志状态：出口翻译后的 gRPC 码与原始错误由
// renderError 写入，响应状态码由 accessWriter 捕获。
type accessState struct {
	grpcCode codes.Code
	err      error
	status   int
}

// logCode 返回日志该记的 gRPC 码：走过出口翻译的按翻译结果；没走过但响应是
// 4xx/5xx（handler 绕过统一出口自己 WriteHeader）记 Unknown，不把失败记成 OK。
func (a *accessState) logCode() codes.Code {
	switch {
	case a.err != nil:
		return a.grpcCode
	case a.status >= 400:
		return codes.Unknown
	default:
		return codes.OK
	}
}

// accessWriter 包住业务 handler 看到的 ResponseWriter：捕获响应状态码，并把
// renderError 的记录请求转交给本次请求的 accessState。renderError 按接口断言
// 识别它（见 errcode.go），环回形态没有这层包装、断言不中即跳过。
type accessWriter struct {
	http.ResponseWriter
	state *accessState
}

func (w *accessWriter) WriteHeader(status int) {
	w.state.status = status
	w.ResponseWriter.WriteHeader(status)
}

// recordAccess 供 renderError 记录出口翻译结果。
func (w *accessWriter) recordAccess(code codes.Code, err error) {
	w.state.grpcCode, w.state.err = code, err
}

// Unwrap 让 http.NewResponseController 能穿透到被包的原 Writer。
func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// directPropagators 直连形态 otelhttp 用的传播器：提取入站 W3C traceparent，
// 上游经 HTTP 打来的调用得以续链（环回形态的 HTTP 端口没有这一层提取）。
// 仓库不设 otel 全局，传播器同 httpc 一样显式给出、只认 TraceContext。
var directPropagators = propagation.TraceContext{}
