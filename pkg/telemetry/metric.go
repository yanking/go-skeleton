package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// mustMeterProvider 造 MeterProvider；exporter 已由 MustNew 校验过，不会是 ExporterNone。
func mustMeterProvider(ctx context.Context, cfg Config, exporter Exporter, res *resource.Resource) *metric.MeterProvider {
	var reader metric.Reader
	switch exporter {
	case ExporterStdout:
		exp, err := stdoutmetric.New(stdoutmetric.WithWriter(writerOf(cfg)))
		if err != nil {
			panic(fmt.Errorf("构造 Telemetry: 创建 stdout metric 导出器: %w", err))
		}
		reader = metric.NewPeriodicReader(exp)
	case ExporterOTLP:
		exp, err := otlpmetricgrpc.New(ctx, otlpMetricOptions(cfg)...)
		if err != nil {
			panic(fmt.Errorf("构造 Telemetry: 创建 OTLP metric 导出器: %w", err))
		}
		reader = metric.NewPeriodicReader(exp)
	}
	return metric.NewMeterProvider(metric.WithResource(res), metric.WithReader(reader))
}

// otlpMetricOptions 组装 OTLP/gRPC 导出器选项。
func otlpMetricOptions(cfg Config) []otlpmetricgrpc.Option {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	return opts
}

// mustStartRuntimeMetrics 注册 Go runtime 指标（goroutine 数、GC、堆等）。
// 这些指标由 MeterProvider 持有，随其 Shutdown 一并停止，无需单独关闭。
func mustStartRuntimeMetrics(mp *metric.MeterProvider) {
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		panic(fmt.Errorf("构造 Telemetry: 注册 runtime 指标: %w", err))
	}
}
