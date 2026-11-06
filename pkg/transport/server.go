// Package transport 构造服务的对外传输组件 Server：装配期监听端口（起不来就死），
// 实现 app.Component（Start 阻塞服务、Stop 优雅停机），并承担错误处理约定的
// 「出口层」职责——把 errcode 的业务字段翻译为 gRPC status / HTTP 响应，
// 原始错误链留在服务端日志。
//
// 三种构造器对应三种形态，共用同一份 Config，各字段按形态必填或必空：
//   - NewRPCServer：纯 gRPC，只开 grpc_addr 一个端口；
//   - NewRPCRESTServer：gRPC + gateway 双端口。HTTP 侧是 grpc-gateway 从同一份
//     proto 契约转译出的代理出口：把 HTTP/JSON 请求翻成 gRPC 调用，经环回连接打回
//     本进程的 gRPC 端口，拦截链对两种协议同样生效，业务代码只写一遍；
//   - NewRESTServer：直连 REST，只开 http_addr 一个端口，没有 gRPC 服务器——pb
//     生成的 RegisterXxxHandlerServer（经 WithDirectGateway）把 handler 实现直接
//     注册进 gateway 路由，请求只做一次 JSON 编解码、直接函数调用 handler，不经
//     环回、不做第二份 proto 编解码，高 QPS 或大报文的 HTTP 出口用它。代价见
//     WithDirectGateway：流式方法一律 Unimplemented；没有 gRPC 端口，东西向调用方
//     与 grpcurl 无处可去。
//
// 两个端口各自监听：gRPC 与 HTTP 的暴露范围、mesh 协议标注（Istio 的端口名 /
// appProtocol 是按端口生效的）、网络策略往往不同——常见形态就是 gRPC 只走东西向、
// HTTP 才对外。
//
// gRPC 侧拦截链按注册顺序组装、先注册者在最外层，业务 handler 位于最内，顺序是
// 出口翻译 → 处理超时 → 访问日志 → panic 兜底 → 鉴权 → 自定义拦截器：
//
//   - errcode 出口翻译固定为链首（仓库级约定，Chain 形态的后续选项只能追加、
//     顶不掉它；漏翻译的错误在此兜成通用内部错误、ctx 超时/取消兜成 10005/10006、
//     非本包造出的 status 只保留码，原文只进访问日志）；
//   - 处理超时给 unary 请求的 ctx 设 deadline（Config.HandlerTimeout，流式不受限）；
//   - 访问日志（WithLogger 才挂：成功 Info、失败 Warn，记 proto/call/peer/code/
//     duration，trace_id 由日志钩子附带）；
//   - panic 兜底在访问日志之内、鉴权之外：panic 先转成通用内部错误，访问日志才
//     记得到这次请求，鉴权策略自己 panic 也一并兜住。没有它时一次 handler 里的
//     空指针会打死整个进程；
//   - 鉴权（WithAuthenticator 才挂：health 与 reflection 前缀豁免——拦探针会让
//     编排系统误判实例死亡；策略由服务注入，放行时可经返回的 ctx 注入已认证身份，
//     FixedBearerAuth 做固定 token 比对、空 token 全拒绝）；
//   - 最内是 WithUnaryInterceptor / WithStreamInterceptor 传入的自定义拦截器。
//
// otel 遥测另经 StatsHandler 挂载（WithTracerProvider / WithMeterProvider 才挂，
// 后者给的是 RED 三件套）。经 gateway 环回的 HTTP 流量穿过同一条链。
//
// 有一处盲区要知道：报文超限、编解码失败这类错误发生在 pb 生成代码的 dec() 阶段，
// 比拦截链更早，于是既没有业务码也不进访问日志——那部分可观测性要靠边缘网关或
// mesh 的指标补。
//
// 直连 REST 形态没有 gRPC 拦截器可挂，等价链路经 runtime.WithMiddlewares 组装在
// 每条 gateway 路由上，链序与 gRPC 侧一致（处理超时 → 访问日志 → panic 兜底 →
// 鉴权 → WithMiddleware 注入的自定义中间件 → handler）：出口翻译由 mux 的错误
// 处理器承担，与环回形态共用同一渲染，客户端契约一致；鉴权策略收到的 method 参数
// 是 HTTP 路径而非 gRPC 方法名（细节见 middleware.go）。两种 HTTP 形态都经 otelhttp
// 包住整个 mux，入站 W3C traceparent 因此得以提取续链。探针与 WithMount 挂载点不经
// middleware，对编排系统与文档页保持免鉴权。
//
// 探针与摘流（部署前必读）：
//   - /healthz 是 liveness「进程还活着吗」，恒定 200，连停机期也是——它转红会让
//     编排系统强杀进程，而停机中的实例正需要时间排空；依赖不可用同样不该让它转红，
//     重启进程治不好一个挂掉的数据库。
//   - /readyz 是 readiness「现在能接流量吗」，查就绪状态位与 WithReadinessCheck
//     注入的依赖检查。gRPC 侧的对等物是内置的 health 服务。
//   - Stop 先把就绪状态置为不可用、等 Config.DrainDelay（默认 5s）再关监听。
//     没有这段等待就是「收到 TERM 的瞬间拒连」：LB 摘流要数百毫秒到数秒，那段
//     窗口的新连接拿到的是 TCP 层 connection refused，客户端重试策略往往不覆盖。
//     这段等待发生在本组件的 Stop 里，因此**包含在** app 的 stop_timeout 之内、
//     不是额外的一段：stop_timeout 要装得下 DrainDelay + HandlerTimeout + 余量
//     （默认 5 + 15 + 5 = 25s），改其中任何一个都要回头看另外两个。
//
// 两类上限的默认值就是生产值，调它们之前先读对应常量的注释：
//   - 请求体 Config.MaxRequestBytes 默认 4MiB，与 grpc-go 的收包上限同尺度。
//     HTTP 端口没有它兜底，几个并发大报文就能把进程打到 OOM；负值即不限制。
//   - 单连接并发流 grpc.MaxConcurrentStreams 默认 256。环回双端口形态下它还顶住
//     HTTP 侧的在途并发（全部 HTTP 流量共用一条环回 ClientConn），超出的请求在
//     客户端侧排队，表现为延迟上升而不是报错。
//
// 用法：
//
//	srv := transport.NewRPCRESTServer(ctx, c.Transport,
//	    transport.WithLogger(logger),
//	    transport.WithAuthenticator(transport.FixedBearerAuth(c.Auth.Token)),
//	    transport.WithService(func(s *grpc.Server) {
//	        xxxv1.RegisterXxxServiceServer(s, handler.NewGRPC(svc))
//	    }),
//	    transport.WithGateway(xxxv1.RegisterXxxServiceHandler),
//	    transport.WithMount("/docs", openapi.Handler()),
//	)
package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

