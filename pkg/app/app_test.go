// app 的导出行为集中在 New / Run 与 Component 接口，按 go-style 用 _test 包做黑盒测试。
package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/yanking/go-skeleton/pkg/app"
)

// discardLogger 丢弃全部日志，避免生命周期日志淹没测试输出。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recorder 按发生顺序记录组件的启停调用，供同一用例内多个 fake 组件共享。
type recorder struct {
	mu    sync.Mutex
	calls []string
}

// add 追加一条调用记录。
func (r *recorder) add(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

// snapshot 返回调用记录的副本，可在 Run 运行期间安全调用。
func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// only 从调用记录里筛出指定前缀的部分。组件是并发拉起的，start 记录之间的顺序天然不确定，
// 只有 stop 顺序由 App 串行决定、可断言，故用它把两类记录分开看。
func only(calls []string, prefix string) []string {
	var got []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			got = append(got, c)
		}
	}
	return got
}

// fakeComponent 测试用组件：Start 记录调用后阻塞，直到 Stop 放行，模拟 Serve 一类的常驻循环。
type fakeComponent struct {
	name     string
	rec      *recorder
	quit     chan struct{}
	quitOnce sync.Once
	startErr error                     // 非 nil 时 Start 立即返回该错误，模拟组件意外退出
	runErr   error                     // 非 nil 时 Start 在被 Stop 放行后返回该错误
	stopErr  error                     // 非 nil 时 Stop 返回该错误
	onStart  func()                    // 非 nil 时在 Start 记录调用后执行
	onStop   func(ctx context.Context) // 非 nil 时在 Stop 记录调用后执行，用于观察停机 ctx
}

// newFake 构造一个默认行为（阻塞运行、正常停止）的测试组件。
func newFake(name string, rec *recorder) *fakeComponent {
	return &fakeComponent{name: name, rec: rec, quit: make(chan struct{})}
}

// Name 返回组件名。
func (c *fakeComponent) Name() string { return c.name }

// Start 记录调用后阻塞到 Stop 放行，或按预置的 startErr 立即失败。
func (c *fakeComponent) Start(context.Context) error {
	c.rec.add("start:" + c.name)
	if c.onStart != nil {
		c.onStart()
	}
	if c.startErr != nil {
		return c.startErr
	}
	<-c.quit
	return c.runErr
}

// Stop 记录调用、放行阻塞中的 Start，并返回预置的停止错误。
func (c *fakeComponent) Stop(ctx context.Context) error {
	c.rec.add("stop:" + c.name)
	if c.onStop != nil {
		c.onStop(ctx)
	}
	c.quitOnce.Do(func() { close(c.quit) })
	return c.stopErr
}

// startAll 让全部组件在拉起时登记，返回的函数阻塞到它们都已进入 Start。
func startAll(comps ...*fakeComponent) func() {
	var started sync.WaitGroup
	started.Add(len(comps))
	for _, c := range comps {
		c.onStart = started.Done
	}
	return started.Wait
}

func TestRunStopsComponentsInReverseOrder(t *testing.T) {
	rec := &recorder{}
	data, grpc, http := newFake("data", rec), newFake("grpc", rec), newFake("http", rec)
	waitStarted := startAll(data, grpc, http)
	a := app.New(app.Config{Logger: discardLogger()}, data, grpc, http)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitStarted()
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("ctx 取消属正常退出，Run 不应返回错误, got %v", err)
	}

	calls := rec.snapshot()
	if got := len(only(calls, "start:")); got != 3 {
		t.Errorf("三个组件都应被拉起, got %d", got)
	}
	want := []string{"stop:http", "stop:grpc", "stop:data"}
	if diff := cmp.Diff(want, only(calls, "stop:")); diff != "" {
		t.Errorf("停止顺序不符 (-want +got):\n%s", diff)
	}
}

func TestRunIgnoresStartErrorsDuringShutdown(t *testing.T) {
	rec := &recorder{}
	// 模拟 http.Server：Shutdown 之后 Serve 返回 ErrServerClosed——这类返回由 App 识别，不算致命错误。
	c := newFake("http", rec)
	c.runErr = errors.New("http: Server closed")
	waitStarted := startAll(c)
	a := app.New(app.Config{Logger: discardLogger()}, c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitStarted()
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("停机期组件返回的错误不应被上报, got %v", err)
	}
}

func TestRunStopsOnComponentFailure(t *testing.T) {
	rec := &recorder{}
	wantErr := errors.New("监听器已关闭")
	data, grpc, http := newFake("data", rec), newFake("grpc", rec), newFake("http", rec)
	grpc.startErr = wantErr

	a := app.New(app.Config{Logger: discardLogger()}, data, grpc, http)

	// ctx 全程不取消：停机必须由组件的意外退出触发。
	err := a.Run(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Run 应返回组件的退出错误, got %v", err)
	}
	if !strings.Contains(err.Error(), "grpc") {
		t.Errorf("错误应标注退出的组件名, got %q", err)
	}

	// 已退出的组件同样会收到 Stop，实现须容忍——这正是 Component.Stop 的契约。
	want := []string{"stop:http", "stop:grpc", "stop:data"}
	if diff := cmp.Diff(want, only(rec.snapshot(), "stop:")); diff != "" {
		t.Errorf("停止顺序不符 (-want +got):\n%s", diff)
	}
}

