// Package telemetry 一键装配 OpenTelemetry 的链路追踪与指标：TracerProvider、
// MeterProvider、Go 运行时指标，导出器在 stdout（本地开发直接可见）与
// otlp（gRPC 上报 Collector）之间经配置切换。
//
// Telemetry 实现 app.Component：无常驻循环，Start 直接返回，Stop 即 flush
// 全部未导出数据并释放导出器。把它注册为第一个组件，逆序停止时它最后停，
// 其余组件停机期间产生的 span 与 metric 都还能被记录并导出。
//
// 本包不设 otel 的 Provider 全局（otel.SetTracerProvider 等）：依赖包（pkg/transport、
// pkg/mysql、pkg/pgsql、pkg/redis、pkg/httpc 等）一律经 TracerProvider() 与
// MeterProvider() 显式拿 Provider 注入，装配关系留在 cmd/initial 一处可见，
// 不藏在全局变量里。代价要知道：instrumentation 库在没拿到 Provider 时回落到
// otel 的 no-op 全局，于是**漏注入不报错、只是那部分遥测静默消失**。各包因此在
// 未拿到 Provider 时记一条「未启用」的装配日志，让漏注入在启动日志里看得见。
//
// 唯一的 otel 全局例外是 otel.SetErrorHandler（go-style 硬规则 6 要求写明代价）：
// SDK 内部错误（导出失败、队列溢出）只有这一个出口，不接管就会以非结构化文本直落
// stderr——JSON 采集链路会把它当噪音丢掉，而 collector 挂掉时正是最需要看见它的时候。
// 代价是进程级可变状态：多次 New 后者覆盖前者，测试里需要时自行保存/还原。
//
// 采样与丢弃行为（生产前必读）：Sampler 零值全采，高 QPS 服务须显式配
// parentbased_traceidratio + sample_ratio，否则导出带宽与后端成本按 QPS 线性涨。
// span 走批量导出（SDK 默认队列 2048、批 512、5s 一刷），队列满了**丢弃最新的 span**
// 并经 ErrorHandler 报错；这组批参数本包不开放配置，要改先想清楚为什么。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Exporter 遥测数据导出器。
type Exporter string

const (
	// ExporterStdout 打到标准输出，Exporter 零值即此项，本地开发肉眼可读。
	ExporterStdout Exporter = "stdout"
	// ExporterOTLP 经 gRPC 上报 OTLP Collector，生产用。
	ExporterOTLP Exporter = "otlp"
)

// UnmarshalText 实现 encoding.TextUnmarshaler，使配置文件里的导出器拼写错误
// 在绑定阶段（conf.MustLoad）就暴露，与 pkg/log 的 Format 行为一致。
// 空串归一到 ExporterStdout，与零值的含义保持一致。
func (e *Exporter) UnmarshalText(text []byte) error {
	switch exporter := Exporter(text); exporter {
	case "", ExporterStdout:
		*e = ExporterStdout
	case ExporterOTLP:
		*e = ExporterOTLP
	default:
		return fmt.Errorf("未知遥测导出器 %q，只支持 %q 与 %q", exporter, ExporterStdout, ExporterOTLP)
	}
	return nil
}

// Sampler 链路采样策略。取值与 OTel 标准环境变量 OTEL_TRACES_SAMPLER 一致，
// 便于与别的语言栈、与 collector 侧的配置对照着读。
type Sampler string

const (
	// SamplerParentBasedAlwaysOn 尊重上游采样决定，无上游时全采；Sampler 零值即此项。
	// 本地开发与低 QPS 服务用它；高 QPS 生产服务改 SamplerParentBasedRatio。
	SamplerParentBasedAlwaysOn Sampler = "parentbased_always_on"
	// SamplerParentBasedRatio 尊重上游采样决定，无上游时按 SampleRatio 比例采。
	// 这是生产常用项：链路要么整条采、要么整条不采，不会断成碎片。
	SamplerParentBasedRatio Sampler = "parentbased_traceidratio"
	// SamplerAlwaysOff 一概不采。只用于明确不需要链路的场景（如压测本身）；
	// 注意它连上游已采样的链路也不续，排障时会看到链路在本服务断掉。
	SamplerAlwaysOff Sampler = "always_off"
)

