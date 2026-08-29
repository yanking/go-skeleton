// Package queue 是 asynq 任务队列的薄封装：Client 负责入队，Worker 负责消费
// （实现 app.Component）。业务方只感知 typename、payload 与 Option，asynq
// 与底层 Redis 协议细节不出本包。
//
// 部署要求：本包使用的 Redis 须是专用实例，不与缓存等可被淘汰的数据混用——
// asynq 把任务数据当持久状态读写，一旦被 LRU/LFU 淘汰即丢任务，故该实例须
// 配 maxmemory-policy noeviction；同时建议开启 AOF 持久化，否则 Redis 重启
// 会丢失尚未消费的任务（内存态数据，无落盘保证）。
//
// 错误语义：Worker.Handle 注册的处理函数返回 nil 视为任务完成；返回非 nil
// error 触发 asynq 按内置退避策略重试，重试耗尽后任务进入 archived 队列——
// 与宪法第 1 条「错误必须上抛」对齐，本包不吞并处理函数的错误、也不代为决定
// 放弃重试。
//
// 用法：
//
//	client := queue.NewClient(queue.Config{Addr: "127.0.0.1:6379"})
//	defer client.Close()
//	client.Enqueue(ctx, "email:send", payload, queue.MaxRetry(3))
//
//	worker := queue.NewWorker(queue.Config{Addr: "127.0.0.1:6379", Concurrency: 10}, logger)
//	worker.Handle("email:send", func(ctx context.Context, payload []byte) error { ... })
//	// worker 实现 app.Component，交给 pkg/app 编排启停即可。
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
)

// Config 队列装配参数，声明式字段由配置文件绑定，Client 与 Worker 共用同一份。
type Config struct {
	// Addr Redis 地址，host:port 形式，必填。
	Addr string `yaml:"addr"`
	// Password Redis 密码，无鉴权则留空。
	Password string `yaml:"password"`
	// DB 选库序号。
	DB int `yaml:"db"`
	// Concurrency Worker 并发处理数，仅 Worker 使用；零值取 asynq 默认
	// （当前进程可用 CPU 数）。
	Concurrency int `yaml:"concurrency"`
}

// Validate 实现 conf 的校验钩子：Addr 缺失在装配期即报错。
func (c Config) Validate() error {
	if c.Addr == "" {
		return errors.New("addr 不能为空")
	}
	return nil
}

// Option 入队可选参数，语义与命名对齐 asynq 同名选项，调用方无需引入 asynq
// 包即可传参——薄封装的目的是让业务方只依赖本包。
type Option func(*[]asynq.Option)

// MaxRetry 指定任务失败后的最大重试次数，负数按 0 处理（对齐 asynq 行为）。
func MaxRetry(n int) Option {
	return func(dst *[]asynq.Option) { *dst = append(*dst, asynq.MaxRetry(n)) }
}

// ProcessIn 指定任务相对当前时间的延迟处理时长，用于错峰重试或延时任务。
func ProcessIn(d time.Duration) Option {
	return func(dst *[]asynq.Option) { *dst = append(*dst, asynq.ProcessIn(d)) }
}

// options 把本包 Option 列表转换为底层 asynq.Option 列表。未导出，靠同包
// 测试直接验证映射关系。
func options(opts []Option) []asynq.Option {
	var out []asynq.Option
	for _, opt := range opts {
		opt(&out)
	}
	return out
}

// Client 入队客户端，并发安全，可在多个 goroutine 间共享。
type Client struct {
	c *asynq.Client
}

// NewClient 按 cfg 构造入队客户端。装配期错误（校验不过）直接 panic，对齐
// pkg/redis、pkg/mysql 的既有约定；客户端惰性建连，构造过程不触网。
func NewClient(cfg Config) *Client {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配 queue.Client: %w", err))
	}
	return &Client{c: asynq.NewClient(asynq.RedisClientOpt{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})}
}

// Enqueue 把一个任务放入队列：typename 标识任务类型，payload 为原始报文，由
// Worker 侧同名 Handle 注册的处理函数解析；opts 控制重试次数、延迟处理等
// 行为，不给即用 asynq 默认策略。
func (c *Client) Enqueue(ctx context.Context, typename string, payload []byte, opts ...Option) error {
	if _, err := c.c.EnqueueContext(ctx, asynq.NewTask(typename, payload), options(opts)...); err != nil {
		return fmt.Errorf("入队任务 %s: %w", typename, err)
	}
	return nil
}

// Close 关闭底层连接。
func (c *Client) Close() error {
	return c.c.Close()
}

