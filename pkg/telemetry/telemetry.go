// Package telemetry 一键装配 OpenTelemetry 的链路追踪与指标：TracerProvider、
// MeterProvider、Go 运行时指标，导出器在 stdout（本地开发直接可见）与
// otlp（gRPC 上报 Collector）之间经配置切换。
//
// Telemetry 实现 app.Component：无常驻循环，Start 直接返回，Stop 即 flush
// 全部未导出数据并释放导出器。把它注册为第一个组件，逆序停止时它最后停，
// 其余组件停机期间产生的 span 与 metric 都还能被记录并导出。
//
// 本包不设 otel 全局（otel.SetTracerProvider 等）：依赖包（pkg/mysql、
// pkg/pgsql、pkg/redis 等）一律经 TracerProvider() 显式拿 Provider 注入，
// 装配关系留在 cmd/initial 一处可见，不藏在全局变量里。
package telemetry

import (
	"context"
	"errors"
	"fmt"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
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

// Config 遥测装配参数，声明式字段由配置文件绑定，注入项标 yaml:"-"。
type Config struct {
	// Service 服务名，写入遥测 resource 的 service.name；装配期注入，必填。
	Service string `yaml:"-"`
	// Exporter 导出器，零值取 ExporterStdout。
	Exporter Exporter `yaml:"exporter"`
	// Endpoint OTLP Collector 的 gRPC 地址（如 collector:4317），
	// Exporter 为 otlp 时必填，其余导出器忽略。
	Endpoint string `yaml:"endpoint"`
	// Insecure otlp 是否明文（跳过 TLS）。本地 Collector 无证书时置 true，
	// 生产走 TLS 保持 false。
	Insecure bool `yaml:"insecure"`
}

// Validate 实现 conf 的校验钩子：otlp 未配 endpoint 在绑定阶段即报错，
// 不拖到进程拉起后。
func (c Config) Validate() error {
	if c.Exporter == ExporterOTLP && c.Endpoint == "" {
		return errors.New("exporter 为 otlp 时必须配置 endpoint")
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

	return &Telemetry{tracer: tracer, meter: meter}
}

// Name 实现 app.Component。
func (t *Telemetry) Name() string { return "telemetry" }

// TracerProvider 暴露内部 Provider，供 pkg/mysql、pkg/pgsql、pkg/redis 等
// 依赖包在装配期做链路追踪注入。Shutdown 之后它退化为 no-op，属预期。
func (t *Telemetry) TracerProvider() trace.TracerProvider { return t.tracer }

// Start 实现 app.Component：遥测没有常驻循环，Provider 已在装配期就绪。
func (t *Telemetry) Start(context.Context) error { return nil }

// Stop 实现 app.Component：flush 全部未导出的 span 与 metric 并释放导出器。
func (t *Telemetry) Stop(ctx context.Context) error { return t.Shutdown(ctx) }

// Shutdown 关闭两个 Provider；任一失败都不遮蔽另一个的错误，聚合返回。
func (t *Telemetry) Shutdown(ctx context.Context) error {
	return errors.Join(t.tracer.Shutdown(ctx), t.meter.Shutdown(ctx))
}