// UnmarshalText 实现 encoding.TextUnmarshaler，使配置文件里的采样器拼写错误在绑定阶段
// 就暴露，与 Exporter 一致。空串归一到 SamplerParentBasedAlwaysOn，与零值含义一致。
func (s *Sampler) UnmarshalText(text []byte) error {
	switch sampler := Sampler(text); sampler {
	case "", SamplerParentBasedAlwaysOn:
		*s = SamplerParentBasedAlwaysOn
	case SamplerParentBasedRatio:
		*s = SamplerParentBasedRatio
	case SamplerAlwaysOff:
		*s = SamplerAlwaysOff
	default:
		return fmt.Errorf("未知采样器 %q，只支持 %q、%q 与 %q",
			sampler, SamplerParentBasedAlwaysOn, SamplerParentBasedRatio, SamplerAlwaysOff)
	}
	return nil
}

// Config 遥测装配参数，声明式字段由配置文件绑定，注入项标 yaml:"-"。
type Config struct {
	// Service 服务名，写入遥测 resource 的 service.name；装配期注入，必填。
	Service string `yaml:"-"`
	// Exporter 导出器，零值取 ExporterStdout。
	//
	// 注意零值是**本地开发值**：整段 telemetry 漏下发时，全部 span 与 metric 会以
	// JSON 打进 stdout，叠加零值的全采样后日志量按 QPS 爆炸，还会把真日志挤出采集配额。
	// 生产的配置文件必须显式写 otlp——new-service 渲染的 yaml 已带这一行。
	Exporter Exporter `yaml:"exporter"`
	// Sampler 采样策略，零值取 SamplerParentBasedAlwaysOn（全采）。
	Sampler Sampler `yaml:"sampler"`
	// SampleRatio 采样比例，Sampler 为 parentbased_traceidratio 时必填，取值 (0,1]。
	// 零值在该策略下恰好意味着「一条都不采」——那是最危险的失败开放形态
	// （配了采样却什么都没采，且毫无征兆），故 Validate 把它判为错误而非默认值。
	SampleRatio float64 `yaml:"sample_ratio"`
	// MetricInterval 指标采集与导出间隔，零值取 60s（SDK 默认）。
	// 与后端的抓取/接收周期对齐：配得比后端周期长，后端会看到重复的旧值。
	MetricInterval time.Duration `yaml:"metric_interval"`
	// Logger 遥测自身的日志出口：装配结果与 SDK 内部错误（导出失败、队列溢出）
	// 都走它。nil 时用 slog.Default()。
	Logger *slog.Logger `yaml:"-"`
	// Endpoint OTLP Collector 的 gRPC 地址（如 collector:4317），
	// Exporter 为 otlp 时必填，其余导出器忽略。
	Endpoint string `yaml:"endpoint"`
	// Insecure otlp 是否明文（跳过 TLS）。本地 Collector 无证书时置 true，
	// 生产走 TLS 保持 false。
	Insecure bool `yaml:"insecure"`
}

// sampler 取归一后的采样策略。直接在 Go 里构造 Config 时 Sampler 是空串
// （不经 UnmarshalText），各处各自判断空串迟早漏一处——与 pkg/redis 的
// Config.mode() 同一形状，零值只在这里归一一次。
func (c Config) sampler() Sampler {
	if c.Sampler == "" {
		return SamplerParentBasedAlwaysOn
	}
	return c.Sampler
}

// Validate 实现 conf 的校验钩子：otlp 未配 endpoint 即报错。New 必调（装配期）；
// 要提前到绑定阶段，由服务 Config 的 Validate 委托到此（conf 只调顶层）。
func (c Config) Validate() error {
	if c.Exporter == ExporterOTLP && c.Endpoint == "" {
		return errors.New("exporter 为 otlp 时必须配置 endpoint")
	}
	// 比例采样的零值就是「一条不采」，不能当默认值放过：配了采样却毫无数据，
	// 而且没有任何征兆，比直接起不来难查得多。
	if c.sampler() == SamplerParentBasedRatio && (c.SampleRatio <= 0 || c.SampleRatio > 1) {
		return fmt.Errorf("sampler 为 %s 时 sample_ratio 必须在 (0,1] 内，当前 %v",
			SamplerParentBasedRatio, c.SampleRatio)
	}
	if c.MetricInterval < 0 {
		return fmt.Errorf("metric_interval 不能为负，当前 %v", c.MetricInterval)
	}
	return nil
}

