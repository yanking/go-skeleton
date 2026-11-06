package redis_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/yanking/go-skeleton/pkg/redis"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     redis.Config
		wantErr bool
	}{
		{name: "单地址即单机", cfg: redis.Config{Addrs: []string{"a:6379"}}, wantErr: false},
		{name: "单机可选库", cfg: redis.Config{Addrs: []string{"a:6379"}, DB: 2}, wantErr: false},
		{name: "地址缺失", cfg: redis.Config{}, wantErr: true},
		{name: "pool_size 为负", cfg: redis.Config{Addrs: []string{"a:6379"}, PoolSize: -1}, wantErr: true},
		{name: "负时长", cfg: redis.Config{Addrs: []string{"a:6379"}, DialTimeout: -1}, wantErr: true},
		// 形态必须由部署方表态：按地址数推断会把独立的单机地址静默当成集群。
		{
			name:    "多地址未声明形态即报错",
			cfg:     redis.Config{Addrs: []string{"a:6379", "b:6379"}},
			wantErr: true,
		},
		{
			name:    "多地址声明集群",
			cfg:     redis.Config{Addrs: []string{"a:6379", "b:6379"}, Mode: redis.ModeCluster},
			wantErr: false,
		},
		{
			name:    "集群不许选库",
			cfg:     redis.Config{Addrs: []string{"a:6379", "b:6379"}, Mode: redis.ModeCluster, DB: 2},
			wantErr: true,
		},
		{
			name:    "单地址集群成立（托管 Redis 的配置端点）",
			cfg:     redis.Config{Addrs: []string{"cfg-endpoint:6379"}, Mode: redis.ModeCluster},
			wantErr: false,
		},
		{
			name:    "standalone 不许多地址",
			cfg:     redis.Config{Addrs: []string{"a:6379", "b:6379"}, Mode: redis.ModeStandalone},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate err got %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("want panic, got 正常返回")
		}
	}()
	redis.New(redis.Config{})
}

func TestStandaloneRoundtrip(t *testing.T) {
	mr := miniredis.RunT(t)
	r := redis.New(redis.Config{Addrs: []string{mr.Addr()}})

	if name := r.Name(); name != "redis" {
		t.Errorf("Name got %q, want %q", name, "redis")
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start got %v, want nil", err)
	}

	ctx := context.Background()
	if err := r.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatalf("Set got %v, want nil", err)
	}
	if got, err := r.Get(ctx, "k").Result(); err != nil || got != "v" {
		t.Fatalf("Get got (%q, %v), want (v, nil)", got, err)
	}

	if err := r.Stop(context.Background()); err != nil {
		t.Errorf("Stop got %v, want nil", err)
	}
}

// 集群客户端惰性发现分片：构造与关闭都不触网，无需真实集群即可验证契约。
func TestClusterLifecycle(t *testing.T) {
	r := redis.New(redis.Config{
		Addrs: []string{"127.0.0.1:6379", "127.0.0.1:6380"},
		Mode:  redis.ModeCluster,
	})

	if name := r.Name(); name != "redis" {
		t.Errorf("Name got %q, want %q", name, "redis")
	}
	if err := r.Start(context.Background()); err != nil {
		t.Errorf("Start got %v, want nil", err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Errorf("Stop got %v, want nil", err)
	}
}

func TestCommandLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mr := miniredis.RunT(t)
	r := redis.New(redis.Config{Addrs: []string{mr.Addr()}, Logger: logger})

	ctx := context.Background()
	_ = r.Set(ctx, "k", "v", 0).Err()
	// 查空键返回 redis.Nil,属正常返回,不进 Error。
	if err := r.Get(ctx, "missing").Err(); !errors.Is(err, goredis.Nil) {
		t.Fatalf("查空键应返回 redis.Nil, got %v", err)
	}
	// 未知命令真实报错,进 Error。
	_ = r.Do(ctx, "NOSUCHCOMMAND").Err()

	out := buf.String()
	if !strings.Contains(out, "msg=命令 name=set") {
		t.Errorf("常规命令应有 Debug 日志, got %q", out)
	}
	if !strings.Contains(out, "msg=命令 name=get") {
		t.Errorf("查空键命令应有 Debug 日志, got %q", out)
	}
	if !strings.Contains(out, "msg=命令执行失败 name=nosuchcommand") {
		t.Errorf("失败命令应有 Error 日志, got %q", out)
	}
	// 业务错误只此一条:查空键与握手期内部命令(client/setinfo 等)都不进 Error。
	if strings.Count(out, "msg=命令执行失败") != 1 {
		t.Errorf("Error 日志应恰好一条, got %q", out)
	}
	// 键名与参数不进日志。
	if strings.Contains(out, "missing") {
		t.Errorf("命令参数不进日志, got %q", out)
	}
}

func TestModeUnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    redis.Mode
		wantErr bool
	}{
		{name: "auto", text: "auto", want: redis.ModeAuto},
		{name: "standalone", text: "standalone", want: redis.ModeStandalone},
		{name: "cluster", text: "cluster", want: redis.ModeCluster},
		{name: "空串归一到 auto", text: "", want: redis.ModeAuto},
		{name: "拼错当场报错", text: "sentinel", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got redis.Mode
			err := got.UnmarshalText([]byte(tt.text))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err got %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("Mode got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestModeZeroValueNotBypassingChecks 钉住零值不绕过校验：直接在 Go 里构造 Config
// 时 Mode 是空串、不经 UnmarshalText，若校验拿 c.Mode 直接比对 ModeAuto，
// 空串会既不等于 auto 也不等于 cluster，于是多地址与选库的检查一并跳过。
func TestModeZeroValueNotBypassingChecks(t *testing.T) {
	// Mode 留零值（空串）＋ 多地址：必须仍被拒。
	cfg := redis.Config{Addrs: []string{"a:6379", "b:6379"}}
	if cfg.Mode != "" {
		t.Fatalf("前提不成立：Mode 零值应为空串，got %q", cfg.Mode)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("Mode 为零值且多地址时必须报错，否则零值绕过了全部形态校验")
	}
}

// TestSpanOmitsCommandArgs 钉住命令参数不进 span：redisotel 默认把整条命令写进
// db.statement（只对 CONFIG SET 的几个参数脱敏），而 trace 后端的权限通常比
// 日志平台更宽、保留更久。
func TestSpanOmitsCommandArgs(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mr := miniredis.RunT(t)
	r := redis.New(redis.Config{Addrs: []string{mr.Addr()}, TracerProvider: tp})
	t.Cleanup(func() { _ = r.Stop(context.Background()) })

	const secret = "sk-live-SUPERSECRET"
	if err := r.Set(context.Background(), "session:u42", secret, 0).Err(); err != nil {
		t.Fatalf("Set got %v, want nil", err)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("应有 span 产出")
	}
	for _, span := range spans {
		for _, attr := range span.Attributes {
			if strings.Contains(attr.Value.String(), secret) {
				t.Errorf("span 属性 %s 泄漏了命令参数: %s", attr.Key, attr.Value.String())
			}
		}
	}
}

// TestMetricsNotEnabledIsVisible 钉住漏注入可见：指标静默消失不报错，
// 必须在启动日志里看得见，否则等排障时才发现没数据就太晚了。
func TestMetricsNotEnabledIsVisible(t *testing.T) {
	var buf bytes.Buffer
	mr := miniredis.RunT(t)
	r := redis.New(redis.Config{
		Addrs:  []string{mr.Addr()},
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	t.Cleanup(func() { _ = r.Stop(context.Background()) })

	if !strings.Contains(buf.String(), "指标未启用") {
		t.Errorf("应记一条「指标未启用」装配日志，got %q", buf.String())
	}
}
