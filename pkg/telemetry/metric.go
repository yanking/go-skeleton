package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// newMeterProvider 构造周期采集（默认 60s）的 MeterProvider；
// 停机时 Shutdown 会做最后一次采集并导出，短生命周期进程的数据不丢。
func newMeterProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdkmetric.MeterProvider, error) {
	var exp sdkmetric.Exporter
	var err error
	switch cfg.Exporter {
	case "", ExporterStdout:
		exp, err = stdoutmetric.New()
	case ExporterOTLP:
		opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		exp, err = otlpmetricgrpc.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("构造 metric 导出器: 未知遥测导出器 %q", cfg.Exporter)
	}
	if err != nil {
		return nil, fmt.Errorf("构造 metric 导出器: %w", err)
	}

	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
	), nil
}