// 传输层的恒定安全默认，不设配置面。
//
// 环回形态刻意不设 ReadTimeout / WriteTimeout：它们是覆盖整个请求/响应周期的硬
// 上限，而 gateway 会把 proto 里的服务端流式方法转译成长连接响应——加了就是按秒
// 切流。直连形态不支持流式方法（见 WithDirectGateway），那条理由在它身上不成立，
// 故单列一组它自己的上限：慢速 body（header 发完后每几十秒滴一个字节）否则能
// 无限期占住连接与 goroutine。
const (
	// defaultReadHeaderTimeout 请求头读取上限，挡住只发头不发完的慢连接。
	defaultReadHeaderTimeout = 10 * time.Second
	// defaultIdleTimeout keep-alive 连接的空闲上限，到点回收，避免闲置连接堆积
	// 占住文件描述符。不设则退化为 ReadTimeout，而后者在环回形态按上面的理由是 0。
	defaultIdleTimeout = 120 * time.Second
	// defaultDirectReadTimeout 直连形态读完整个请求的上限（含 body）。
	defaultDirectReadTimeout = 60 * time.Second
	// defaultDirectWriteTimeout 直连形态写完整个响应的上限。
	defaultDirectWriteTimeout = 60 * time.Second
)

// gRPC 服务器的恒定安全默认。grpc-go 这几项的默认值分别是「不限」「永不」，
// 对长连接协议比 HTTP 侧更要紧：一条连接就能开满流、闲置连接永不回收。
const (
	// defaultMaxConcurrentStreams 单条连接上的并发流上限。grpc-go 默认
	// math.MaxUint32 即不限，单个客户端一条连接开数万流就是数万 goroutine
	// 加解码缓冲；且不设时服务端连 SETTINGS_MAX_CONCURRENT_STREAMS 都不发，
	// 客户端侧也无从自律。
	//
	// 环回双端口形态下这个值还有一层耦合要知道：全部 HTTP 流量共用一条环回
	// ClientConn，于是 HTTP 端口的在途并发也被它顶住——超出的请求在客户端侧
	// 排队，表现为延迟上升而不是报错。高 QPS 的 HTTP 出口要么调大它
	// （WithGRPCServerOption 传 grpc.MaxConcurrentStreams），要么改用直连 REST 形态。
	defaultMaxConcurrentStreams = 256
	// defaultMaxConnectionIdle 闲置连接回收时长，与 HTTP 侧的 IdleTimeout 同值、
	// 同理由（fd 不被闲置连接占住）。gRPC 是长连接协议，这条比 HTTP 侧更有必要。
	defaultMaxConnectionIdle = 120 * time.Second
	// defaultMaxConnectionAge 连接最长存活时长，到点让客户端重连。
	// 不设的话 L4 负载均衡与 headless Service 下客户端会长期钉在老 pod 上：
	// 扩容后新 pod 拿不到流量，缩容与发布时流量又挤在少数连接上抖动。
	defaultMaxConnectionAge = 30 * time.Minute
	// defaultMaxConnectionAgeGrace 到龄连接的收尾宽限，让在途 RPC 做完再断。
	defaultMaxConnectionAgeGrace = 10 * time.Second
)

// defaultDrainDelay 摘流等待的默认值：Stop 先把健康状态置为不可用，等这段时间
// 让编排系统把本实例从负载均衡摘掉，再关闭监听。
//
// 没有这段等待就是「收到 TERM 的瞬间拒连」：LB 与 kube-proxy 摘流依赖 Endpoint
// 更新或就绪探针转红，需要数百毫秒到数秒，而 Shutdown / GracefulStop 的第一件事
// 就是关 listener。这段窗口里到达的新连接拿到的是 TCP 层的 connection refused
// （不是 503），客户端的重试策略往往不覆盖它——每次滚动更新都会丢一批请求，
// 而且表现为客户端侧的连接错误，很难归因到发布。
//
// 这段等待发生在本组件的 Stop 里，所以它**包含在** app 的 stop_timeout 之内，
// 不是额外的一段：stop_timeout 要装得下 drain_delay + handler_timeout + 余量
// （默认 5 + 15 + 5 = 25s）。pkg/app 的预算分配按「保留制」给最先停的组件几乎
// 全部额度，正是为了让这三个值能这样对上（见 app.budgetFor）。
const defaultDrainDelay = 5 * time.Second

