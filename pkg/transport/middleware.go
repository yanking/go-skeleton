package transport

// 直连 REST 形态（NewRESTServer）的拦截链：没有 gRPC 拦截器可挂，等价能力经
// runtime.WithMiddlewares 组装在每条 gateway 路由上，链序与 gRPC 侧一致——
// 访问日志在外、鉴权在内、WithMiddleware 的自定义中间件在最内（仍在 handler 之外），
// 出口翻译由 mux 的错误处理器承担（renderError）。
// middleware 由 ServeMux.Handle 在路由注册时挂上，只包住 gateway 路由：
// /healthz、WithMount 挂载点与未匹配路由的 404 都不经此，与 gRPC 侧
// 「探针豁免、未注册方法不经拦截器」的语义对齐。

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/yanking/go-skeleton/pkg/errcode"
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
// 此差异。拒绝时按统一错误体渲染，与业务错误同一出口；放行时策略返回的 ctx 经
// r.WithContext 带给 handler，身份注入因此在直连形态同样生效。
func directAuth(a Authenticator) runtime.Middleware {
	return func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			ctx := r.Context()
			if token := r.Header.Get("Authorization"); token != "" {
				ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", token))
			}
			ctx, err := authorize(ctx, a, r.URL.Path)
			if err != nil {
				renderError(w, err)
				return
			}
			// 策略注入的身份要带给 handler。为取凭证临时注入的 incoming metadata
			// 顺带也带了下去，无害：生成代码的 AnnotateContext 用
			// metadata.NewIncomingContext 按请求头整体替换，不会叠出重复键。
			next(w, r.WithContext(ctx), pathParams)
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

// directTimeout 直连形态的处理超时 middleware，语义同 gRPC 侧的 unaryTimeout。
func directTimeout(timeout time.Duration) runtime.Middleware {
	return func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next(w, r.WithContext(ctx), pathParams)
		}
	}
}

// directRecovery 直连形态的 panic 兜底，位置与理由同 gRPC 侧的 unaryRecovery。
//
// net/http 自己会在连接层 recover，所以进程不会死，但代价是故障变得看不见：
// 客户端拿到裸 EOF 而不是 500（于是触发重试风暴），slog 里一条记录都没有，
// 堆栈走标准 log 的 stderr 纯文本。这里把三件事都补上。
//
// 已经写出响应头之后才 panic 的，不能再渲染错误体——那会把两段 JSON 拼在一起，
// 客户端解析不出来，而访问日志还会把它记成当初那个成功状态码。此时唯一诚实的
// 信号是断掉连接：panic(http.ErrAbortHandler) 正是 net/http 为此准备的哨兵
// （它不再打印堆栈，连接直接断，客户端至少知道出事了）。
func directRecovery(logger *slog.Logger) runtime.Middleware {
	return func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			rw := &recoveryWriter{ResponseWriter: w}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				logPanic(r.Context(), logger, r.Method+" "+r.URL.Path, rec)
				if rw.wroteHeader {
					panic(http.ErrAbortHandler)
				}
				renderError(rw, errcode.ErrInternal)
			}()
			next(rw, r, pathParams)
		}
	}
}

// recoveryWriter 记下响应头是否已写出，供 directRecovery 判断还能不能渲染错误体。
type recoveryWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *recoveryWriter) WriteHeader(status int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *recoveryWriter) Write(b []byte) (int, error) {
	// 没显式 WriteHeader 就 Write 时，net/http 会隐式补一个 200——同样算已写出。
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// recordAccess 把 renderError 的记录请求透传给外层的 accessWriter（若有）。
// 不透传的话本层的包装会挡住那条断言，访问日志就记不到出口翻译后的码。
func (w *recoveryWriter) recordAccess(code codes.Code, err error) {
	if acc, ok := w.ResponseWriter.(interface {
		recordAccess(codes.Code, error)
	}); ok {
		acc.recordAccess(code, err)
	}
}

// Unwrap 让 http.NewResponseController 与外层包装能穿透到原 Writer。
func (w *recoveryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
