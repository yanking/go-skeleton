package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/yanking/go-skeleton/pkg/telemetry"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     telemetry.Config
		wantErr bool
	}{
		{name: "零值即 stdout", cfg: telemetry.Config{}, wantErr: false},
		{name: "stdout 不需要 endpoint", cfg: telemetry.Config{Exporter: telemetry.ExporterStdout}, wantErr: false},
		{name: "otlp 缺 endpoint", cfg: telemetry.Config{Exporter: telemetry.ExporterOTLP}, wantErr: true},
		{name: "otlp 带 endpoint", cfg: telemetry.Config{Exporter: telemetry.ExporterOTLP, Endpoint: "collector:4317"}, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate err got %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestExporterUnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    telemetry.Exporter
		wantErr bool
	}{
		{name: "stdout", text: "stdout", want: telemetry.ExporterStdout},
		{name: "otlp", text: "otlp", want: telemetry.ExporterOTLP},
		{name: "空串归一到默认 stdout", text: "", want: telemetry.ExporterStdout},
		{name: "拼错当场报错", text: "jaeger", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got telemetry.Exporter
			err := got.UnmarshalText([]byte(tt.text))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err got %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("Exporter got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewPanics(t *testing.T) {
	tests := []struct {
		name string
		cfg  telemetry.Config
	}{
		{name: "Service 为空", cfg: telemetry.Config{}},
		{name: "未知导出器", cfg: telemetry.Config{Service: "user", Exporter: "zipkin"}},
		{name: "otlp 缺 endpoint", cfg: telemetry.Config{Service: "user", Exporter: telemetry.ExporterOTLP}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("want panic, got 正常返回")
				}
			}()
			telemetry.New(context.Background(), tt.cfg)
		})
	}
}

func TestComponentLifecycle(t *testing.T) {
	tel := telemetry.New(context.Background(), telemetry.Config{Service: "user"})

	if name := tel.Name(); name != "telemetry" {
		t.Errorf("Name got %q, want %q", name, "telemetry")
	}
	if err := tel.Start(context.Background()); err != nil {
		t.Errorf("Start got %v, want nil", err)
	}
	if err := tel.Stop(context.Background()); err != nil {
		t.Errorf("Stop got %v, want nil", err)
	}
}

func TestTraceAttrs(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01},
		SpanID:  trace.SpanID{0x02},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	attrs := telemetry.TraceAttrs(ctx)
	if len(attrs) != 2 {
		t.Fatalf("带 span 的 ctx 应附加 2 个字段, got %d", len(attrs))
	}
	want := map[string]string{
		"trace_id": "01000000000000000000000000000000",
		"span_id":  "0200000000000000",
	}
	for _, attr := range attrs {
		wantValue, ok := want[attr.Key]
		if !ok {
			t.Errorf("多余字段 %q", attr.Key)
			continue
		}
		if got := attr.Value.String(); got != wantValue {
			t.Errorf("字段 %q got %q, want %q", attr.Key, got, wantValue)
		}
	}

	if got := telemetry.TraceAttrs(context.Background()); got != nil {
		t.Errorf("无 span 的 ctx 应返回 nil, got %v", got)
	}
}

func TestSamplerUnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    telemetry.Sampler
		wantErr bool
	}{
		{name: "全采", text: "parentbased_always_on", want: telemetry.SamplerParentBasedAlwaysOn},
		{name: "比例采样", text: "parentbased_traceidratio", want: telemetry.SamplerParentBasedRatio},
		{name: "一概不采", text: "always_off", want: telemetry.SamplerAlwaysOff},
		{name: "空串归一到默认全采", text: "", want: telemetry.SamplerParentBasedAlwaysOn},
		{name: "拼错当场报错", text: "traceidratio", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got telemetry.Sampler
			err := got.UnmarshalText([]byte(tt.text))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err got %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("Sampler got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestConfigValidateSampling 钉住比例采样的零值不被当默认值放过：
// sample_ratio 为 0 时 TraceIDRatioBased 一条都不采，配了采样却毫无数据且无征兆。
func TestConfigValidateSampling(t *testing.T) {
	tests := []struct {
		name    string
		cfg     telemetry.Config
		wantErr bool
	}{
		{name: "全采不需要比例", cfg: telemetry.Config{Sampler: telemetry.SamplerParentBasedAlwaysOn}, wantErr: false},
		{name: "比例采样缺比例即报错", cfg: telemetry.Config{Sampler: telemetry.SamplerParentBasedRatio}, wantErr: true},
		{name: "比例为负报错", cfg: telemetry.Config{Sampler: telemetry.SamplerParentBasedRatio, SampleRatio: -0.1}, wantErr: true},
		{name: "比例超过 1 报错", cfg: telemetry.Config{Sampler: telemetry.SamplerParentBasedRatio, SampleRatio: 1.5}, wantErr: true},
		{name: "比例合法", cfg: telemetry.Config{Sampler: telemetry.SamplerParentBasedRatio, SampleRatio: 0.05}, wantErr: false},
		{name: "全采时比例为 0 无妨", cfg: telemetry.Config{SampleRatio: 0}, wantErr: false},
		{name: "采集间隔为负报错", cfg: telemetry.Config{MetricInterval: -time.Second}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate err got %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestMeterProviderInjectable 钉住指标 Provider 可被依赖包取用：漏了这个出口，
// 各 instrumentation 只能回落到 otel 的 no-op 全局，指标静默消失。
func TestMeterProviderInjectable(t *testing.T) {
	tel := telemetry.New(context.Background(), telemetry.Config{Service: "svc", Logger: discardLogger()})
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	mp := tel.MeterProvider()
	if mp == nil {
		t.Fatal("MeterProvider 不应为 nil")
	}
	// 能真的造出 Meter 与 instrument 才算接得上。
	if _, err := mp.Meter("probe").Int64Counter("probe.count"); err != nil {
		t.Fatalf("从注入的 Provider 造 instrument: %v", err)
	}
}

// TestErrorHandlerBridgedToSlog 钉住 SDK 内部错误走 slog：不接管就只剩 stderr 上的
// 非结构化文本，而 collector 挂掉时正是最需要看见它的时候。
func TestErrorHandlerBridgedToSlog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	tel := telemetry.New(context.Background(), telemetry.Config{Service: "svc", Logger: logger})
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })

	otel.Handle(errors.New("导出失败标记"))

	if !strings.Contains(buf.String(), "导出失败标记") {
		t.Errorf("SDK 内部错误未进 slog，日志内容: %s", buf.String())
	}
}

// discardLogger 给不关心日志内容的用例用，免得装配日志糊满测试输出。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
