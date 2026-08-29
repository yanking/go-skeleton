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

// newTracerProvider 构造带批量导出的 TracerProvider；span 在内存攒批异步写出，
// 停机时由 Shutdown 兜底 flush。
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

	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp),
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
