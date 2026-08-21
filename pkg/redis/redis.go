// Package redis 构造 Redis 客户端连接池，并把它做成 pkg/app 的资源型组件。
// 本包只出连接，不出仓储——仓储属 data 层，由它实现 biz 定义的接口。
package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// defaultConnectTimeout 装配期探活的重试窗口。
const defaultConnectTimeout = 5 * time.Second

// Telemetry 可观测性提供方。pkg/telemetry.Telemetry 靠结构化接口天然满足，
// 故本包不 import pkg/telemetry。
type Telemetry interface {
	TracerProvider() trace.TracerProvider
	MeterProvider() metric.MeterProvider
}

// Config Redis 连接参数。
type Config struct {
	// Addr 服务地址 host:port；必填。
	Addr string
	// Password 访问密码，无密码留空。
	Password string
	// DB 库号，零值即 0 号库。
	DB int
	// PoolSize 连接池大小，零值取 go-redis 默认（10 × GOMAXPROCS）。
	PoolSize int
	// ConnectTimeout 装配期探活的重试窗口，零值取 5s。见 MustNew 的说明。
	ConnectTimeout time.Duration
	// Telemetry 可观测性提供方，nil 时完全不埋点（零开销）。
	Telemetry Telemetry
	// Logger 构造与停机日志，nil 时用 slog.Default()。
	Logger *slog.Logger
}

// Client 是带生命周期的 Redis 客户端。内嵌 *goredis.Client，故 Get/Set 等命令可直接调用；
// 需要原始客户端时取内嵌字段 c.Client。它的方法集满足 pkg/app.Component。
type Client struct {
	*goredis.Client
	logger *slog.Logger
}

// MustNew 建立 Redis 连接池并在 ConnectTimeout 窗口内反复探活，连不上即 panic。
//
// 为什么要探活而不是惰性连接：地址或密码配错时，惰性连接会让服务「启动成功」，
// K8s 认为 Pod 已就绪并开始导流，直到第一个请求进来才报错。
// 为什么要重试而不是探一次就死：服务与 Redis 常常同时启动（docker-compose、K8s 同时拉起），
// 探一次会因为几秒的启动时间差而白白 CrashLoopBackOff 几轮。
func MustNew(ctx context.Context, cfg Config) *Client {
	if cfg.Addr == "" {
		panic(errors.New("构造 Redis: Addr 不能为空"))
	}
	if cfg.DB < 0 {
		panic(fmt.Errorf("构造 Redis: 库号 %d 不能为负", cfg.DB))
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultConnectTimeout
	}

	rdb := goredis.NewClient(&goredis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		PoolSize: cfg.PoolSize,
	})

	// 埋点要在探活之前挂上，这样连不上时的重试过程本身也在链路里可见。
	// provider 显式注入，不依赖全局。
	if cfg.Telemetry != nil {
		if err := redisotel.InstrumentTracing(rdb,
			redisotel.WithTracerProvider(cfg.Telemetry.TracerProvider())); err != nil {
			panic(fmt.Errorf("构造 Redis: 挂载 trace 埋点: %w", err))
		}
		if err := redisotel.InstrumentMetrics(rdb,
			redisotel.WithMeterProvider(cfg.Telemetry.MeterProvider())); err != nil {
			panic(fmt.Errorf("构造 Redis: 挂载 metric 埋点: %w", err))
		}
	}

	if err := ping(ctx, timeout, rdb.Ping); err != nil {
		_ = rdb.Close()
		panic(fmt.Errorf("构造 Redis: 探活 %s: %w", cfg.Addr, err))
	}
	logger.Info("Redis 已就绪", "component", "redis", "addr", cfg.Addr, "db", cfg.DB)

	return &Client{Client: rdb, logger: logger}
}

// ping 在 timeout 窗口内反复探活，直到成功或窗口耗尽。
func ping(ctx context.Context, timeout time.Duration, probe func(context.Context) *goredis.StatusCmd) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		err := probe(ctx).Err()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w（最后一次错误: %w）", ctx.Err(), err)
		case <-time.After(retryInterval):
		}
	}
}

// retryInterval 两次探活之间的间隔。
const retryInterval = 200 * time.Millisecond

// Name 实现 pkg/app.Component。
func (c *Client) Name() string { return "redis" }

// Start 实现 pkg/app.Component。资源型组件没有常驻循环，直接返回。
func (c *Client) Start(context.Context) error { return nil }

// Stop 关闭连接池。
func (c *Client) Stop(context.Context) error { return c.Client.Close() }
