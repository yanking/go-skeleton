package redis_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/redis"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func TestMustNewPanicsOnInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  redis.Config
	}{
		{name: "Addr 为空", cfg: redis.Config{}},
		{name: "库号为负", cfg: redis.Config{Addr: "127.0.0.1:6379", DB: -1}},
		{
			// 127.0.0.1:1 上不会有人监听，重试窗口耗尽后必须 panic，
			// 而不是「启动成功、第一个请求才 500」。
			name: "连不上时重试到超时",
			cfg:  redis.Config{Addr: "127.0.0.1:1", ConnectTimeout: 200 * time.Millisecond},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("want panic, got 正常返回")
				}
			}()
			tt.cfg.Logger = discardLogger()
			redis.MustNew(context.Background(), tt.cfg)
		})
	}
}

// Client 必须满足 pkg/app.Component，否则 cmd 无法把它注册进 app。
var _ app.Component = (*redis.Client)(nil)

func TestClientRoundTripAndStop(t *testing.T) {
	// miniredis 是进程内的真 Redis 实现：走真实协议，不需要外部服务。
	srv := miniredis.RunT(t)
	ctx := context.Background()
	c := redis.MustNew(ctx, redis.Config{Addr: srv.Addr(), Logger: discardLogger()})

	if got, want := c.Name(), "redis"; got != want {
		t.Errorf("Name got %q, want %q", got, want)
	}
	if err := c.Start(ctx); err != nil {
		t.Errorf("Start 返回错误: %v", err)
	}

	// 命令直接调用，无需先取内嵌字段——这是内嵌 *goredis.Client 的目的。
	if err := c.Set(ctx, "语言", "Go", 0).Err(); err != nil {
		t.Fatalf("SET 失败: %v", err)
	}
	got, err := c.Get(ctx, "语言").Result()
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	if want := "Go"; got != want {
		t.Errorf("GET got %q, want %q", got, want)
	}

	if err := c.Stop(ctx); err != nil {
		t.Errorf("Stop 返回错误: %v", err)
	}
	if err := c.Ping(ctx).Err(); err == nil {
		t.Error("Stop 之后连接池应已关闭，命令不该还能成功")
	}
}

func TestConnectRetriesUntilServerAppears(t *testing.T) {
	// 重试要解决的真实场景：服务与 Redis 同时启动，Redis 慢几百毫秒才就绪。
	// 先抢一个空闲端口再放掉，得到一个「暂时没人监听」的地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("放端口: %v", err)
	}

	// 延迟取 2.5s 是有讲究的：go-redis 单次 Ping 自身会重试，实测能扛约 1.7s，
	// 低于这个阈值的延迟根本测不出本包的重试循环（曾用 400ms，去掉循环用例照样通过）。
	// database/sql 的 Ping 则完全不重试，pkg/mysql、pkg/postgres 里这层是刚需。
	srv := miniredis.NewMiniRedis()
	t.Cleanup(srv.Close)
	go func() {
		time.Sleep(2500 * time.Millisecond)
		_ = srv.StartAddr(addr)
	}()

	ctx := context.Background()
	c := redis.MustNew(ctx, redis.Config{
		Addr:           addr,
		ConnectTimeout: 10 * time.Second,
		Logger:         discardLogger(),
	})
	t.Cleanup(func() { _ = c.Stop(ctx) })

	if err := c.Ping(ctx).Err(); err != nil {
		t.Errorf("重试连上后应可用, got %v", err)
	}
}

func TestTelemetryProducesCommandSpan(t *testing.T) {
	var spans bytes.Buffer
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterStdout,
		Writer:   &spans,
		Logger:   discardLogger(),
	})

	srv := miniredis.RunT(t)
	// pkg/telemetry.Telemetry 靠结构化接口满足 redis.Telemetry，本包不 import pkg/telemetry。
	c := redis.MustNew(ctx, redis.Config{Addr: srv.Addr(), Telemetry: tel, Logger: discardLogger()})

	if err := c.Set(ctx, "语言", "Go", 0).Err(); err != nil {
		t.Fatalf("SET 失败: %v", err)
	}

	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := tel.Stop(ctx); err != nil {
		t.Fatalf("关闭 telemetry: %v", err)
	}

	if got := spans.String(); !strings.Contains(got, "set") {
		t.Errorf("未见 Redis 命令 span\n实际输出:\n%s", got)
	}
}