// 请求体与处理耗时的默认上限，可经 Config 覆盖。
const (
	// defaultMaxRequestBytes 单个 HTTP 请求体的大小上限，取与 grpc-go 默认的
	// 收包上限（4MiB）同值，两个端口因此同一尺度。不设的话几个并发的大报文
	// 就能把进程打到 OOM——gRPC 端口有 grpc-go 的默认值兜底，HTTP 端口没有。
	defaultMaxRequestBytes int64 = 4 << 20
	// defaultHandlerTimeout 单次 unary 请求的处理超时。客户端不传 deadline 时，
	// 没有它 handler 可以一直跑下去：下游卡住时请求无限堆积，没有自然泄压点。
	// 只作用于 unary——流式方法天然长连接，给它设硬上限就是按秒切流。
	//
	// 它不是硬上限：只给 ctx 设 deadline，不看 ctx 的 handler 照样跑完，那次请求的
	// 访问日志也照样记 OK。能被它掐断的是守着 ctx 的下游调用（本仓的 DB、Redis、
	// 出站 HTTP 都守），所以「ctx 一路传下去」这条纪律才是它生效的前提。
	//
	// 取值与停机预算是一组：停机要等在途请求做完，所以
	// app 的 stop_timeout 必须装得下 defaultDrainDelay + 本值 + 余量
	// （默认 5 + 15 + 5 = 25s）。本值配得比 stop_timeout 还大的话，
	// TERM 前一刻放进来的请求合法地可以跑过整个停机预算，优雅排空就无从保证。
	defaultHandlerTimeout = 15 * time.Second
)

// Config 传输装配参数，三种构造器共用，字段按形态必填或必空（见各构造器）。
type Config struct {
	// GRPCAddr gRPC 监听地址（如 ":9090"）。NewRPCServer / NewRPCRESTServer 必填；
	// NewRESTServer 必须为空——该形态没有 gRPC 端口。
	GRPCAddr string `yaml:"grpc_addr"`
	// HTTPAddr gateway 出口的监听地址（如 ":8080"）。NewRPCRESTServer（环回转译）与
	// NewRESTServer（直连）必填；NewRPCServer 必须为空——该形态不开 HTTP。
	HTTPAddr string `yaml:"http_addr"`
	// MaxRequestBytes 单个 HTTP 请求体的大小上限，零值取 defaultMaxRequestBytes，
	// 负值即不限制（先读那个常量的注释再决定）。gRPC 端口的对等旋钮是 grpc-go 的
	// MaxRecvMsgSize，本包按同值设置，要改用 WithGRPCServerOption。
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
	// HandlerTimeout 单次 unary 请求的处理超时，零值取 defaultHandlerTimeout，
	// 负值即不限制。它给请求 ctx 设 deadline——不强行结束 handler（那会漏下
	// goroutine、还可能撞上已写出的部分响应），而是让守着 ctx 的下游调用
	// （pkg/mysql、pkg/pgsql、pkg/redis、pkg/httpc 都守）自己断掉。
	// 业务代码因此必须把 ctx 一路传下去，这是本仓既有的纪律。
	//
	// 流式方法不受此限：长连接是它的常态。
	HandlerTimeout time.Duration `yaml:"handler_timeout"`
	// DrainDelay 摘流等待，零值取 defaultDrainDelay，负值即不等（本地开发、
	// 单实例部署，或摘流由 preStop 钩子负责时）。
	//
	// 它发生在本组件的 Stop 里，因此是**包含在** app 的 stop_timeout 之内的，
	// 不是额外的一段——配得接近 stop_timeout 就没有时间排空在途请求了。
	DrainDelay time.Duration `yaml:"drain_delay"`
	// Reflection 是否开启 gRPC reflection（grpcurl 等工具靠它发现服务与字段结构）。
	//
	// 默认关。它恒定免鉴权（探针与它一起在豁免名单里），打开就等于把完整的服务
	// 契约对任何能连到 gRPC 端口的人公开——同集群的 pod、SSRF、端口误暴露都算，
	// 而那正是构造攻击载荷的第一步。本地开发与联调打开，生产保持关闭。
	Reflection bool `yaml:"reflection"`
}

// drainDelay 取生效的摘流等待，0 即不等。
func (c Config) drainDelay() time.Duration {
	switch {
	case c.DrainDelay == 0:
		return defaultDrainDelay
	case c.DrainDelay < 0:
		return 0
	default:
		return c.DrainDelay
	}
}

// maxRequestBytes 取生效的请求体上限，0 即不限制。
func (c Config) maxRequestBytes() int64 {
	switch {
	case c.MaxRequestBytes == 0:
		return defaultMaxRequestBytes
	case c.MaxRequestBytes < 0:
		return 0
	default:
		return c.MaxRequestBytes
	}
}

// grpcMaxRecvSize 把请求体上限翻成 grpc-go 的收包上限。
//
// 两边对 0 的解读正好相反：HTTP 侧 limitBody 把 ≤0 当「不限制」，而 grpc-go 里
// MaxRecvMsgSize(0) 就是 0 字节——它的「不限」是 math.MaxInt32。直接复用同一个
// 返回值的话，配了 max_request_bytes: -1（文档承诺的「不限制」）的服务会把 gRPC
// 端口上所有非空请求拒掉，报 received message larger than max (N vs. 0)。
func grpcMaxRecvSize(cfg Config) int {
	if limit := cfg.maxRequestBytes(); limit > 0 {
		return int(limit)
	}
	return math.MaxInt32
}

