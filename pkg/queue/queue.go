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
// 放弃重试。每次失败都会记一条带 typename、task_id、已重试次数与错误原文的
// 日志（asynq 本身只把错误写进 Redis 的 task 记录，进程日志里只有一串 UUID，
// 要另装 asynqmon 才看得到——那等于把失败原因藏起来）。
//
// 重试的默认跨度要知道：asynq 默认 MaxRetry=25，退避是 n^4+15+rand(30)(n+1) 秒，
// 25 次累计约 20 天，第 11 次就已经是 2.8 小时之后。多数业务要的不是这个，
// 入队时用 MaxRetry 显式给一个（与 Timeout 同样需要显式表态）。
//
// 用法：
//
//	client := queue.NewClient(queue.Config{Addr: "127.0.0.1:6379"})
//	defer client.Close()
//	client.Enqueue(ctx, "email:send", payload,
//	    queue.MaxRetry(3), queue.TaskID("email:1"), queue.Timeout(5*time.Second))
//	client.Cancel(ctx, "email:1") // 撤销尚未开始处理的任务
//
//	worker := queue.NewWorker(queue.Config{Addr: "127.0.0.1:6379", Concurrency: 10}, logger)
//	worker.Handle("email:send", func(ctx context.Context, payload []byte) error { ... })
//	// worker 实现 app.Component，交给 pkg/app 编排启停即可。
package queue

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
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
	// Username ACL 用户名（Redis 6+），无则留空走默认用户。
	Username string `yaml:"username"`
	// TLS 是否经 TLS 连接。托管 Redis 常强制加密传输（ElastiCache 的
	// in-transit encryption、Azure Cache 默认），不开就连不上。
	TLS bool `yaml:"tls"`
	// Concurrency Worker 并发处理数，仅 Worker 使用；零值取 asynq 默认
	// （当前进程可用 CPU 数）。
	Concurrency int `yaml:"concurrency"`
	// PoolSize Redis 连接池大小，零值取 go-redis 默认（10×GOMAXPROCS）。
	//
	// 与 Concurrency 配套看：并发数远高于池大小时，worker 会大面积撞上
	// ErrPoolTimeout（go-redis 的池等待上限是 ReadTimeout+1s），任务因此超时
	// 失败并进入重试。2 核容器的默认池是 20，配 concurrency: 100 就是这个下场。
	PoolSize int `yaml:"pool_size"`
	// ShutdownTimeout Worker 停机时等待在途任务的时长，零值取 asynq 默认 8s。
	//
	// 它从 app 的 stop_timeout 预算里扣，且 worker 按注册顺序通常是最先停的
	// 那个——配得太大会把传输层的排空窗口挤掉（见 Worker.Stop）。
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// redisOpt 组装 asynq 的 Redis 连接参数，Client 与 Worker 共用。
func (c Config) redisOpt() asynq.RedisClientOpt {
	opt := asynq.RedisClientOpt{
		Addr:     c.Addr,
		Username: c.Username,
		Password: c.Password,
		DB:       c.DB,
		PoolSize: c.PoolSize,
	}
	if c.TLS {
		// 只给默认值：系统根证书、版本下限。不暴露 InsecureSkipVerify——
		// 「连不上就跳过校验」是最常见的降级误用（与 pkg/redis 同一取舍）。
		opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return opt
}

// Validate 实现 conf 的校验钩子：Addr 缺失在装配期即报错。
func (c Config) Validate() error {
	if c.Addr == "" {
		return errors.New("addr 不能为空")
	}
	if c.Concurrency < 0 || c.PoolSize < 0 || c.ShutdownTimeout < 0 {
		return errors.New("concurrency、pool_size、shutdown_timeout 不能为负")
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

// TaskID 指定任务的显式 ID，撤销（Client.Cancel）按它寻址。空串视为未指定，
// 由 asynq 自动生成；同一 ID 在队列中所有未终结的任务间必须唯一，冲突时入队
// 返回错误。需要撤销能力的任务方须传入。
func TaskID(id string) Option {
	return func(dst *[]asynq.Option) { *dst = append(*dst, asynq.TaskID(id)) }
}

// Timeout 指定单个任务的处理时长上限，超时即按失败计入重试。不指定时用 asynq
// 的默认值 30 分钟——那对绝大多数任务都太长：一个卡住的处理函数会占着 worker
// 名额半小时，并发数被它一点点吃掉。有外部调用的任务方都该显式给一个。
func Timeout(d time.Duration) Option {
	return func(dst *[]asynq.Option) { *dst = append(*dst, asynq.Timeout(d)) }
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

// defaultQueue 本包的任务队列名：Enqueue 未指定队列名，任务进 asynq 默认队列，
// Cancel 按同名队列寻址——两侧共用此常量，改一处必须同步另一处。
const defaultQueue = "default"

// ErrTaskNotFound 撤销目标不存在：任务从未入队、已处理完成或已被撤销——
// 已终结的任务在 Redis 里不留痕，无法与「从未存在」区分，统一按此哨兵上报。
var ErrTaskNotFound = errors.New("任务不存在或已终结")

// Client 入队客户端，并发安全，可在多个 goroutine 间共享。
type Client struct {
	c *asynq.Client
	// insp 撤销用的检查器，与入队共用同一 Redis 连接参数，惰性建连。
	insp *asynq.Inspector
}

// NewClient 按 cfg 构造入队客户端。装配期错误（校验不过）直接 panic，对齐
// pkg/redis、pkg/mysql 的既有约定；客户端惰性建连，构造过程不触网。
func NewClient(cfg Config) *Client {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配 queue.Client: %w", err))
	}
	opt := cfg.redisOpt()
	return &Client{c: asynq.NewClient(opt), insp: asynq.NewInspector(opt)}
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

// Cancel 撤销一个尚未开始处理的任务（含延时、待重试与死信中的），按入队时
// TaskID 指定的 ID 寻址。任务不存在或已终结返回 ErrTaskNotFound；任务正在处理中
// （active）时底层不支持删除，作为普通错误上抛——撤销与消费并发时会有这条窄缝，
// 调用方重试一次通常会得到 ErrTaskNotFound 或成功。ctx 预留：底层 asynq.Inspector
// 不收取消信号，当前不产生作用。
func (c *Client) Cancel(ctx context.Context, taskID string) error {
	if taskID == "" {
		return errors.New("撤销任务: taskID 不能为空")
	}
	if err := c.insp.DeleteTask(defaultQueue, taskID); err != nil {
		if errors.Is(err, asynq.ErrTaskNotFound) {
			return fmt.Errorf("撤销任务 %s: %w", taskID, ErrTaskNotFound)
		}
		return fmt.Errorf("撤销任务 %s: %w", taskID, err)
	}
	return nil
}

// Close 关闭底层连接（入队客户端与撤销检查器一并释放）。两处都要关，失败也
// 都要报——先返回其一会把另一处的失败吞掉，那是宪法第 1 条禁止的。
func (c *Client) Close() error {
	return errors.Join(c.insp.Close(), c.c.Close())
}

// Name 实现 app.Component。
func (c *Client) Name() string { return "queue-client" }

// Start 实现 app.Component：入队客户端惰性建连，无需启动动作。
func (c *Client) Start(context.Context) error { return nil }

// Stop 实现 app.Component：关闭连接，等价于 Close。
//
// Client 实现组件接口是为了能直接交给 pkg/app 编排：任何会入队的服务都要持有
// 它，而装配动线（cmd/<svc>/initial 的 createInfra）只收 []app.Component——
// 不实现的话每个服务都得自己手搓一个包装器，或者干脆忘掉 Close，于是停机时
// Redis 连接不被优雅关闭、服务端留下半开连接。
func (c *Client) Stop(context.Context) error { return c.Close() }

// Worker 任务消费者，实现 app.Component：Start 启动内部处理协程，Stop 优雅
// 停机、等待在途任务完成。
type Worker struct {
	mux *asynq.ServeMux
	srv *asynq.Server
	// unhealthy 最近一次 Redis 连通性检查的结果，nil 即正常。
	// 用指针包一层是因为 atomic.Pointer 需要具体类型，而 error 是接口。
	unhealthy atomic.Pointer[error]
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
	w := &Worker{mux: asynq.NewServeMux()}
	w.srv = asynq.NewServer(cfg.redisOpt(), asynq.Config{
		Concurrency:     cfg.Concurrency,
		ShutdownTimeout: cfg.ShutdownTimeout,
		Logger:          slogLogger{logger: logger},
		// 处理函数的错误必须进日志。asynq 只把它写进 Redis 的 task 记录，
		// 进程日志里只有一串 UUID（retry 分支连错误都不记，archive 分支只说
		// 「Retry exhausted for task id=...」），于是下游三方挂了的时候，
		// 看日志完全不知道为什么失败——要另装 asynqmon 才看得到。
		// 包注释与宪法第 1 条都承诺不吞处理函数的错误，这里才算兑现。
		ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
			attrs := []any{"typename", task.Type(), "err", err}
			if id, ok := asynq.GetTaskID(ctx); ok {
				attrs = append(attrs, "task_id", id)
			}
			if retried, ok := asynq.GetRetryCount(ctx); ok {
				attrs = append(attrs, "retried", retried)
			}
			if maxRetry, ok := asynq.GetMaxRetry(ctx); ok {
				attrs = append(attrs, "max_retry", maxRetry)
			}
			logger.ErrorContext(ctx, "任务处理失败", attrs...)
		}),
		// Redis 连通性的周期检查，结果供 Healthy 取用（见那里的理由）。
		HealthCheckFunc: w.recordHealth,
	})
	return w
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

// recordHealth 记下 asynq 的周期健康检查结果。
func (w *Worker) recordHealth(err error) {
	if err == nil {
		w.unhealthy.Store(nil)
		return
	}
	w.unhealthy.Store(&err)
}

// Healthy 返回最近一次 Redis 连通性检查的结果，nil 即正常。
//
// 把它交给传输组件的就绪检查，队列的健康状况就参与了就绪判定：
//
//	transport.WithReadinessCheck(func(context.Context) error { return worker.Healthy() })
//
// 为什么需要它：Worker 起来之后 Redis 才不可用（实例重启、密码轮换、网络策略
// 变更）时，asynq 在内部重试并打日志，而 Start 早已返回、app 收不到任何致命
// 错误——于是实例一直报「就绪」，任务在队列里堆积，靠人发现。纯异步服务
// （none 变体）尤其危险：进程里没有任何会退出的组件，Run 永久阻塞在 ctx.Done()
// 上，除了队列长度没有别的信号。
//
// 尚未跑过检查时返回 nil：刚启动就把实例判为不就绪，等于让每次发布多等一个
// 检查周期，而此时「连不上」还只是未知、不是事实。
func (w *Worker) Healthy() error {
	if err := w.unhealthy.Load(); err != nil {
		return *err
	}
	return nil
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
// 注意本方法的等待时长由 Config.ShutdownTimeout 决定（零值即 asynq 默认 8s），
// 与传入的 ctx 无关——asynq 的 Shutdown 不收 ctx。
//
// 这段时间从 app 的 stop_timeout 预算里扣，而 worker 按「基础组件在前、业务组件
// 在后」的注册顺序通常最先停：配成 8s 而 stop_timeout 只有 10s 的话，传输层实际
// 只剩 2s 排空。pkg/app 按剩余组件数均分预算，不会让 worker 把总额吃光，但
// worker 自己这一段仍是实打实的——两个值要一起定。
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
