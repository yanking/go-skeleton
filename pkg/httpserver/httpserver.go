// Package httpserver 是基于 gin 的 HTTP 服务组件（http 形态）：只开一个 HTTP 端口，路由由服务自己用 gin 注册。
// 出口行为与 pkg/transport 对齐：
//   - 错误体同为 {"code":<业务码>,"message":<用户可读消息>}，HTTP 状态按 gRPC 状态码的同一张表映射
//     （grpc-gateway 的 HTTPStatusFromCode），故障类码的消息换成通用内部错误消息，原始错误只进日志；
//   - 访问日志成功记 Info、失败记 Warn，字段固定：method、route、path、peer、status、duration，失败另加 code 与 err；
//   - 链路追踪经 otelhttp，span 按路由模板命名（如 "GET /v1/orders/:id"），不按带参数的实际路径。
//
// handler 用 Handle 包一层：只管返回结果或 error，由本包统一翻译，业务代码里不手写错误响应；
// 请求体用 Bind 解析，解不动即参数错误 10001。
//
// 全局状态：gin 的运行模式是进程级开关，New 里设为 release，关掉它往标准输出打的调试信息（会混进 JSON 日志）。
// 这是 go-style 硬规则 6 的又一个例外；代价是同进程里别的 gin 引擎也随之是 release 模式。
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"

	"github.com/yanking/go-skeleton/pkg/errcode"
)

// defaultReadHeaderTimeout 请求头读取上限，挡住只发头不发完的慢连接（与 pkg/transport 一致）。
const defaultReadHeaderTimeout = 10 * time.Second

// Config HTTP 服务配置。
type Config struct {
	// Addr 监听地址（host:port），必填；写 :0 即由系统分配端口（测试用），实际地址见 Server.Addr。
	Addr string `yaml:"addr"`
}

// Validate 校验配置。
func (c Config) Validate() error {
	if c.Addr == "" {
		return errors.New("addr 不能为空")
	}
	return nil
}

// ErrorBody 是出错时的响应体，形状与 pkg/transport 的 gateway 出口一致，供接口文档引用。
type ErrorBody struct {
	// 业务码，客户端据此分支处理。
	Code int `json:"code"`
	// 面向用户的提示，可直接展示。
	Message string `json:"message"`
} //@name ErrorBody

// Option 是 New 的可选项。
type Option func(*options)

type mount struct {
	pattern string
	handler http.Handler
}

type options struct {
	tracerProvider trace.TracerProvider
	logger         *slog.Logger
	routes         []func(gin.IRouter)
	mounts         []mount
}

// WithTracerProvider 注入链路追踪，不给则不埋 span。
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) { o.tracerProvider = tp }
}

// WithLogger 注入访问日志输出，不给则不记访问日志。
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithRoutes 追加一组路由注册，按传入顺序执行；各模块各传一次。
func WithRoutes(register func(r gin.IRouter)) Option {
	return func(o *options) { o.routes = append(o.routes, register) }
}

// WithMount 把一个现成的 http.Handler 挂在 pattern 及其子树上（如 /docs 文档页），路径原样交给它，不去前缀。
func WithMount(pattern string, h http.Handler) Option {
	return func(o *options) { o.mounts = append(o.mounts, mount{pattern: pattern, handler: h}) }
}

// Server 是 HTTP 服务组件，实现 app.Component。
type Server struct {
	srv *http.Server
	ln  net.Listener
}