// handlerTimeout 取生效的处理超时，0 即不限制。
func (c Config) handlerTimeout() time.Duration {
	switch {
	case c.HandlerTimeout == 0:
		return defaultHandlerTimeout
	case c.HandlerTimeout < 0:
		return 0
	default:
		return c.HandlerTimeout
	}
}

// Server 服务的对外传输出口，实现 app.Component。grpc 与 http 哪些生效由所选
// 构造器决定：直连 REST 形态没有 gRPC 服务器，grpc/grpcLn/conn 为 nil；纯 gRPC
// 形态没有 HTTP 出口，http/httpLn 为 nil。
type Server struct {
	grpc   *grpc.Server
	grpcLn net.Listener
	// health gRPC 健康服务，Stop 时置 NOT_SERVING 以触发摘流；纯 HTTP 形态为 nil。
	health *health.Server
	// serving 就绪状态位，/readyz 与 gRPC health 共用同一个事实来源。
	serving atomic.Bool
	// started 是否已进入 Start。Stop 据此决定要不要摘流：别的组件装配失败触发
	// 全量停机时，本组件还没接过一个请求，白等 drain_delay 没有意义。
	started atomic.Bool
	// readiness 依赖就绪检查，由服务经 WithReadinessCheck 注入。
	readiness []ReadinessCheck
	// logger 就绪状态翻转的日志出口，未注入 WithLogger 时为 nil。
	logger *slog.Logger
	// lastReady 上次就绪判定的结果，用于只在状态翻转时记日志。
	lastReady atomic.Bool
	// drainDelayValue 摘流等待时长，见 Config.DrainDelay。
	drainDelayValue time.Duration

	// 以下字段仅在开启 HTTP 出口时非 nil，nil 即纯 gRPC 形态。
	http   *http.Server
	httpLn net.Listener
	// conn 是环回连接，仅 NewRPCRESTServer 形态非 nil；直连形态无环回。
	conn *grpc.ClientConn
	// drain 取消 HTTP 连接的 base ctx，由 Stop 在宽限期耗尽后调用，
	// 让在途 handler 知道该收尾了（见 baseContext）。纯 gRPC 形态为 nil。
	drain context.CancelFunc
}

// NewRPCServer 构造纯 gRPC 形态：只监听 grpc_addr，不开 HTTP。此刻即「起不来就该死」
// 的装配期：端口占用、配置与选项自相矛盾一律 panic。全部注册在返回前完成，
// 调用方拿到的即可运行状态。
func NewRPCServer(cfg Config, opts ...Option) *Server {
	o := newOptions(opts)
	validateRPC(cfg, o)

	grpcLn, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 gRPC %s: %w", cfg.GRPCAddr, err))
	}

	s := newServer(cfg, o)
	s.grpc, s.grpcLn = newGRPCServer(cfg, o, s), grpcLn
	for _, register := range o.services {
		register(s.grpc)
	}
	return s
}

// NewRPCRESTServer 构造双端口形态：gRPC 走 grpc_addr，gateway 转译的 HTTP 出口走
// http_addr，HTTP 流量经环回连接打回本进程 gRPC、穿过与原生 gRPC 相同的拦截链。
// ctx 用于 gateway 路由注册；装配期语义同 NewRPCServer。
func NewRPCRESTServer(ctx context.Context, cfg Config, opts ...Option) *Server {
	o := newOptions(opts)
	validateRPCREST(cfg, o)

	grpcLn, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 gRPC %s: %w", cfg.GRPCAddr, err))
	}

	s := newServer(cfg, o)
	s.grpc, s.grpcLn = newGRPCServer(cfg, o, s), grpcLn
	for _, register := range o.services {
		register(s.grpc)
	}
	s.attachHTTP(ctx, cfg, o)
	return s
}

// NewRESTServer 构造直连 REST 形态：只监听 http_addr，没有 gRPC 服务器——handler
// 实现经 WithDirectGateway 直接注册进 gateway 路由（能力与代价见 WithDirectGateway）。
// 装配期语义同 NewRPCServer。
//
// 传给本函数的 ctx 只用于路由注册，不是请求 handler ctx 的父：grpc-gateway 的
// request_context 生成选项（本仓在 buf.gen.yaml 里显式钉为 true）让生成代码以
// req.Context() 为父，于是客户端断连**会**取消 handler ctx，而停机信号**到不了**
// 在途 handler。要让在途请求感知停机，靠的是 Stop 在宽限期耗尽后取消 http.Server
// 的 BaseContext（见 baseContext），不是这个 ctx。
func NewRESTServer(ctx context.Context, cfg Config, opts ...Option) *Server {
	o := newOptions(opts)
	validateREST(cfg, o)

	s := newServer(cfg, o)
	s.attachDirectHTTP(ctx, cfg, o)
	return s
}

// newServer 建一个就绪状态为「服务中」的 Server 骨架。
func newServer(cfg Config, o *options) *Server {
	s := &Server{readiness: o.readiness, drainDelayValue: cfg.drainDelay(), logger: o.logger}
	s.serving.Store(true)
	s.lastReady.Store(true)
	return s
}

