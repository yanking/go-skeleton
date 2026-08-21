package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// mustResource 组装 resource：它描述「是谁在上报」，会附到每一条 span 与 metric 上。
// semconv 版本必须与 sdk/resource 内部使用的一致，否则 Merge 会因 schema URL 冲突而失败。
func mustResource(cfg Config) *resource.Resource {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.Service)}
	if cfg.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.Version))
	}
	if cfg.Env != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(cfg.Env))
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, attrs...))
	if err != nil {
		panic(fmt.Errorf("构造 Telemetry: 组装 resource: %w", err))
	}
	return res
}

// mustTracerProvider 造 TracerProvider；exporter 已由 MustNew 校验过，不会是 ExporterNone。
func mustTracerProvider(ctx context.Context, cfg Config, exporter Exporter, res *resource.Resource) *sdktrace.TracerProvider {
	ratio := cfg.SampleRatio
	if ratio == 0 {
		ratio = 1
	}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		// ParentBased 是关键：上游已决定采样的请求必须跟随其决定，
		// 否则同一条链路会在服务边界断成两截，后端里看到的全是残缺的半截 trace。
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	}

	switch exporter {
	case ExporterStdout:
		exp, err := stdouttrace.New(stdouttrace.WithWriter(writerOf(cfg)))
		if err != nil {
			panic(fmt.Errorf("构造 Telemetry: 创建 stdout trace 导出器: %w", err))
		}
		// 本地调试要的是「立刻看见」，故同步导出，不攒批。
		opts = append(opts, sdktrace.WithSyncer(exp))
	case ExporterOTLP:
		exp, err := otlptracegrpc.New(ctx, otlpTraceOptions(cfg)...)
		if err != nil {
			panic(fmt.Errorf("构造 Telemetry: 创建 OTLP trace 导出器: %w", err))
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	return sdktrace.NewTracerProvider(opts...)
}

// otlpTraceOptions 组装 OTLP/gRPC 导出器选项。
func otlpTraceOptions(cfg Config) []otlptracegrpc.Option {
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return opts
}
