package telemetry_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

func TestExporterUnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    telemetry.Exporter
		wantErr bool
	}{
		{name: "none", text: "none", want: telemetry.ExporterNone},
		{name: "otlp", text: "otlp", want: telemetry.ExporterOTLP},
		{name: "stdout", text: "stdout", want: telemetry.ExporterStdout},
		{name: "空串归一到默认 none", text: "", want: telemetry.ExporterNone},
		{name: "拼错当场报错", text: "otpl", wantErr: true},
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

// discardLogger 丢弃全部日志，避免测试输出被构造日志淹没。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func TestMustNewPanicsOnInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  telemetry.Config
	}{
		{name: "Service 为空", cfg: telemetry.Config{}},
		{name: "Exporter 不认识", cfg: telemetry.Config{Service: "user", Exporter: "otpl"}},
		{name: "otlp 缺 Endpoint", cfg: telemetry.Config{Service: "user", Exporter: telemetry.ExporterOTLP}},
		{name: "采样率超出 0~1", cfg: telemetry.Config{Service: "user", SampleRatio: 1.5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("want panic, got 正常返回")
				}
			}()
			tt.cfg.Logger = discardLogger()
			telemetry.MustNew(context.Background(), tt.cfg)
		})
	}
}

// Telemetry 必须满足 pkg/app.Component，否则 cmd 无法把它注册进 app。
// 断言只放在测试里：生产代码靠结构化接口自动满足，不 import pkg/app。
var _ app.Component = (*telemetry.Telemetry)(nil)

func TestExporterNoneProducesNoOutput(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterNone,
		Writer:   &buf,
		Logger:   discardLogger(),
	})

	if got, want := tel.Name(), "telemetry"; got != want {
		t.Errorf("Name got %q, want %q", got, want)
	}
	if err := tel.Start(ctx); err != nil {
		t.Errorf("Start 返回错误: %v", err)
	}

	_, span := tel.TracerProvider().Tracer("test").Start(ctx, "某操作")
	if span.IsRecording() {
		t.Error("Exporter=none 时不该产生记录中的 span")
	}
	span.End()

	if err := tel.Stop(ctx); err != nil {
		t.Errorf("Stop 返回错误: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("Exporter=none 不该有任何输出, got %q", buf.String())
	}
}

func TestStdoutExporterEmitsSpan(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Version:  "v1.2.3",
		Env:      "test",
		Exporter: telemetry.ExporterStdout,
		Writer:   &buf,
		Logger:   discardLogger(),
	})

	_, span := tel.TracerProvider().Tracer("测试埋点").Start(ctx, "取用户")
	if !span.IsRecording() {
		t.Error("默认全采时 span 应处于记录状态")
	}
	span.End()

	// Stop 负责 flush，没有它这批 span 会随进程一起丢掉。
	if err := tel.Stop(ctx); err != nil {
		t.Fatalf("Stop 返回错误: %v", err)
	}

	got := buf.String()
	for _, want := range []string{"取用户", "测试埋点", "user", "v1.2.3", "test"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout 输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
}

func TestMustNewSetsGlobals(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterStdout,
		Writer:   &buf,
		Logger:   discardLogger(),
	})
	defer func() { _ = tel.Stop(ctx) }()

	// 全仓库只有本包调 otel.Set*，第三方埋点库（otelsql 等）靠全局才能工作。
	if otel.GetTracerProvider() != tel.TracerProvider() {
		t.Error("全局 TracerProvider 未被设为本包构造的实例")
	}
	// composite propagator 内含切片、不可比较，只能比对它声明的字段集；
	// 该集合内部用 map 去重，顺序不稳定，故先排序再比。
	if diff := cmp.Diff(slices.Sorted(slices.Values(tel.Propagator().Fields())),
		slices.Sorted(slices.Values(otel.GetTextMapPropagator().Fields()))); diff != "" {
		t.Errorf("全局 propagator 与本包构造的不一致 (-want +got):\n%s", diff)
	}
}

func TestPropagatorSetEvenWhenDisabled(t *testing.T) {
	// 遥测关闭时也必须有 propagator：本服务不采样，但上游传来的 trace context
	// 要原样透传给下游，否则整条链路会在这个服务这里断开。
	tel := telemetry.MustNew(context.Background(), telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterNone,
		Logger:   discardLogger(),
	})

	got := tel.Propagator().Fields()
	for _, want := range []string{"traceparent", "tracestate", "baggage"} {
		if !slices.Contains(got, want) {
			t.Errorf("propagator 缺少字段 %q, got %v", want, got)
		}
	}
}