// validateRPC 把「装配起来必然出错」的组合挡在监听端口之前：纯 gRPC 形态。
func validateRPC(cfg Config, o *options) {
	switch {
	case cfg.GRPCAddr == "":
		panic(errors.New("装配 transport: grpc_addr 不能为空——双端口形态用 NewRPCRESTServer"))
	case cfg.HTTPAddr != "":
		panic(errors.New("装配 transport: NewRPCServer 是纯 gRPC 形态却配了 http_addr——双端口形态用 NewRPCRESTServer"))
	case len(o.gateways) > 0:
		panic(errors.New("装配 transport: WithGateway 属双端口形态（NewRPCRESTServer），纯 gRPC 形态没有 HTTP 出口"))
	case len(o.directGateways) > 0:
		panic(errors.New("装配 transport: WithDirectGateway 属直连 REST 形态（NewRESTServer），纯 gRPC 形态没有 HTTP 出口"))
	case len(o.mounts) > 0:
		panic(errors.New("装配 transport: 挂载点需要 HTTP 出口——双端口形态用 NewRPCRESTServer，直连形态用 NewRESTServer"))
	case len(o.middlewares) > 0:
		panic(errors.New("装配 transport: WithMiddleware 是 HTTP 中间件，纯 gRPC 形态没有 HTTP 出口——横切关注点用 WithUnaryInterceptor"))
	}
}

// validateRPCREST 同 validateRPC：双端口形态。
func validateRPCREST(cfg Config, o *options) {
	switch {
	case cfg.GRPCAddr == "":
		panic(errors.New("装配 transport: grpc_addr 不能为空"))
	case cfg.HTTPAddr == "":
		panic(errors.New("装配 transport: NewRPCRESTServer 是双端口形态，http_addr 必配——纯 gRPC 形态用 NewRPCServer"))
	case len(o.gateways) == 0:
		panic(errors.New("装配 transport: 配了 http_addr 却没有 WithGateway，HTTP 端口上不会有任何路由"))
	case len(o.directGateways) > 0:
		panic(errors.New("装配 transport: WithDirectGateway 属直连 REST 形态（NewRESTServer），环回形态的路由用 WithGateway"))
	case len(o.middlewares) > 0:
		panic(errors.New("装配 transport: WithMiddleware 属直连 REST 形态（NewRESTServer）——环回形态的横切关注点写成 gRPC 拦截器经 WithUnaryInterceptor 注入，HTTP 与 gRPC 一并覆盖；只挂 HTTP 中间件会漏掉东西向流量"))
	}
	validateMounts(o.mounts)
}

// validateREST 同 validateRPC：直连 REST 形态。没有 gRPC 服务器，gRPC 侧的选项
// 在这里都是「装配了也无处生效」，一律挡下而非静默忽略。
func validateREST(cfg Config, o *options) {
	switch {
	case cfg.HTTPAddr == "":
		panic(errors.New("装配 transport: NewRESTServer 须配 http_addr"))
	case cfg.GRPCAddr != "":
		panic(errors.New("装配 transport: NewRESTServer 是纯 HTTP 形态却配了 grpc_addr——双端口形态用 NewRPCRESTServer"))
	case len(o.directGateways) == 0:
		panic(errors.New("装配 transport: 没有 WithDirectGateway，HTTP 端口上不会有任何路由"))
	case len(o.gateways) > 0:
		panic(errors.New("装配 transport: WithGateway 属环回双端口形态（NewRPCRESTServer），直连路由用 WithDirectGateway"))
	case len(o.services) > 0:
		panic(errors.New("装配 transport: WithService 注册 gRPC 服务实现，NewRESTServer 没有 gRPC 服务器——handler 经 WithDirectGateway 直接注册"))
	case len(o.unary) > 0 || len(o.stream) > 0:
		panic(errors.New("装配 transport: 自定义 gRPC 拦截器在直连形态没有挂载点——鉴权与访问日志由本包经 middleware 提供，其余横切关注点包在 WithDirectGateway 的路由层实现"))
	case len(o.grpcOptions) > 0:
		panic(errors.New("装配 transport: WithGRPCServerOption 在直连形态没有 gRPC 服务器可配"))
	}
	validateMounts(o.mounts)
}

// validateMounts 校验挂载模式：以 / 开头且不能是根路径——根路径归 gateway。
func validateMounts(mounts []mount) {
	for _, m := range mounts {
		if !strings.HasPrefix(m.pattern, "/") || strings.TrimSuffix(m.pattern, "/") == "" {
			panic(fmt.Errorf("装配 transport: 挂载模式 %q 非法，须以 / 开头且不能是根路径（根路径归 gateway）", m.pattern))
		}
	}
}