// Worker 任务消费者，实现 app.Component：Start 启动内部处理协程，Stop 优雅
// 停机、等待在途任务完成。
type Worker struct {
	mux *asynq.ServeMux
	srv *asynq.Server
}

// NewWorker 按 cfg 构造消费者，logger 为 nil 时用 slog.Default()。装配期错误
// （校验不过）直接 panic；服务端惰性建连，构造过程不触网。
func NewWorker(cfg Config, logger *slog.Logger) *Worker {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配 queue.Worker: %w", err))
	}
	if logger == nil {
		logger = slog.Default()
	}
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB},
		asynq.Config{
			Concurrency: cfg.Concurrency,
			Logger:      slogLogger{logger: logger},
		},
	)
	return &Worker{mux: asynq.NewServeMux(), srv: srv}
}

// Handle 注册 typename 对应的处理函数：h 返回 nil 视为完成，返回 error 即按
// asynq 重试策略重试（见包注释「错误语义」）。对同一 typename 重复注册会
// panic——这是 asynq.ServeMux 的既有行为，本包不吞掉它，重复注册通常意味着
// 装配期写错了服务接线。
func (w *Worker) Handle(typename string, h func(ctx context.Context, payload []byte) error) {
	w.mux.HandleFunc(typename, func(ctx context.Context, task *asynq.Task) error {
		return h(ctx, task.Payload())
	})
}

// Name 实现 app.Component。
func (w *Worker) Name() string { return "queue-worker" }

// Start 实现 app.Component：启动内部处理协程后立即返回——asynq.Server 的
// 消费循环运行在其自管的后台协程中，不占用本方法的调用栈，故不必像
// http.Server.Serve 那样阻塞（无常驻循环的资源型组件同类写法见 pkg/redis、
// pkg/mysql）。
//
// 这里刻意不用 asynq.Server.Run：它会自装一套独立的 OS 信号监听
// （SIGTERM/SIGINT/SIGTSTP），与 pkg/app 由 cmd 统一持有的根 ctx 信号入口
// 重复——两者各自反应，停机时序不可控。改用 Start，阻塞与否、何时停机
// 完全交给 pkg/app 驱动。
func (w *Worker) Start(context.Context) error {
	if err := w.srv.Start(w.mux); err != nil {
		return fmt.Errorf("启动 queue worker: %w", err)
	}
	return nil
}

// Stop 实现 app.Component：优雅停机，等待在途任务完成，超时未完成的任务会被放回
// 队列等待重试。Shutdown 对尚未 Start 的 Server 是空操作，容忍「组件尚未真正运行时
// 被 Stop」的场景。
//
// 注意本方法的等待时长由 asynq.Config.ShutdownTimeout 决定（未配置即 asynq 默认
// 8s），与传入的 ctx 无关——asynq 的 Shutdown 不收 ctx。故服务把 app 的
// stop_timeout 配成小于 8s 时，pkg/app 会先放弃等待、进程随后退出，在途任务被硬
// 中断（asynq 侧会把它们放回队列重试，不丢任务，但会多一次重复投递）。要压低整体
// 停机预算就把 stop_timeout 留在 8s 以上。
func (w *Worker) Stop(context.Context) error {
	w.srv.Shutdown()
	return nil
}

// slogLogger 把 asynq 的 Print 风格日志接口（Debug/Info/Warn/Error/Fatal 接收
// 变长参数、内部按 fmt.Sprint 拼接）桥接到 slog，写法对齐 pkg/mysql、
// pkg/redis 桥接 GORM/go-redis 日志的既有约定。这里的 fmt.Sprint 是第三方
// 接口所迫的例外（go-style 硬规则 5），不做「修正」。
type slogLogger struct {
	logger *slog.Logger
}

func (l slogLogger) Debug(args ...any) { l.logger.Debug(fmt.Sprint(args...)) }
func (l slogLogger) Info(args ...any)  { l.logger.Info(fmt.Sprint(args...)) }
func (l slogLogger) Warn(args ...any)  { l.logger.Warn(fmt.Sprint(args...)) }
func (l slogLogger) Error(args ...any) { l.logger.Error(fmt.Sprint(args...)) }

// Fatal 桥接 asynq 的 Fatal 级别日志，但不终止进程——业务路径禁止 panic 或
// 主动退出（宪法第 1 条），异常一律通过返回值上抛，由调用方决定如何处理。
func (l slogLogger) Fatal(args ...any) { l.logger.Error(fmt.Sprint(args...)) }
