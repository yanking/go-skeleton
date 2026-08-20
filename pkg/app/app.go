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

// defaultStopTimeout 未配置时的停机总超时。
const defaultStopTimeout = 10 * time.Second

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

// Config App 的运行参数，零值即默认。
type Config struct {
	// StopTimeout 停机总超时（全部组件共享，非每组件），从开始逆序停止起算。
	// 超时后各组件的 Stop 仍会被调用，只是拿到已过期的 ctx，应立即强制关闭。零值取 10s。
	StopTimeout time.Duration
	// Logger 生命周期日志输出，nil 时用 slog.Default()。
	Logger *slog.Logger
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
		fatal <- fmt.Errorf("运行组件 %s: %w", c.Name(), err)
	}
}

// stop 逆序停止全部组件并聚合错误；单个组件失败或超时都不中断其余组件的停止——
// 跳过等于资源不释放。ctx 的宽限期由全部组件共享，耗尽后剩余组件仍会被调用 Stop。
func (a *App) stop(ctx context.Context) error {
	var errs []error
	for i := len(a.components) - 1; i >= 0; i-- {
		if err := a.stopOne(ctx, a.components[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopOne 停止单个组件：Stop 在独立 goroutine 中执行，宽限期内没等到就放弃等待、继续停下一个。
// 不这么做的话，一个不肯返回的 Stop（如被流式 RPC 挂住的 GracefulStop）会让排在它后面的
// 组件全部停不掉。放弃等待会漏下该 goroutine，进程正在退出，这个代价可以接受。
// done 带缓冲，故被放弃的 Stop 返回时也不会阻塞在写入上。
func (a *App) stopOne(ctx context.Context, c Component) error {
	done := make(chan error, 1)
	go func() { done <- c.Stop(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			a.logger.Error("组件停止失败", "component", c.Name(), "err", err)
			return fmt.Errorf("停止组件 %s: %w", c.Name(), err)
		}
		a.logger.Info("组件已停止", "component", c.Name())
		return nil
	case <-ctx.Done():
		a.logger.Error("组件停止超时，放弃等待", "component", c.Name())
		return fmt.Errorf("停止组件 %s: 宽限期耗尽，未等到返回: %w", c.Name(), ctx.Err())
	}
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
