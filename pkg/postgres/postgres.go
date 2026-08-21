// Package postgres 构造 PostgreSQL 连接池，并把它做成 pkg/app 的资源型组件。
// 本包只出连接，不出仓储——仓储属 data 层，由它实现 biz 定义的接口。
//
// 出口是 *sql.DB（走 pgx/v5/stdlib）而非 *pgxpool.Pool：与 pkg/mysql 形状一致，
// data 层的写法可以互通，golang-migrate 与 sqlc 也都直接可用。
// 代价是拿不到 COPY FROM、LISTEN/NOTIFY 等 PG 独有能力——真需要时可经
// c.Conn(ctx) 再 conn.Raw(...) 取到底层 pgx 连接，不是死路。
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // 注册 database/sql 驱动 "pgx"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// driverName pgx/v5/stdlib 在 database/sql 里注册的驱动名。
const driverName = "pgx"

// 连接池与探活的默认值。
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 25
	defaultConnMaxLifetime = 30 * time.Minute
	defaultConnMaxIdleTime = 5 * time.Minute
	defaultConnectTimeout  = 5 * time.Second
	retryInterval          = 200 * time.Millisecond
)

// Telemetry 可观测性提供方。pkg/telemetry.Telemetry 靠结构化接口天然满足，
// 故本包不 import pkg/telemetry。
type Telemetry interface {
	TracerProvider() trace.TracerProvider
	MeterProvider() metric.MeterProvider
}

// Config PostgreSQL 连接参数。
type Config struct {
	// DSN 数据源名，如 postgres://user:pass@host:5432/db?sslmode=disable；必填。
	DSN string `yaml:"dsn"`
	// MaxOpenConns 连接池上限，零值取 25。须小于 PG 的 max_connections 减去其他客户端的占用。
	MaxOpenConns int `yaml:"max_open_conns"`
	// MaxIdleConns 空闲连接上限，零值取 25。设得比 MaxOpenConns 小会让高峰过后频繁重建连接。
	MaxIdleConns int `yaml:"max_idle_conns"`
	// ConnMaxLifetime 连接最长存活时间，零值取 30min。
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	// ConnMaxIdleTime 连接最长空闲时间，零值取 5min。
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	// ConnectTimeout 装配期探活的重试窗口，零值取 5s。见 MustNew 的说明。
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	// Telemetry 可观测性提供方，nil 时完全不埋点（零开销）。
	Telemetry Telemetry `yaml:"-"`
	// Logger 构造与停机日志，nil 时用 slog.Default()。
	Logger *slog.Logger `yaml:"-"`
}

// Client 是带生命周期的 PostgreSQL 连接池。内嵌 *sql.DB，故 QueryContext 等方法可直接调用；
// 需要原始句柄时取内嵌字段 c.DB。它的方法集满足 pkg/app.Component。
type Client struct {
	*sql.DB
	logger *slog.Logger
	unhook func()
}

// MustNew 建立连接池并在 ConnectTimeout 窗口内反复探活，连不上即 panic。
//
// 为什么要探活而不是惰性连接：sql.Open 不建立任何连接，DSN 或密码配错时服务会「启动成功」，
// K8s 认为 Pod 已就绪并开始导流，直到第一个请求进来才报错。
// 为什么要重试而不是探一次就死：服务与数据库常常同时启动，探一次会因为几秒的时间差
// 白白 CrashLoopBackOff 几轮。database/sql 的 Ping 自身不做任何重试，这层是刚需。
func MustNew(ctx context.Context, cfg Config) *Client {
	if cfg.DSN == "" {
		panic(errors.New("构造 PostgreSQL: DSN 不能为空"))
	}
	// 先解析一次，把 DSN 的语法错误挡在建连之前——否则要等探活超时才暴露。
	parsed, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		panic(fmt.Errorf("构造 PostgreSQL: 解析 DSN: %w", err))
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	db, unhook := mustOpen(cfg)
	db.SetMaxOpenConns(orDefault(cfg.MaxOpenConns, defaultMaxOpenConns))
	db.SetMaxIdleConns(orDefault(cfg.MaxIdleConns, defaultMaxIdleConns))
	db.SetConnMaxLifetime(orDefaultDuration(cfg.ConnMaxLifetime, defaultConnMaxLifetime))
	db.SetConnMaxIdleTime(orDefaultDuration(cfg.ConnMaxIdleTime, defaultConnMaxIdleTime))

	if err := ping(ctx, orDefaultDuration(cfg.ConnectTimeout, defaultConnectTimeout), db.PingContext); err != nil {
		unhook()
		_ = db.Close()
		panic(fmt.Errorf("构造 PostgreSQL: 探活 %s:%d: %w", parsed.Host, parsed.Port, err))
	}
	logger.Info("PostgreSQL 已就绪", "component", "postgres",
		"host", parsed.Host, "port", parsed.Port, "db", parsed.Database)

	return &Client{DB: db, logger: logger, unhook: unhook}
}

// mustOpen 打开连接池；配了 Telemetry 就走 otelsql，并注册连接池指标。
// 返回的 unhook 用于在停机时注销指标回调，否则 MeterProvider 关闭后仍会被回调。
func mustOpen(cfg Config) (*sql.DB, func()) {
	if cfg.Telemetry == nil {
		db, err := sql.Open(driverName, cfg.DSN)
		if err != nil {
			panic(fmt.Errorf("构造 PostgreSQL: 打开连接池: %w", err))
		}
		return db, func() {}
	}

	attrs := otelsql.WithAttributes(semconv.DBSystemNameKey.String("postgresql"))
	db, err := otelsql.Open(driverName, cfg.DSN, attrs,
		otelsql.WithTracerProvider(cfg.Telemetry.TracerProvider()),
		otelsql.WithMeterProvider(cfg.Telemetry.MeterProvider()))
	if err != nil {
		panic(fmt.Errorf("构造 PostgreSQL: 打开连接池: %w", err))
	}

	registration, err := otelsql.RegisterDBStatsMetrics(db, attrs,
		otelsql.WithMeterProvider(cfg.Telemetry.MeterProvider()))
	if err != nil {
		_ = db.Close()
		panic(fmt.Errorf("构造 PostgreSQL: 注册连接池指标: %w", err))
	}
	return db, func() { _ = registration.Unregister() }
}

// ping 在 timeout 窗口内反复探活，直到成功或窗口耗尽。
func ping(ctx context.Context, timeout time.Duration, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		err := probe(ctx)
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

func orDefault(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func orDefaultDuration(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}

// Name 实现 pkg/app.Component。
func (c *Client) Name() string { return "postgres" }

// Start 实现 pkg/app.Component。资源型组件没有常驻循环，直接返回。
func (c *Client) Start(context.Context) error { return nil }

// Stop 先注销指标回调再关闭连接池——反过来的话，连接池已关而回调仍在，
// 下一次采集会读到一个已关闭的 DB。
func (c *Client) Stop(context.Context) error {
	c.unhook()
	return c.DB.Close()
}