// newGRPCServer 按选项装好拦截链、资源上限与内置服务。
func newGRPCServer(cfg Config, o *options, s *Server) *grpc.Server {
	// 出口层错误翻译是仓库级约定，作为链首固定挂上；Chain*Interceptor 多次调用是
	// 追加语义，后面的注入项与自定义拦截器都不会顶掉它。
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(errCodeToStatus),
		grpc.ChainStreamInterceptor(errCodeToStatusStream),
	}
	// 资源上限：grpc-go 的默认值是「单连接流数不限、连接永不回收」，理由见各常量。
	opts = append(opts,
		grpc.MaxRecvMsgSize(grpcMaxRecvSize(cfg)),
		grpc.MaxConcurrentStreams(defaultMaxConcurrentStreams),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     defaultMaxConnectionIdle,
			MaxConnectionAge:      defaultMaxConnectionAge,
			MaxConnectionAgeGrace: defaultMaxConnectionAgeGrace,
		}))
	if o.tracerProvider != nil {
		statsOpts := []otelgrpc.Option{
			otelgrpc.WithTracerProvider(o.tracerProvider),
			// 传播器必须显式给，否则入站 traceparent 读不到（见 tracePropagators）。
			otelgrpc.WithPropagators(tracePropagators),
		}
		if o.meterProvider != nil {
			statsOpts = append(statsOpts, otelgrpc.WithMeterProvider(o.meterProvider))
		}
		opts = append(opts, grpc.StatsHandler(otelgrpc.NewServerHandler(statsOpts...)))
	}
	if timeout := cfg.handlerTimeout(); timeout > 0 {
		// 超时在访问日志之外：日志因此记得到「这次请求是超时失败的」。
		opts = append(opts, grpc.ChainUnaryInterceptor(unaryTimeout(timeout)))
	}
	if o.logger != nil {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(unaryAccessLog(o.logger)),
			grpc.ChainStreamInterceptor(streamAccessLog(o.logger)))
	}
	// recovery 在访问日志之内、鉴权之外：panic 先被转成 error，访问日志才记得到
	// 这次请求（放在日志之外的话 panic 会跳过日志的记录点），而鉴权策略自己 panic
	// 也一并兜住。宪法第 1 条禁止业务路径 panic，但那是对作者的要求，不是运行期的
	// 防线——出口层是唯一能兜住它的地方。
	opts = append(opts,
		grpc.ChainUnaryInterceptor(unaryRecovery(o.logger)),
		grpc.ChainStreamInterceptor(streamRecovery(o.logger)))
	if o.authenticator != nil {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(unaryAuth(o.authenticator)),
			grpc.ChainStreamInterceptor(streamAuth(o.authenticator)))
	}
	if len(o.unary) > 0 {
		opts = append(opts, grpc.ChainUnaryInterceptor(o.unary...))
	}
	if len(o.stream) > 0 {
		opts = append(opts, grpc.ChainStreamInterceptor(o.stream...))
	}
	opts = append(opts, o.grpcOptions...)

	srv := grpc.NewServer(opts...)
	// 健康服务的句柄要留住：Stop 靠它置 NOT_SERVING 触发摘流。原先就地丢弃，
	// 于是实例「永远健康」，连停机中都报 SERVING。
	s.health = health.NewServer()
	healthpb.RegisterHealthServer(srv, s.health)
	if cfg.Reflection {
		reflection.Register(srv) // 免鉴权且吐出完整契约，默认关，见 Config.Reflection
	}
	return srv
}

// attachHTTP 装上环回转译出口：独立监听、环回连接、路由树。
func (s *Server) attachHTTP(ctx context.Context, cfg Config, o *options) {
	addr := cfg.HTTPAddr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 HTTP %s: %w", addr, err))
	}
	s.httpLn = ln

	// 环回拨回本进程的 gRPC 端口，HTTP 流量因此同样穿过全部拦截器。
	// grpc.NewClient 惰性建连，装配期不触网。
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if o.tracerProvider != nil {
		// 环回这一跳也要带上 traceparent，否则 HTTP 入口提取到的 trace 上下文
		// 走不进 gRPC 服务端：服务端的 StatsHandler 从 metadata 里什么都读不到，
		// 于是另起一条 trace，外部调用链在本服务的 HTTP 边界断掉。
		dialOpts = append(dialOpts, grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(o.tracerProvider),
			otelgrpc.WithPropagators(tracePropagators))))
	}
	conn, err := grpc.NewClient(loopbackAddr(s.grpcLn), dialOpts...)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 拨号 gateway 环回: %w", err))
	}
	s.conn = conn

	gwMux := runtime.NewServeMux(
		runtime.WithErrorHandler(gatewayErrorHandler),
		runtime.WithRoutingErrorHandler(routingErrorHandler),
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
	)
	for _, register := range o.gateways {
		if err := register(ctx, gwMux, conn); err != nil {
			panic(fmt.Errorf("装配 transport: 注册 gateway 路由: %w", err))
		}
	}

	// 入站 traceparent 在这里提取：没有它，外部调用方带着 trace 上下文打 HTTP
	// 端口时本服务会另起一条 trace，跨服务链路断开——而双端口形态正是推荐的对外
	// HTTP 形态。直连形态早就有这一层，环回形态原先漏了。
	var handler http.Handler = s.mountMux(gwMux, o.mounts)
	if o.tracerProvider != nil {
		handler = otelhttp.NewHandler(handler, "http",
			otelhttp.WithTracerProvider(o.tracerProvider),
			otelhttp.WithPropagators(tracePropagators),
			// 探针不埋 span、不计指标：默认全采样下每 pod 每分钟十几条探针 span
			// 常驻，而摘流期 /readyz 的 503 会被 otelhttp 计进 http.server.* 的
			// 错误率——每次滚动更新都往 SLI 里灌一批 5xx。
			otelhttp.WithFilter(func(r *http.Request) bool {
				return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
			}),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			}))
	}

	s.http = &http.Server{
		Handler:           limitBody(handler, cfg.maxRequestBytes()),
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
		BaseContext:       s.baseContext(),
	}
}

