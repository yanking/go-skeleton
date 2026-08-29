package telemetry_test

import (
	"context"
	"testing"

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
