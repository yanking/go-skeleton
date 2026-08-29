// Package redis 构造 go-redis 客户端，单机与集群从配置到调用完全无区别：
// addrs 给一个地址即单机，给多个即集群（redis.UniversalClient 的官方推断
// 语义，客户端自动发现全部分片）；DB 内嵌 UniversalClient，命令方法
// 直接可用，业务代码与部署形态彻底解耦，换部署只改地址列表。
//
// 经 Logger 注入 slog 后自动上报命令日志：执行失败进 Error（redis.Nil 是
// 查空键的正常返回，除外）、超过慢命令阈值进 Warn、其余进 Debug。只记
// 命令名不记参数——参数可能含敏感值。
//
// DB 同时实现 app.Component：客户端惰性建连，Start 直接返回，Stop 关闭
// 客户端。装配期不 ping，理由同 SQL 连接池：DB 晚于进程就绪是常态。
//
// 用法：
//
//	rdb := redis.New(redis.Config{Addrs: []string{"127.0.0.1:6379"}})
//	rdb.Get(ctx, "key") // 与 go-redis 手感完全一致
package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/extra/redisotel/v9"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/trace"
)

// Config 客户端装配参数，声明式字段由配置文件绑定，注入项标 yaml:"-"。
type Config struct {
	// Addrs 节点地址列表：恰好一个 → 单机；两个及以上 → 集群（须是种子节点，
	// 客户端自动发现其余分片）。注意分片集群别只给一个地址——会被推断成单机。
	Addrs []string `yaml:"addrs"`
	// Password 密码，无则留空。
	Password string `yaml:"password"`
	// DB 单机模式选库序号；集群（多地址）不支持选库，必须为 0。
	DB int `yaml:"db"`
	// PoolSize 每节点连接池大小，零值取 go-redis 默认。
	PoolSize int `yaml:"pool_size"`
	// TracerProvider 链路追踪注入项，nil 则不埋 DB span。
	TracerProvider trace.TracerProvider `yaml:"-"`
	// Logger 命令日志（执行失败、慢命令）输出，nil 时用 slog.Default()。
	Logger *slog.Logger `yaml:"-"`
}

// Validate 实现 conf 的校验钩子：地址缺失、集群选库在绑定阶段即报错。
func (c Config) Validate() error {
	if len(c.Addrs) == 0 {
		return errors.New("addrs 不能为空")
	}
	if len(c.Addrs) > 1 && c.DB != 0 {
		return errors.New("多地址即集群模式,不支持选库, db 须为 0")
	}
	if c.PoolSize < 0 {
		return errors.New("pool_size 不能为负")
	}
	return nil
}

// DB 内嵌 redis.UniversalClient（命令方法直接可用），同时实现 app.Component。
type DB struct {
	goredis.UniversalClient
}

// New 按 cfg 构造客户端。装配期错误（校验不过、追踪注入失败）直接 panic。
func New(cfg Config) *DB {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配 DB: %w", err))
	}

	client := goredis.NewUniversalClient(&goredis.UniversalOptions{
		Addrs:    cfg.Addrs,
		Password: cfg.Password,
		DB:       cfg.DB,
		PoolSize: cfg.PoolSize,
	})

	if cfg.TracerProvider != nil {
		if err := redisotel.InstrumentTracing(client,
			redisotel.WithTracerProvider(cfg.TracerProvider)); err != nil {
			panic(fmt.Errorf("装配 DB: 注入链路追踪: %w", err))
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	client.AddHook(cmdLogger{logger: logger})

	return &DB{UniversalClient: client}
}

// Name 实现 app.Component。
func (r *DB) Name() string { return "redis" }

// Start 实现 app.Component：客户端惰性建连，无需启动动作。
func (r *DB) Start(context.Context) error { return nil }

// Stop 实现 app.Component：关闭客户端与连接池。
func (r *DB) Stop(context.Context) error { return r.Close() }