func TestStdoutExporterEmitsMetrics(t *testing.T) {
	var buf bytes.Buffer
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterStdout,
		Writer:   &buf,
		Logger:   discardLogger(),
	})

	counter, err := tel.MeterProvider().Meter("测试埋点").Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("创建计数器: %v", err)
	}
	counter.Add(ctx, 1)

	// Stop 触发最后一次采集并 flush；没有它这批指标会随进程一起丢掉。
	if err := tel.Stop(ctx); err != nil {
		t.Fatalf("Stop 返回错误: %v", err)
	}

	got := buf.String()
	for _, want := range []string{"test.counter", "go.goroutine.count", "user"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout 输出缺少 %q\n完整输出:\n%s", want, got)
		}
	}
}

func TestStopIsIdempotent(t *testing.T) {
	// app 只会调一次 Stop，但 provider 的 Shutdown 二次调用会报错
	// （PeriodicReader 返回 ErrReaderShutdown），故本包须自己吃掉重复调用。
	var buf bytes.Buffer
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterStdout,
		Writer:   &buf,
		Logger:   discardLogger(),
	})

	if err := tel.Stop(ctx); err != nil {
		t.Fatalf("首次 Stop 返回错误: %v", err)
	}
	if err := tel.Stop(ctx); err != nil {
		t.Errorf("重复 Stop 应为空操作, got %v", err)
	}
}

func TestSamplerFollowsSampledParent(t *testing.T) {
	// ParentBased 的意义：上游已决定采样的请求必须跟随其决定。
	// 采样率压到 1e-9，只有跟随父决定才可能记录——若换成裸的 TraceIDRatioBased，
	// 这个 span 几乎必然被丢弃，链路就在服务边界断成了两截。
	var buf bytes.Buffer
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:     "user",
		Exporter:    telemetry.ExporterStdout,
		Writer:      &buf,
		SampleRatio: 1e-9,
		Logger:      discardLogger(),
	})
	defer func() { _ = tel.Stop(ctx) }()

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("构造 TraceID: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("构造 SpanID: %v", err)
	}
	parent := trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))

	_, span := tel.TracerProvider().Tracer("测试埋点").Start(parent, "下游操作")
	defer span.End()

	if !span.IsRecording() {
		t.Error("上游已采样时，本服务的 span 也应记录（ParentBased 未生效）")
	}
	if got := span.SpanContext().TraceID(); got != traceID {
		t.Errorf("TraceID 未延续, got %v, want %v", got, traceID)
	}
}

func TestOTLPExporterConstructsWithoutCollector(t *testing.T) {
	// otlptracegrpc / otlpmetricgrpc 默认非阻塞建连，故没有 collector 也应构造成功——
	// 否则服务会因为 collector 没起来而拒绝启动，这不是可观测性该有的权重。
	// 本用例只保证 OTLP 分支能走通，不验证真实上报。
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterOTLP,
		Endpoint: "127.0.0.1:4317",
		Insecure: true,
		Logger:   discardLogger(),
	})

	if _, ok := tel.TracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Errorf("OTLP 模式下应构造出 SDK TracerProvider, got %T", tel.TracerProvider())
	}

	// 连不上 collector 时导出会重试，故给 Stop 一个有界 ctx——这正是 pkg/app 的做法。
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := tel.Stop(stopCtx); err != nil {
		t.Logf("无 collector 时 Stop 报错属预期: %v", err)
	}
}

func TestTraceAttrs(t *testing.T) {
	// TraceAttrs 是 pkg/log.Extractor 的实现，把当前 span 的 trace_id / span_id
	// 打进每条日志——日志与链路靠这两个字段关联。
	ctx := context.Background()
	if got := telemetry.TraceAttrs(ctx); got != nil {
		t.Errorf("无 span 时不该产生字段, got %v", got)
	}

	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterStdout,
		Writer:   io.Discard,
		Logger:   discardLogger(),
	})
	defer func() { _ = tel.Stop(ctx) }()

	spanCtx, span := tel.TracerProvider().Tracer("测试埋点").Start(ctx, "某操作")
	defer span.End()

	got := telemetry.TraceAttrs(spanCtx)
	want := []slog.Attr{
		slog.String("trace_id", span.SpanContext().TraceID().String()),
		slog.String("span_id", span.SpanContext().SpanID().String()),
	}
	if diff := cmp.Diff(want, got, cmp.Comparer(func(a, b slog.Attr) bool { return a.Equal(b) })); diff != "" {
		t.Errorf("字段不符 (-want +got):\n%s", diff)
	}
}