func TestRunAggregatesStopErrors(t *testing.T) {
	rec := &recorder{}
	dataErr, grpcErr := errors.New("连接池关闭失败"), errors.New("优雅停止失败")
	data, grpc, http := newFake("data", rec), newFake("grpc", rec), newFake("http", rec)
	data.stopErr, grpc.stopErr = dataErr, grpcErr
	waitStarted := startAll(data, grpc, http)
	a := app.New(app.Config{Logger: discardLogger()}, data, grpc, http)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitStarted()
	cancel()

	err := <-done
	if !errors.Is(err, dataErr) || !errors.Is(err, grpcErr) {
		t.Fatalf("两个组件的停止错误都应被聚合, got %v", err)
	}

	// 某组件停止失败不得中断其余组件的停止。
	want := []string{"stop:http", "stop:grpc", "stop:data"}
	if diff := cmp.Diff(want, only(rec.snapshot(), "stop:")); diff != "" {
		t.Errorf("停止顺序不符 (-want +got):\n%s", diff)
	}
}

func TestStopContextIsDecoupledFromRunContext(t *testing.T) {
	tests := []struct {
		name        string
		stopTimeout time.Duration
		want        time.Duration // 期望的停机 ctx 剩余时长上限
	}{
		{name: "显式配置停机超时", stopTimeout: 3 * time.Second, want: 3 * time.Second},
		{name: "零值取默认 10s", stopTimeout: 0, want: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			var (
				gotErr      error
				gotDeadline time.Time
				gotHasDL    bool
			)
			c := newFake("http", rec)
			c.onStop = func(ctx context.Context) {
				gotErr = ctx.Err()
				gotDeadline, gotHasDL = ctx.Deadline()
			}
			waitStarted := startAll(c)
			a := app.New(app.Config{Logger: discardLogger(), StopTimeout: tt.stopTimeout}, c)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- a.Run(ctx) }()

			waitStarted()
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Run 返回错误: %v", err)
			}

			// 外部 ctx 已因信号取消，停机 ctx 必须仍然有效，否则宽限期为零。
			if gotErr != nil {
				t.Errorf("停机 ctx 不应已取消, got %v", gotErr)
			}
			if !gotHasDL {
				t.Fatal("停机 ctx 应带 deadline")
			}
			if remaining := time.Until(gotDeadline); remaining <= tt.want-time.Second || remaining > tt.want {
				t.Errorf("停机 ctx 剩余时长 %v, 期望落在 (%v, %v]", remaining, tt.want-time.Second, tt.want)
			}
		})
	}
}

func TestStopContinuesAfterBlockedStop(t *testing.T) {
	rec := &recorder{}
	data, grpc, http := newFake("data", rec), newFake("grpc", rec), newFake("http", rec)

	dataStopped := make(chan struct{})
	var dataStopErr error
	data.onStop = func(ctx context.Context) {
		dataStopErr = ctx.Err()
		close(dataStopped)
	}
	// 模拟被流式 RPC 挂住、永不返回的 GracefulStop：它不得拖住排在其后的 data。
	grpc.onStop = func(context.Context) { select {} }

	waitStarted := startAll(data, grpc, http)
	a := app.New(app.Config{Logger: discardLogger(), StopTimeout: 100 * time.Millisecond}, data, grpc, http)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitStarted()
	cancel()

	err := <-done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("放弃等待应作为错误上报, got %v", err)
	}
	if !strings.Contains(err.Error(), "grpc") {
		t.Errorf("错误应标注卡住的组件名, got %q", err)
	}

	select {
	case <-dataStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("宽限期被 grpc 耗尽，但 data 仍须被停止——跳过等于资源不释放")
	}
	if !errors.Is(dataStopErr, context.DeadlineExceeded) {
		t.Errorf("宽限期耗尽后组件应拿到已过期的停机 ctx, got %v", dataStopErr)
	}
}

func TestRunLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newFake("http", rec)
	c.onStart = cancel // 拉起即取消，Run 随后直接进入停机；全程在测试 goroutine 内完成
	a := app.New(app.Config{Logger: discardLogger()}, c)

	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}

	// Run 返回前已等到全部运行 goroutine 交回，剩下的只是它们执行完最后一条语句到真正退出的窗口。
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Errorf("Run 返回后仍有 goroutine 未退出: before=%d got=%d", before, got)
	}
}

func TestRunWithoutComponents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := app.New(app.Config{Logger: discardLogger()}).Run(ctx); err != nil {
		t.Fatalf("空组件列表应能正常收敛, got %v", err)
	}
}

func TestRunWithCanceledContextStopsEverything(t *testing.T) {
	rec := &recorder{}
	c := newFake("data", rec)
	a := app.New(app.Config{Logger: discardLogger()}, c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := a.Run(ctx); err != nil {
		t.Fatalf("ctx 取消属正常退出, got %v", err)
	}
	// 组件是否来得及进入 Start 不确定（并发拉起），但 Stop 必然被调用。
	want := []string{"stop:data"}
	if diff := cmp.Diff(want, only(rec.snapshot(), "stop:")); diff != "" {
		t.Errorf("组件应被停止 (-want +got):\n%s", diff)
	}
}