// New 构造 HTTP 服务。此刻即「起不来就该死」的装配期：端口占用、路由冲突一律 panic；
// 全部注册在返回前完成，调用方拿到的即可运行状态。/healthz 由本包注册。
func New(ctx context.Context, cfg Config, opts ...Option) *Server {
	o := &options{}
	for _, apply := range opts {
		apply(o)
	}
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配 httpserver: %w", err))
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	if o.logger != nil {
		engine.Use(accessLog(o.logger))
	}
	engine.Use(recovery)
	if o.tracerProvider != nil {
		engine.Use(spanName)
	}
	engine.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	for _, register := range o.routes {
		register(engine)
	}
	for _, m := range o.mounts {
		engine.Any(m.pattern, gin.WrapH(m.handler))
		engine.Any(m.pattern+"/*rest", gin.WrapH(m.handler))
	}

	var handler http.Handler = engine
	if o.tracerProvider != nil {
		handler = otelhttp.NewHandler(engine, "http", otelhttp.WithTracerProvider(o.tracerProvider))
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		panic(fmt.Errorf("装配 httpserver: 监听 %s: %w", cfg.Addr, err))
	}
	return &Server{
		srv: &http.Server{Handler: handler, ReadHeaderTimeout: defaultReadHeaderTimeout},
		ln:  ln,
	}
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Name 实现 app.Component。
func (s *Server) Name() string { return "http" }

// Start 实现 app.Component：阻塞服务，直到 Stop 或出错；停机期的正常收尾不上报。
func (s *Server) Start(context.Context) error {
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop 实现 app.Component：停止接新连接，等在途请求在 ctx 期限内收尾。
func (s *Server) Stop(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// Handle 把「返回结果或 error」的 handler 接到 gin：成功时以 200 返回 JSON；
// 出错时按 Render 的规则写错误体，原始错误记进访问日志。
func Handle[T any](fn func(c *gin.Context) (T, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp, err := fn(c)
		if err != nil {
			Render(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}

// Render 把 err 写成错误响应并中止后续 handler：errcode 按其状态与消息输出，ctx 超时、取消分别对应
// 10005、10006，其余一律是内部错误 10003；故障类码只给通用消息。原始错误挂到 gin.Context 上，由访问日志记录。
func Render(c *gin.Context, err error) {
	_ = c.Error(err)
	status, body := translate(err)
	c.Set(ctxKeyCode, body.Code)
	c.AbortWithStatusJSON(status, body)
}

// Bind 把 JSON 请求体解进 dst；请求体为空视同 {}，未知字段忽略（与 grpc-gateway 出口一致）。
// 解析失败是参数错误 10001，原因只进日志。
func Bind(c *gin.Context, dst any) error {
	if c.Request.Body == nil {
		return nil
	}
	if err := json.NewDecoder(c.Request.Body).Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return errcode.Wrap(fmt.Errorf("解析请求体: %w", err), errcode.ErrInvalidParameter)
	}
	return nil
}

// ctxKeyCode 是 gin.Context 里记业务码的键，访问日志据此输出 code 字段。
const ctxKeyCode = "httpserver.code"

// translate 把错误翻成 HTTP 状态与错误体。
func translate(err error) (int, ErrorBody) {
	var ec errcode.Code
	switch {
	case errors.As(err, &ec):
	case errors.Is(err, context.DeadlineExceeded):
		ec = errcode.ErrDeadlineExceeded
	case errors.Is(err, context.Canceled):
		ec = errcode.ErrCanceled
	default:
		ec = errcode.ErrInternal
	}
	if ec.Status == codes.OK {
		// 漏填状态的 errcode：OK 状态没有「错误」可言，按内部错误兜底，业务码照留。
		ec.Status = codes.Internal
	}
	msg := ec.Message
	if isFault(ec.Status) {
		msg = errcode.ErrInternal.Message
	}
	return runtime.HTTPStatusFromCode(ec.Status), ErrorBody{Code: ec.Code, Message: msg}
}

// isFault 报告状态是否属于服务端故障：这类错误的消息可能夹带内部细节，不给客户端看。
func isFault(c codes.Code) bool {
	switch c {
	case codes.Unknown, codes.Internal, codes.Unavailable, codes.DataLoss:
		return true
	}
	return false
}

// recovery 兜住 handler 的 panic：按内部错误响应，堆栈随原始错误进访问日志。
func recovery(c *gin.Context) {
	defer func() {
		if r := recover(); r != nil {
			Render(c, fmt.Errorf("handler panic: %v\n%s", r, debug.Stack()))
		}
	}()
	c.Next()
}

// spanName 把 otelhttp 建的 span 按路由模板重新命名并补上 http.route；没匹配到路由的保持原名。
func spanName(c *gin.Context) {
	if route := c.FullPath(); route != "" {
		span := trace.SpanFromContext(c.Request.Context())
		span.SetName(c.Request.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route))
	}
	c.Next()
}

// accessLog 每个请求记一条访问日志。trace_id 等由 pkg/log 的 Extractor 从请求 ctx 里带出。
func accessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		ctx := c.Request.Context()
		attrs := []any{
			"method", c.Request.Method,
			"route", c.FullPath(),
			"path", c.Request.URL.Path,
			"peer", c.Request.RemoteAddr,
			"status", c.Writer.Status(),
			"duration", time.Since(start).String(),
		}
		if err := c.Errors.Last(); err != nil {
			code, _ := c.Get(ctxKeyCode)
			logger.WarnContext(ctx, "http 访问", append(attrs, "code", code, "err", err.Err)...)
			return
		}
		logger.InfoContext(ctx, "http 访问", attrs...)
	}
}
