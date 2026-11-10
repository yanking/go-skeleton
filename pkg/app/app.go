// Package app 编排服务内所有可启停组件的生命周期：按注册顺序拉起、逆序停止。
// 根 ctx 由调用方（cmd）给出，本包不监听信号、也不自造根 ctx。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// defaultStopTimeout 未配置时的停机总超时。比 K8s terminationGracePeriodSeconds
// 的默认值（30s）留 5s 余量：取等只是刚好用满，收尾日志、遥测 flush 与进程退出
// 都还要时间，用满的那一刻 SIGKILL 同时到达，这些就全丢了。
//
// 25s 这个数是算出来的，不是挑的：它要装得下传输组件的摘流（drain_delay 默认
// 5s）加在途请求排空（handler_timeout 默认 15s）再加余量。改其中任何一个都要
// 回头看另外两个。
//
// 传输组件的摘流等待（pkg/transport 的 drain_delay，默认 5s）发生在它自己的 Stop
// 里，所以是**包含在本预算之内**的，不是额外的一段：本值要装得下
// drain_delay + handler_timeout + 余量（默认 5 + 15 + 5 = 25）。
const defaultStopTimeout = 25 * time.Second

// 两个哨兵让调用方（cmd）能区分停机的两种结局：一次干净但超时的停机和一次真
// 崩溃，不该在退出码与告警上长得一模一样。
//
// Run 返回的是 errors.Join(runErr, stopErr)，两者可能同时存在（组件崩了、另一个
// 组件又停得慢）。判定顺序因此要紧：先看 ErrRunFailure，否则「崩 + 慢」会被归成
// 「慢」而不告警。
var (
	// ErrStopTimeout 停机未在宽限期内完成。
	ErrStopTimeout = errors.New("停机未在宽限期内完成")
	// ErrRunFailure 组件在运行期意外退出。
	ErrRunFailure = errors.New("组件意外退出")
)

// Component 可被 App 编排的生命周期组件。
//
// 实现方把「常驻循环」原样交给 Start、把「让循环停下来」原样交给 Stop 即可，
// 无需自起 goroutine、无需自建错误上报 channel、也无需过滤停机期的正常退出错误
// （http.ErrServerClosed、grpc.ErrServerStopped 等）——这些一律由 App 处理：
//
//	func (s *HTTPServer) Start(ctx context.Context) error { return s.srv.Serve(s.ln) }
//	func (s *HTTPServer) Stop(ctx context.Context) error  { return s.srv.Shutdown(ctx) }
type Component interface {
	// Name 组件名，用于日志与错误标注。
	Name() string

	// Start 运行组件，通常阻塞到组件被 Stop 停下或自身出错为止。
	// 无常驻循环的资源型组件（数据库连接池、gateway 环回 ClientConn）直接返回 nil。
	// 监听端口一类「起不来就该死」的准备工作放在装配期（cmd 构造组件时）完成，不放进 Start：
	// App 按注册顺序拉起，但不等待也无从判断组件是否已就绪。
	// 停机期的返回值一律按预期处理，只有非停机期的非 nil 返回才被视为致命错误。
	Start(ctx context.Context) error

	// Stop 停止组件，须在 ctx 到期前返回；超时后 App 不再等它，直接停下一个组件。
	// Stop 可能在组件尚未真正运行时被调用（例如别的组件先失败触发全量停机），实现须容忍。
	Stop(ctx context.Context) error
}

// Config App 的运行参数，零值即默认。声明式参数带 yaml 键，服务配置可直接
// 嵌入绑定；注入项标 yaml:"-"，只能由装配期在 Go 里给——约定与 pkg/log 一致。
type Config struct {
	// StopTimeout 停机总超时，从开始逆序停止起算，零值取 defaultStopTimeout。
	//
	// 它是全部组件的总额。分配不是均分、也不是先到先吃：每个组件拿到「剩余预算
	// 减去后面组件的保底」，于是最先停的传输组件拿到几乎全部，而排在后面的 DB、
	// Redis 各有保底不会拿到已过期的 ctx（理由见 budgetFor）。
	//
	// 它要装得下传输组件的摘流加排空，即 transport 的 drain_delay + handler_timeout
	// 再加余量——默认值（5s + 15s + 5s = 25s）正是这么来的，改其中任何一个都要
	// 回头看另外两个。
	//
	// 一处不精确要说明：每个组件至少拿 minStopBudget，所以总额在极端情况下会被
	// 撑破——最坏超出是「组件数 × minStopBudget」（当前是每个 100ms，五个组件即
	// 半秒）。部署侧的宽限期本来就该留余量，这点超出在余量之内。
	//
	// 该值须 ≤ 部署侧给进程的宽限期减去传输组件的摘流等待，否则超出的部分只是
	// 纸面预算——SIGKILL 不会因为配置写得大就晚来。
	StopTimeout time.Duration `yaml:"stop_timeout"`
	// Logger 生命周期日志输出，nil 时用 slog.Default()。
	Logger *slog.Logger `yaml:"-"`
}