// attachDirectHTTP 装上直连 REST 出口：gateway 路由直接注册 handler 实现，无 gRPC
// 服务器、无环回；鉴权与访问日志经 middleware 挂在每条路由上（链序见包注释），
// 错误出口由 mux 的错误处理器承担。
func (s *Server) attachDirectHTTP(ctx context.Context, cfg Config, o *options) {
	addr := cfg.HTTPAddr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic(fmt.Errorf("装配 transport: 监听 HTTP %s: %w", addr, err))
	}
	s.httpLn = ln

	// 链序与 gRPC 侧一致：访问日志在外、鉴权在内（先注册者在外层，
	// runtime.chainMiddlewares 的组装语义）；出口翻译是错误处理器，天然在最外。
	var middlewares []runtime.Middleware
	if timeout := cfg.handlerTimeout(); timeout > 0 {
		middlewares = append(middlewares, directTimeout(timeout))
	}
	if o.logger != nil {
		middlewares = append(middlewares, directAccessLog(o.logger))
	}
	// recovery 在访问日志之内、鉴权之外，与 gRPC 侧同位置同理由。没有它时
	// net/http 会在连接层 recover：客户端拿到的是裸 EOF（而非 500），访问日志
	// 一条都没有，堆栈还走标准 log 的 stderr 纯文本——故障被吞成「看不见」。
	middlewares = append(middlewares, directRecovery(o.logger))
	if o.authenticator != nil {
		middlewares = append(middlewares, directAuth(o.authenticator))
	}
	// 自定义中间件排在内置链之内、handler 之外，与 gRPC 侧自定义拦截器的位置一致。
	middlewares = append(middlewares, o.middlewares...)
	gwMux := runtime.NewServeMux(
		runtime.WithErrorHandler(gatewayErrorHandler),
		runtime.WithRoutingErrorHandler(routingErrorHandler),
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
		runtime.WithMiddlewares(middlewares...),
	)
	for _, register := range o.directGateways {
		if err := register(ctx, gwMux); err != nil {
			panic(fmt.Errorf("装配 transport: 注册直连路由: %w", err))
		}
	}

	var handler http.Handler = s.mountMux(gwMux, o.mounts)
	if o.tracerProvider != nil || o.meterProvider != nil {
		otelOpts := []otelhttp.Option{
			otelhttp.WithPropagators(tracePropagators),
			// 探针不埋 span、不计指标：默认全采样下每 pod 每分钟十几条探针 span
			// 常驻，而摘流期 /readyz 的 503 会被 otelhttp 计进 http.server.* 的
			// 错误率——每次滚动更新都往 SLI 里灌一批 5xx。
			otelhttp.WithFilter(func(r *http.Request) bool {
				return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
			}),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + r.URL.Path
			}),
		}
		if o.tracerProvider != nil {
			otelOpts = append(otelOpts, otelhttp.WithTracerProvider(o.tracerProvider))
		}
		if o.meterProvider != nil {
			otelOpts = append(otelOpts, otelhttp.WithMeterProvider(o.meterProvider))
		}
		handler = otelhttp.NewHandler(handler, "http", otelOpts...)
	}
	s.http = &http.Server{
		Handler:           limitBody(handler, cfg.maxRequestBytes()),
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		// 直连形态不支持流式方法，这两个硬上限因此安全（环回形态不能设，见常量注释）。
		ReadTimeout:  defaultDirectReadTimeout,
		WriteTimeout: defaultDirectWriteTimeout,
		IdleTimeout:  defaultIdleTimeout,
		BaseContext:  s.baseContext(),
	}
}

// limitBody 给请求体套大小上限。limit ≤0 即不限制，原样返回。
// 超限时 MaxBytesReader 让后续读取报错，由 gateway 的错误处理器翻成统一错误体。
func limitBody(next http.Handler, limit int64) http.Handler {
	if limit <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// baseContext 给 http.Server 装一个可取消的连接级 ctx，并把 cancel 留在 Server 上。
//
// 请求 ctx 派生自它，于是停机宽限期耗尽后 Stop 取消它，在途 handler 就能观察到
// 「该收尾了」。没有这条通路时，Shutdown 超时返回后 handler 仍在跑、还能写出
// 200，直到进程退出时在任意一行被硬切——「已扣款未记账」正是这么来的。
//
// 注意宽限期内不取消：那会把优雅排空变成当场掐断。
func (s *Server) baseContext() func(net.Listener) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	s.drain = cancel
	return func(net.Listener) context.Context { return ctx }
}

// mountMux 组外层路由树：两个探针、各挂载点、根路径归 gateway。精确与子树两个
// 模式都要注册，否则子路径会落进根路径的 gateway。
//
// 两个探针对应 K8s 的两种语义，不能合成一个：
//   - /healthz 是 liveness「进程还活着吗」。它恒定 200，连停机期也是——
//     liveness 转红会让编排系统强杀进程，而停机中的实例正需要时间排空；
//     依赖不可用同样不该让它转红，重启进程治不好一个挂掉的数据库。
//   - /readyz 是 readiness「现在能接流量吗」。它查就绪状态位（Stop 入口置假，
//     这是摘流的起点）与注入的依赖检查。编排系统据此决定要不要往这里发流量。
//
// 两个探针都免鉴权：拦探针会让编排系统误判实例死亡。
func (s *Server) mountMux(gwMux http.Handler, mounts []mount) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		err := s.ready(r.Context())
		s.logReadinessFlip(r.Context(), err)
		if err != nil {
			// 原因只进日志，不进响应体：这个端点免鉴权，而依赖检查的错误文本会
			// 带连接串细节（pgx 的失败信息里有 user、database、host）——
			// 宪法第 2 条，原始错误绝不进给客户端的消息。
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	for _, mt := range mounts {
		pattern := strings.TrimSuffix(mt.pattern, "/")
		mux.Handle(pattern, mt.handler)
		mux.Handle(pattern+"/", mt.handler)
	}
	mux.Handle("/", gwMux)
	return mux
}

