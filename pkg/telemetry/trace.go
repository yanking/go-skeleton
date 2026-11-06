package telemetry

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/yanking/go-skeleton/pkg/log"
)

// newSampler 按配置构造采样器。取值已由 Sampler.UnmarshalText 与 Config.Validate
// 把住（含比例采样的 sample_ratio 范围），故这里的 default 分支只会在绕过配置文件
// 直接在 Go 里构造且写错时触发。
func newSampler(cfg Config) (sdktrace.Sampler, error) {
	switch cfg.Sampler {
	case "", SamplerParentBasedAlwaysOn:
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case SamplerParentBasedRatio:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio)), nil
	case SamplerAlwaysOff:
		return sdktrace.NeverSample(), nil
	default:
		return nil, fmt.Errorf("构造采样器: 未知采样器 %q", cfg.Sampler)
	}
}

// newTracerProvider 构造带批量导出的 TracerProvider；span 在内存攒批异步写出，
// 停机时由 Shutdown 兜底 flush。采样策略由配置给出（默认全采，见包注释）。
func newTracerProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	var exp sdktrace.SpanExporter
	var err error
	switch cfg.Exporter {
	case "", ExporterStdout:
		exp, err = stdouttrace.New()
	case ExporterOTLP:
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exp, err = otlptracegrpc.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("构造 trace 导出器: 未知遥测导出器 %q", cfg.Exporter)
	}
	if err != nil {
		return nil, fmt.Errorf("构造 trace 导出器: %w", err)
	}

	sampler, err := newSampler(cfg)
	if err != nil {
		return nil, err
	}

	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sampler),
	), nil
}

// TraceAttrs 从 ctx 提取当前 span 的 trace_id 与 span_id 附加到日志，
// 无有效 span 时返回 nil。作为 log.Extractor 挂进 pkg/log 后，
// 日志与链路凭同一 trace_id 关联——排障时把两者串起来的钥匙。
var TraceAttrs log.Extractor = func(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	}
}