// Telemetry 持有 trace 与 metric 的 Provider，实现 app.Component。
type Telemetry struct {
	tracer *sdktrace.TracerProvider
	meter  *sdkmetric.MeterProvider
}

// New 按 cfg 装配遥测。装配期错误（Service 为空、配置校验不过、导出器建不起来、
// 运行时指标注册失败）直接 panic——遥测起不来的进程不该带着观测盲区继续跑。
// 配置校验复用 Validate：直接在 Go 里构造（不经配置文件）时同样的规则也生效，
// 免得 otlp 缺 endpoint 被导出器静默吞成默认地址。
func New(ctx context.Context, cfg Config) *Telemetry {
	if cfg.Service == "" {
		panic(errors.New("装配遥测: Service 不能为空"))
	}
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配遥测: %w", err))
	}

	// resource 标注每条遥测数据的来源：默认属性（SDK、进程等）之上叠加服务名。
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(cfg.Service)))
	if err != nil {
		panic(fmt.Errorf("装配遥测: 合并 resource: %w", err))
	}

	tracer, err := newTracerProvider(ctx, cfg, res)
	if err != nil {
		panic(fmt.Errorf("装配遥测: %w", err))
	}
	meter, err := newMeterProvider(ctx, cfg, res)
	if err != nil {
		panic(fmt.Errorf("装配遥测: %w", err))
	}

	// Go 运行时指标（GC、内存、goroutine 等）挂到同一个 MeterProvider，随其导出。
	if err := otelruntime.Start(otelruntime.WithMeterProvider(meter)); err != nil {
		panic(fmt.Errorf("装配遥测: 启动运行时指标: %w", err))
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// SDK 内部错误接管到 slog（包注释交代了这个全局例外的代价）：collector 挂掉、
	// 批队列溢出都只从这里出声，不接管就只剩 stderr 上的非结构化文本。
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Error("遥测 SDK 内部错误", "err", err)
	}))
	// 装配结果记一条：导出器与采样策略配错时，启动日志里能直接看出来，
	// 不必等到「后端怎么没数据」才回头查配置。
	// service 不重复记：logger 已经带着它（pkg/log 的 New 固定附加），
	// 再写一次会让这条 JSON 出现两个同名键。
	logger.Info("遥测已装配",
		"exporter", cfg.Exporter,
		"sampler", cfg.sampler(),
		"sample_ratio", cfg.SampleRatio,
		"metric_interval", metricInterval(cfg).String())

	return &Telemetry{tracer: tracer, meter: meter}
}

// Name 实现 app.Component。
func (t *Telemetry) Name() string { return "telemetry" }

// TracerProvider 暴露内部 Provider，供 pkg/mysql、pkg/pgsql、pkg/redis 等
// 依赖包在装配期做链路追踪注入。Shutdown 之后它退化为 no-op，属预期。
func (t *Telemetry) TracerProvider() trace.TracerProvider { return t.tracer }

// MeterProvider 暴露内部 Provider，供 pkg/transport、pkg/mysql、pkg/pgsql、pkg/redis、
// pkg/httpc 等依赖包在装配期做指标注入——请求量/错误率/延迟与连接池饱和度都靠它，
// 漏注入则那部分指标静默缺失（见包注释）。Shutdown 之后退化为 no-op，属预期。
func (t *Telemetry) MeterProvider() otelmetric.MeterProvider { return t.meter }

// Meter 返回挂在进程统一 MeterProvider 上的业务指标 Meter（stdout/otlp 双导出），
// 供业务层在装配期经注入使用（服务自定义指标不要自建 Provider，也别碰 otel 全局，
// 与 TracerProvider 同一约定）。Shutdown 之后退化为 no-op，属预期。
func (t *Telemetry) Meter(name string) otelmetric.Meter { return t.meter.Meter(name) }

// Start 实现 app.Component：遥测没有常驻循环，Provider 已在装配期就绪。
func (t *Telemetry) Start(context.Context) error { return nil }

// Stop 实现 app.Component：flush 全部未导出的 span 与 metric 并释放导出器。
func (t *Telemetry) Stop(ctx context.Context) error { return t.Shutdown(ctx) }

// Shutdown 关闭两个 Provider；任一失败都不遮蔽另一个的错误，聚合返回。
func (t *Telemetry) Shutdown(ctx context.Context) error {
	return errors.Join(t.tracer.Shutdown(ctx), t.meter.Shutdown(ctx))
}