// Name 实现 app.Component。
func (s *Server) Name() string { return "transport" }

// Start 实现 app.Component：阻塞服务，直到 Stop 或某一路出错。
// 停机期各 Serve 的正常收尾由 closedOnStop 过滤，不上报给 app。
func (s *Server) Start(context.Context) error {
	s.started.Store(true)
	switch {
	case s.grpc == nil: // 直连 REST 形态：只有 HTTP 一路
		return closedOnStop(s.http.Serve(s.httpLn))
	case s.http == nil: // 纯 gRPC 形态：只有 gRPC 一路
		return closedOnStop(s.grpc.Serve(s.grpcLn))
	}

	// 双端口形态：两条 Serve 各自阻塞在自己的 listener 上；缓冲等于路数，
	// 故 Start 因某一路出错提前返回时，另一路的写入也不会阻塞在这里。
	errs := make(chan error, 2)
	go func() { errs <- s.grpc.Serve(s.grpcLn) }()
	go func() { errs <- s.http.Serve(s.httpLn) }()
	for range cap(errs) {
		if err := closedOnStop(<-errs); err != nil {
			return err
		}
	}
	return nil
}

// Stop 实现 app.Component：按依赖方向排空在途流量。双端口形态顺序不可换——HTTP
// 请求经环回打到本进程 gRPC，先关环回或先停 gRPC 都会打断在途请求。
// 各 Server 的 Shutdown/GracefulStop 会关掉自己的 listener，新连接随之被拒。
func (s *Server) Stop(ctx context.Context) error {
	// 先摘流：把就绪状态置假（/readyz 转红、gRPC health 置 NOT_SERVING），
	// 等编排系统把本实例从负载均衡摘掉，再关监听。顺序反了就是「收到 TERM
	// 的瞬间拒连」，详见 defaultDrainDelay 的注释。
	s.markNotServing()
	// 没跑起来就不等：装配失败触发的全量停机里，本组件一个请求都没接过。
	if delay := s.drainDelayValue; delay > 0 && s.started.Load() {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			// 宽限期本来就不够了，别把它全耗在等待上。
		}
	}

	var errs []error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("停止 HTTP: %w", err))
			// 宽限期已耗尽而 handler 还在跑：Shutdown 不会掐断它们，
			// 取消 base ctx 让它们自己收尾（见 baseContext）。
			if s.drain != nil {
				s.drain()
			}
		}
		if s.conn != nil {
			if err := s.conn.Close(); err != nil {
				errs = append(errs, fmt.Errorf("关闭 gateway 环回连接: %w", err))
			}
		}
	}
	if s.grpc != nil {
		if err := s.stopGRPC(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// markNotServing 置就绪状态为不可用：HTTP 的 /readyz 与 gRPC health 同时转红。
// 幂等，重复调用无副作用。
func (s *Server) markNotServing() {
	s.serving.Store(false)
	if s.health != nil {
		// Shutdown 把全部已注册服务（含空名的整体状态）置为 NOT_SERVING。
		s.health.Shutdown()
	}
}

// logReadinessFlip 只在就绪状态翻转时记一条。
//
// 探针每隔几秒打一次，失败时每次都记会把真正的故障信息挤出日志；但「什么时候
// 开始不就绪、什么时候恢复」必须看得到——否则依赖不可达这个事件在日志平台里
// 一点痕迹都没有，运维只能靠手动 curl 探针才知道原因。
func (s *Server) logReadinessFlip(ctx context.Context, err error) {
	ready := err == nil
	if s.lastReady.Swap(ready) == ready || s.logger == nil {
		return
	}
	if ready {
		s.logger.InfoContext(ctx, "就绪检查恢复")
		return
	}
	s.logger.WarnContext(ctx, "就绪检查失败", "err", err)
}

// ready 跑就绪判定：先看状态位，再跑注入的依赖检查。
//
// 依赖检查是 readiness 的要点：装配期不 ping 库（DB 比进程晚就绪是常态）这个
// 取舍成立的前提，正是「上层健康检查会 ping」——没有这一环的话，连不上库的实例
// 照样通过就绪探针，编排系统继续往它发流量，每个请求都 500。
func (s *Server) ready(ctx context.Context) error {
	if !s.serving.Load() {
		return errors.New("停机中")
	}
	for _, check := range s.readiness {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return nil
}

// stopGRPC 宽限期内等在途 RPC 收尾，耗尽则强制断开——不肯返回的 GracefulStop
// 会拖死整个停机序列。
func (s *Server) stopGRPC(ctx context.Context) error {
	stopped := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		s.grpc.Stop()
		<-stopped
		return fmt.Errorf("停止 gRPC: 宽限期耗尽，已强制断开: %w", ctx.Err())
	}
}

// closedOnStop 过滤停机期各 Serve 的正常收尾错误：listener 被关闭、两个 Server
// 各自的「已关闭」哨兵——都是 Stop 的预期结果，不是故障。
func closedOnStop(err error) error {
	switch {
	case err == nil,
		errors.Is(err, grpc.ErrServerStopped),
		errors.Is(err, http.ErrServerClosed),
		errors.Is(err, net.ErrClosed):
		return nil
	}
	return err
}

// loopbackAddr 环回拨号地址（127.0.0.1:port）。监听 ":0" 随机端口的场景也能拿到真实端口。
func loopbackAddr(ln net.Listener) string {
	if t, ok := ln.Addr().(*net.TCPAddr); ok {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(t.Port))
	}
	return ln.Addr().String()
}
