// Package mysql 构造 MySQL 连接池，并把它做成 pkg/app 的资源型组件。
// 本包只出连接，不出仓储——仓储属 data 层，由它实现 biz 定义的接口。
//
// 与 pkg/postgres 形状刻意保持一致：两者各自 blank import 自己的驱动，
// 合成一个包会让只用 MySQL 的服务白白多链约 6MB 的 pgx 驱动（实测 7.2MB → 13.5MB）。
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/XSAM/otelsql"
	driver "github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// driverName database/sql 里的驱动名，由 go-sql-driver/mysql 注册。
const driverName = "mysql"

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

// Config MySQL 连接参数。
type Config struct {
	// DSN 数据源名，如 user:pass@tcp(host:3306)/db?parseTime=true；必填。
	DSN string `yaml:"dsn"`
	// MaxOpenConns 连接池上限，零值取 25。
	MaxOpenConns int `yaml:"max_open_conns"`
	// MaxIdleConns 空闲连接上限，零值取 25。设得比 MaxOpenConns 小会让高峰过后频繁重建连接。
	MaxIdleConns int `yaml:"max_idle_conns"`
	// ConnMaxLifetime 连接最长存活时间，零值取 30min。须小于 MySQL 的 wait_timeout，
	// 否则会拿到已被服务端单方面关闭的连接。
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

// Client 是带生命周期的 MySQL 连接池。内嵌 *sql.DB，故 QueryContext 等方法可直接调用；
// 需要原始句柄时取内嵌字段 c.DB。它的方法集满足 pkg/app.Component。
type Client struct {
	*sql.DB
	logger  *slog.Logger
	unhook  func()
	dsnHost string
}

// MustNew 建立连接池并在 ConnectTimeout 窗口内反复探活，连不上即 panic。
//
// 为什么要探活而不是惰性连接：sql.Open 不建立任何连接，DSN 或密码配错时服务会「启动成功」，
// K8s 认为 Pod 已就绪并开始导流，直到第一个请求进来才报错。
// 为什么要重试而不是探一次就死：服务与数据库常常同时启动，探一次会因为几秒的时间差
// 白白 CrashLoopBackOff 几轮。database/sql 的 Ping 自身不做任何重试，这层是刚需。
func MustNew(ctx context.Context, cfg Config) *Client {
	if cfg.DSN == "" {
		panic(errors.New("构造 MySQL: DSN 不能为空"))
	}
	// 先解析一次，把 DSN 的语法错误挡在建连之前——否则要等探活超时才暴露。
	parsed, err := driver.ParseDSN(cfg.DSN)
	if err != nil {
		panic(fmt.Errorf("构造 MySQL: 解析 DSN: %w", err))
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
		panic(fmt.Errorf("构造 MySQL: 探活 %s: %w", parsed.Addr, err))
	}
	logger.Info("MySQL 已就绪", "component", "mysql", "addr", parsed.Addr, "db", parsed.DBName)

	return &Client{DB: db, logger: logger, unhook: unhook, dsnHost: parsed.Addr}
}

// mustOpen 打开连接池；配了 Telemetry 就走 otelsql，并注册连接池指标。
// 返回的 unhook 用于在停机时注销指标回调，否则 MeterProvider 关闭后仍会被回调。
func mustOpen(cfg Config) (*sql.DB, func()) {
	if cfg.Telemetry == nil {
		db, err := sql.Open(driverName, cfg.DSN)
		if err != nil {
			panic(fmt.Errorf("构造 MySQL: 打开连接池: %w", err))
		}
		return db, func() {}
	}

	attrs := otelsql.WithAttributes(semconv.DBSystemNameKey.String("mysql"))
	db, err := otelsql.Open(driverName, cfg.DSN, attrs,
		otelsql.WithTracerProvider(cfg.Telemetry.TracerProvider()),
		otelsql.WithMeterProvider(cfg.Telemetry.MeterProvider()))
	if err != nil {
		panic(fmt.Errorf("构造 MySQL: 打开连接池: %w", err))
	}

	registration, err := otelsql.RegisterDBStatsMetrics(db, attrs,
		otelsql.WithMeterProvider(cfg.Telemetry.MeterProvider()))
	if err != nil {
		_ = db.Close()
		panic(fmt.Errorf("构造 MySQL: 注册连接池指标: %w", err))
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
func (c *Client) Name() string { return "mysql" }

// Start 实现 pkg/app.Component。资源型组件没有常驻循环，直接返回。
func (c *Client) Start(context.Context) error { return nil }

// Stop 先注销指标回调再关闭连接池——反过来的话，连接池已关而回调仍在，
// 下一次采集会读到一个已关闭的 DB。
func (c *Client) Stop(context.Context) error {
	c.unhook()
	return c.DB.Close()
}