// App 按注册顺序编排一组组件的启停。
type App struct {
	components  []Component
	stopTimeout time.Duration
	logger      *slog.Logger
	// stopping 标记 App 已决定停机：此后组件 Start 的返回值一律按预期处理，不再视为致命错误。
	stopping atomic.Bool
}

// New 构造 App，components 的顺序即拉起顺序，停止时按其逆序执行。
func New(cfg Config, components ...Component) *App {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	stopTimeout := cfg.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = defaultStopTimeout
	}
	return &App{components: components, stopTimeout: stopTimeout, logger: logger}
}

// Run 拉起全部组件并阻塞，直到 ctx 被取消或某个组件意外退出，随后逆序停止全部组件。
// ctx 取消属正常退出，不计为错误；返回值聚合组件的意外退出错误与停止过程中的失败，
// 调用方（cmd）应据此以非零码退出。
func (a *App) Run(ctx context.Context) error {
	// fatal 与 finished 由 Run 独占创建：容量均等于组件数、每个运行 goroutine 至多各写一次，
	// 故写入永不阻塞，即便 Run 已不再接收也不会因此泄漏；只读不关。
	fatal := make(chan error, len(a.components))
	finished := make(chan struct{}, len(a.components))

	for _, c := range a.components {
		go func() {
			a.runComponent(ctx, c, fatal)
			finished <- struct{}{}
		}()
		a.logger.Info("组件已拉起", "component", c.Name())
	}

	var runErr error
	select {
	case <-ctx.Done():
		a.logger.Info("开始停机", "reason", "signal")
	case runErr = <-fatal:
		a.logger.Error("组件意外退出，开始停机", "err", runErr)
	}
	a.stopping.Store(true)

	// 触发停机的 ctx 通常已被取消，直接沿用等于宽限期为零，故剥离其取消信号后另挂 StopTimeout。
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.stopTimeout)
	defer cancel()

	stopErr := a.stop(stopCtx)
	a.await(stopCtx, finished)

	return errors.Join(runErr, stopErr)
}

// minStopBudget 单个组件至少拿到的停机时间。
//
// 多数组件的 Stop 是「关个池、瞬间返回」，一点时间都不给就等于白白跳过它们的
// 收尾；但它也不能大，否则组件多的时候会把总预算撑破。
const minStopBudget = 100 * time.Millisecond

// budgetFor 给下一个要停的组件分配时间：剩余预算减去「后面每个组件各留
// minStopBudget」之后的全部，下限是 minStopBudget 本身。
//
// 为什么是保留制而不是均分：各组件的时间需求根本不均。传输组件要先摘流、再等
// 在途请求做完（量级是秒），而 DB、Redis 的 Stop 是关个池（毫秒级）。均分会让
// 最先停的传输组件只拿到 T/n——六个组件、25s 预算下就是 4.2s，连摘流加排空都
// 装不下，在途请求被 grpc.Stop() 当场切断，而每次停机都以「宽限期耗尽」收场。
//
// 保留制同时守住了均分本来要解决的那个问题：原先全部组件共享一个总预算、先到
// 先吃，传输层被慢 handler 挂住就把预算吃光，排在后面的 DB、Redis 拿到已过期的
// ctx 立刻返回「超时」——于是关库与仍在执行的 handler 重叠，handler 的下一条
// SQL 撞上 sql: database is closed，事务做了一半。现在它们各自有保底。
func (a *App) budgetFor(ctx context.Context, remaining int) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok || remaining <= 0 {
		return a.stopTimeout
	}
	// 给排在后面的 remaining-1 个组件各留一份保底。
	reserved := time.Duration(remaining-1) * minStopBudget
	if budget := time.Until(deadline) - reserved; budget > minStopBudget {
		return budget
	}
	return minStopBudget
}

