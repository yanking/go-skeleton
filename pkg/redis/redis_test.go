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
		{name: "多地址即集群", cfg: redis.Config{Addrs: []string{"a:6379", "b:6379"}}, wantErr: false},
		{name: "地址缺失", cfg: redis.Config{}, wantErr: true},
		{name: "集群不许选库", cfg: redis.Config{Addrs: []string{"a:6379", "b:6379"}, DB: 2}, wantErr: true},
		{name: "pool_size 为负", cfg: redis.Config{Addrs: []string{"a:6379"}, PoolSize: -1}, wantErr: true},
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
	r := redis.New(redis.Config{Addrs: []string{"127.0.0.1:6379", "127.0.0.1:6380"}})

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
