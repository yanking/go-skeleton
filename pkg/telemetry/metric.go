package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// defaultMetricInterval 指标采集与导出的默认间隔，与 OTel SDK 自身的默认值一致。
const defaultMetricInterval = 60 * time.Second

// metricInterval 取生效的采集间隔，零值归一到默认值。
func metricInterval(cfg Config) time.Duration {
	if cfg.MetricInterval <= 0 {
		return defaultMetricInterval
	}
	return cfg.MetricInterval
}

// newMeterProvider 构造周期采集的 MeterProvider；
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
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithInterval(metricInterval(cfg)))),
	), nil
}
