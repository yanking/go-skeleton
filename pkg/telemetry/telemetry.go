// Package telemetry 构造服务的可观测性设施：链路追踪与指标。
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Exporter 遥测数据的导出方式。
type Exporter string

const (
	// ExporterNone 完全关闭导出，Exporter 零值即此项；测试与本地默认。
	ExporterNone Exporter = "none"
	// ExporterOTLP 经 OTLP/gRPC 发往 collector。
	ExporterOTLP Exporter = "otlp"
	// ExporterStdout 打到 Config.Writer，本地调试用。
	ExporterStdout Exporter = "stdout"
)

// UnmarshalText 实现 encoding.TextUnmarshaler，使配置文件里的拼写错误在绑定阶段
// （conf.MustLoad）就暴露，而不必等到 MustNew。空串归一到 ExporterNone。
func (e *Exporter) UnmarshalText(text []byte) error {
	switch exporter := Exporter(text); exporter {
	case "", ExporterNone:
		*e = ExporterNone
	case ExporterOTLP:
		*e = ExporterOTLP
	case ExporterStdout:
		*e = ExporterStdout
	default:
		return fmt.Errorf("未知导出方式 %q，只支持 %q、%q 与 %q", exporter, ExporterNone, ExporterOTLP, ExporterStdout)
	}
	return nil
}

// Config 可观测性构造参数。
type Config struct {
	// Service 服务名，写入 resource 的 service.name；必填。
	Service string
	// Exporter 导出方式，零值取 ExporterNone。
	Exporter Exporter
	// Endpoint Exporter 为 ExporterOTLP 时的 collector 地址，如 localhost:4317；此时必填。
	Endpoint string
	// Insecure OTLP 是否走明文，集群内直连 collector 时通常为 true。
	Insecure bool
	// Version 服务版本，写入 resource 的 service.version；可空。
	Version string
	// Env 部署环境，写入 resource 的 deployment.environment.name；可空。
	Env string
	// SampleRatio 采样率，取值 0~1，零值取 1（全采）。
	// 要关闭遥测请用 Exporter=none，不要把采样率设 0——关闭只有一个开关。
	SampleRatio float64
	// Writer Exporter 为 ExporterStdout 时的输出目标，nil 时用 os.Stdout。
	Writer io.Writer
	// Logger 构造与停机日志，nil 时用 slog.Default()。
	Logger *slog.Logger
}

// Telemetry 持有 trace 与 metric 的 provider，并在停机时把缓冲数据 flush 出去。
// 它的方法集满足 pkg/app.Component，可直接注册进 app.New。
type Telemetry struct {
	logger         *slog.Logger
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	propagator     propagation.TextMapPropagator
	// shutdowns 各 provider 的关闭函数，按构造顺序追加、停机时逆序执行。
	shutdowns []func(context.Context) error
}

// MustNew 按 cfg 构造可观测性设施。配置非法直接 panic——装配期起不来就死。
// ctx 用于建立导出器连接，由调用方（cmd）给出。
func MustNew(ctx context.Context, cfg Config) *Telemetry {
	if cfg.Service == "" {
		panic(errors.New("构造 Telemetry: Service 不能为空"))
	}
	exporter := cfg.Exporter
	if err := exporter.UnmarshalText([]byte(cfg.Exporter)); err != nil {
		panic(fmt.Errorf("构造 Telemetry: %w", err))
	}
	if exporter == ExporterOTLP && cfg.Endpoint == "" {
		panic(errors.New("构造 Telemetry: Exporter 为 otlp 时 Endpoint 必填"))
	}
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		panic(fmt.Errorf("构造 Telemetry: 采样率 %v 超出 0~1", cfg.SampleRatio))
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// W3C TraceContext + Baggage：跨服务传递 trace 上下文的标准载体。
	// 即便遥测关闭也要设——本服务不采样，但上游传来的 trace context 仍须原样透传，
	// 否则整条链路会在这个服务这里断开（noop tracer 会保留 ctx 中的 SpanContext）。
	propagator := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	otel.SetTextMapPropagator(propagator)

	t := &Telemetry{
		logger:         logger,
		tracerProvider: tracenoop.NewTracerProvider(),
		meterProvider:  metricnoop.NewMeterProvider(),
		propagator:     propagator,
	}
	if exporter == ExporterNone {
		// 不创建任何 provider，全局与访问器都保持 noop——关闭遥测就该是零开销。
		logger.Info("可观测性已关闭", "component", "telemetry")
		return t
	}

	res := mustResource(cfg)
	tp := mustTracerProvider(ctx, cfg, exporter, res)
	t.tracerProvider = tp
	t.shutdowns = append(t.shutdowns, tp.Shutdown)
	otel.SetTracerProvider(tp)

	mp := mustMeterProvider(ctx, cfg, exporter, res)
	mustStartRuntimeMetrics(mp)
	t.meterProvider = mp
	t.shutdowns = append(t.shutdowns, mp.Shutdown)
	otel.SetMeterProvider(mp)

	logger.Info("可观测性已就绪", "component", "telemetry", "exporter", exporter, "endpoint", cfg.Endpoint)
	return t
}

// TracerProvider 返回本包构造的 TracerProvider，供需要显式注入的埋点库使用
// （如 otelgrpc.WithTracerProvider）。Exporter=none 时返回 noop 实现，永不为 nil。
func (t *Telemetry) TracerProvider() trace.TracerProvider { return t.tracerProvider }

// MeterProvider 返回本包构造的 MeterProvider，供需要显式注入的埋点库使用
// （如 otelgrpc.WithMeterProvider）。Exporter=none 时返回 noop 实现，永不为 nil。
func (t *Telemetry) MeterProvider() metric.MeterProvider { return t.meterProvider }

// Propagator 返回本包构造的跨进程上下文传播器（W3C TraceContext + Baggage），
// 供需要显式注入的埋点库使用（如 otelgrpc.WithPropagators）。遥测关闭时同样有效。
func (t *Telemetry) Propagator() propagation.TextMapPropagator { return t.propagator }

// writerOf 取 stdout 导出器的输出目标。
func writerOf(cfg Config) io.Writer {
	if cfg.Writer != nil {
		return cfg.Writer
	}
	return os.Stdout
}

// Name 实现 pkg/app.Component。
func (t *Telemetry) Name() string { return "telemetry" }

// Start 实现 pkg/app.Component。可观测性是资源型组件，没有常驻循环，直接返回。
func (t *Telemetry) Start(context.Context) error { return nil }

// Stop 逆序关闭各 provider，关闭过程本身会把缓冲中的数据 flush 出去。
// 单个 provider 失败不中断其余关闭——跳过等于丢数据。重复调用是安全的空操作。
func (t *Telemetry) Stop(ctx context.Context) error {
	var errs []error
	for i := len(t.shutdowns) - 1; i >= 0; i-- {
		if err := t.shutdowns[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	t.shutdowns = nil
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("关闭可观测性: %w", err)
	}
	return nil
}

// TraceAttrs 从 ctx 取出当前 span 的 trace_id 与 span_id，供 pkg/log.Extractor 使用：
//
//	log.MustNew(log.Config{..., Extractors: []log.Extractor{telemetry.TraceAttrs}})
//
// 无有效 span 时返回 nil，日志里就不会出现这两个字段——而不是一串全零，
// 后者会让「这条日志属于哪条链路」这个问题得到一个看似有答案的错误答案。
//
// 它放在本包而非 pkg/log，是为了让 pkg/log 保持零第三方依赖：
// 日志包不该因为「可能会接链路追踪」而钉上整棵 OTel 依赖树。
func TraceAttrs(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	}
}