// runComponent 运行单个组件，只把「非停机期的非 nil 返回」视为致命错误上报。
// 致命错误就地记日志：Run 只取首个错误，多个组件同时挂掉时其余错误不至于无声无息。
func (a *App) runComponent(ctx context.Context, c Component, fatal chan<- error) {
	err := c.Start(ctx)
	switch {
	case err == nil:
		a.logger.Debug("组件运行结束", "component", c.Name())
	case a.stopping.Load() || ctx.Err() != nil:
		// 停机期的返回属预期（http.ErrServerClosed、grpc.ErrServerStopped 等），不上报。
		a.logger.Debug("组件停机退出", "component", c.Name(), "err", err)
	default:
		a.logger.Error("组件意外退出", "component", c.Name(), "err", err)
		fatal <- fmt.Errorf("运行组件 %s: %w: %w", c.Name(), ErrRunFailure, err)
	}
}

// stop 逆序停止全部组件并聚合错误；单个组件失败或超时都不中断其余组件的停止——
// 跳过等于资源不释放。每个组件分到的时间见 budgetFor。
func (a *App) stop(ctx context.Context) error {
	var errs []error
	for i := len(a.components) - 1; i >= 0; i-- {
		// i+1 是含当前这个在内的剩余组件数。
		budget := a.budgetFor(ctx, i+1)
		if err := a.stopOne(ctx, a.components[i], budget); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopOne 停止单个组件，最多等 budget：Stop 在独立 goroutine 中执行，到点没等到
// 就放弃等待、继续停下一个。不这么做的话，一个不肯返回的 Stop（如被流式 RPC 挂住
// 的 GracefulStop）会让排在它后面的组件全部停不掉。放弃等待会漏下该 goroutine，
// 进程正在退出，这个代价可以接受。done 带缓冲，故被放弃的 Stop 返回时也不会阻塞
// 在写入上。
//
// 组件拿到的 ctx 以 budget 为期限、且不携带停机 ctx 的取消信号：总预算耗尽后仍要
// 给每个剩余组件一点时间收尾（见 budgetFor），直接沿用已过期的 ctx 等于不给。
// 用 WithoutCancel 而非另起 Background：ctx 里的值（trace 上下文一类）要留给
// 组件的收尾动作，剥掉的只是取消信号与期限。
func (a *App) stopOne(ctx context.Context, c Component, budget time.Duration) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.Stop(stopCtx) }()

	select {
	case err := <-done:
		return a.stopResult(c, err)
	case <-stopCtx.Done():
	}
	// ctx 到期与 Stop 返回可能同时就绪，而 select 在这种情况下是随机选的：
	// 再非阻塞查一次，免得已经返回的组件被报成超时——那会掩盖真正卡住的那一个。
	select {
	case err := <-done:
		return a.stopResult(c, err)
	default:
	}

	a.logger.Error("组件停止超时，放弃等待", "component", c.Name(), "budget", budget.String())
	// 它最终的结果仍要记一条：组件报出的真实失败（如「关闭连接池失败：还有 3 个
	// 连接在用」）否则就此丢掉，上抛的只剩一句「宽限期耗尽」。
	go func() {
		if err := <-done; err != nil {
			a.logger.Error("组件在放弃等待后报告停止失败", "component", c.Name(), "err", err)
		}
	}()
	return fmt.Errorf("停止组件 %s: %w: %w", c.Name(), ErrStopTimeout, stopCtx.Err())
}

// stopResult 把组件 Stop 的返回值转成日志与错误。
func (a *App) stopResult(c Component, err error) error {
	if err != nil {
		a.logger.Error("组件停止失败", "component", c.Name(), "err", err)
		return fmt.Errorf("停止组件 %s: %w", c.Name(), err)
	}
	a.logger.Info("组件已停止", "component", c.Name())
	return nil
}

// await 等待全部运行 goroutine 退出；宽限期内没等到就放弃，不让不肯返回的 Start 拖住进程退出。
func (a *App) await(ctx context.Context, finished <-chan struct{}) {
	for pending := len(a.components); pending > 0; pending-- {
		select {
		case <-finished:
		case <-ctx.Done():
			a.logger.Warn("仍有组件的 Start 未返回，放弃等待", "pending", pending)
			return
		}
	}
}
