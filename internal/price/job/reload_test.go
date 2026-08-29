package job

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// quietLogger 构造一个丢弃全部输出的 logger，避免测试断言与生命周期日志混在
// 一起，同时满足 NewReload 对 *slog.Logger 非 nil 的隐含要求。
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// errMockPlans、errMockRebuild 分别是 stubReloadService.Plans、
// stubRebuilder.Rebuild 预设失败时返回的错误，仅用于测试断言比对。
var (
	errMockPlans   = errors.New("mock 读取连接计划失败")
	errMockRebuild = errors.New("mock 重建连接失败")
)

// stubReloadService 是 ReloadService 的测试替身：每次 Plans 调用计数自增，
// onCall（若设置）在计数增加后同步触发，用于在指定的第 N 次调用上做断言/
// 释放等待，不依赖 time.Sleep 猜时间。
type stubReloadService struct {
	calls  atomic.Int32
	plans  map[string][]exchange.ConnPlan
	err    error
	onCall func(n int32)
}

func (s *stubReloadService) Plans(ctx context.Context) (map[string][]exchange.ConnPlan, error) {
	n := s.calls.Add(1)
	if s.onCall != nil {
		s.onCall(n)
	}
	return s.plans, s.err
}

// stubRebuilder 是 Rebuilder 的测试替身：记录调用次数与最近一次收到的
// plans，可预设固定失败。
type stubRebuilder struct {
	calls    atomic.Int32
	gotPlans atomic.Value // []exchange.ConnPlan
	err      error
	onCall   func(n int32)
}

func (r *stubRebuilder) Rebuild(ctx context.Context, plans []exchange.ConnPlan) error {
	n := r.calls.Add(1)
	r.gotPlans.Store(plans)
	if r.onCall != nil {
		r.onCall(n)
	}
	return r.err
}

// TestNewReload_ZeroIntervalTakesDefault 锚定 configs/price.yaml
// collector.reload_interval 字段注释「零值取 5m」：interval 非正数时
// NewReload 必须兜底到 defaultReloadInterval。
func TestNewReload_ZeroIntervalTakesDefault(t *testing.T) {
	c := NewReload(&stubReloadService{}, nil, 0, quietLogger())
	j, ok := c.(*reload)
	if !ok {
		t.Fatalf("NewReload() 返回 %T, want *reload", c)
	}
	if j.interval != defaultReloadInterval {
		t.Errorf("interval = %v, want 默认值 %v", j.interval, defaultReloadInterval)
	}
}

// TestNewReload_ConfiguredIntervalWins 锚定显式配置的 interval 不被默认值
// 覆盖。
func TestNewReload_ConfiguredIntervalWins(t *testing.T) {
	c := NewReload(&stubReloadService{}, nil, 30*time.Second, quietLogger())
	j := c.(*reload)
	if j.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", j.interval)
	}
}

// TestReload_RunsFirstRoundImmediately 锚定首轮立即执行：interval 调成
// 1 小时，若首轮不是立即跑，1 秒内不会等到 Plans 被调用。
func TestReload_RunsFirstRoundImmediately(t *testing.T) {
	done := make(chan struct{})
	svc := &stubReloadService{onCall: func(n int32) {
		if n == 1 {
			close(done)
		}
	}}
	c := NewReload(svc, nil, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("首轮未立即执行：等待 1s 仍未调用 Plans")
	}
}

// TestReload_RepeatsOnInterval 锚定按周期重复执行。
func TestReload_RepeatsOnInterval(t *testing.T) {
	third := make(chan struct{})
	svc := &stubReloadService{onCall: func(n int32) {
		if n == 3 {
			close(third)
		}
	}}
	c := NewReload(svc, nil, 5*time.Millisecond, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	select {
	case <-third:
	case <-time.After(time.Second):
		t.Fatalf("未按周期重复执行：1s 内只调用了 %d 次", svc.calls.Load())
	}
}

// TestReload_StartReturnsOnContextCancel 锚定 ctx 取消是唯一的正常退出
// 路径，返回值须为 nil。
func TestReload_StartReturnsOnContextCancel(t *testing.T) {
	c := NewReload(&stubReloadService{}, nil, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- c.Start(ctx) }()
	cancel()

	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("Start() error = %v, want nil(正常停机)", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 Start 未返回")
	}
}

// TestReload_PlansErrorDoesNotStopLoop 锚定「单轮出错只 Warn，下一轮
// 重试」：Plans 每次都失败，循环仍要按 interval 继续跑下去，不能因为一次
// 失败就卡住或退出。
func TestReload_PlansErrorDoesNotStopLoop(t *testing.T) {
	second := make(chan struct{})
	svc := &stubReloadService{err: errMockPlans, onCall: func(n int32) {
		if n == 2 {
			close(second)
		}
	}}
	c := NewReload(svc, nil, 5*time.Millisecond, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("Plans 持续报错后循环停止了，want 下一轮继续重试")
	}
}

// TestReload_RebuildsEachExchange 锚定核心接线：Plans 返回的每个交易所都要
// 拿各自的 plans 调用 mgrs 里对应的 Rebuild。
func TestReload_RebuildsEachExchange(t *testing.T) {
	plansA := []exchange.ConnPlan{{URL: "wss://a"}}
	plansB := []exchange.ConnPlan{{URL: "wss://b"}}

	doneA, doneB := make(chan struct{}), make(chan struct{})
	mgrA := &stubRebuilder{onCall: func(n int32) {
		if n == 1 {
			close(doneA)
		}
	}}
	mgrB := &stubRebuilder{onCall: func(n int32) {
		if n == 1 {
			close(doneB)
		}
	}}

	svc := &stubReloadService{plans: map[string][]exchange.ConnPlan{"a": plansA, "b": plansB}}
	c := NewReload(svc, map[string]Rebuilder{"a": mgrA, "b": mgrB}, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	waitClosed(t, doneA, "交易所 a 的 Rebuild 未被调用")
	waitClosed(t, doneB, "交易所 b 的 Rebuild 未被调用")

	gotA := mgrA.gotPlans.Load().([]exchange.ConnPlan)
	if len(gotA) != 1 || gotA[0].URL != "wss://a" {
		t.Errorf("mgrA 收到的 plans = %+v, want %+v", gotA, plansA)
	}
	gotB := mgrB.gotPlans.Load().([]exchange.ConnPlan)
	if len(gotB) != 1 || gotB[0].URL != "wss://b" {
		t.Errorf("mgrB 收到的 plans = %+v, want %+v", gotB, plansB)
	}
}

// TestReload_AllExchangesAttemptedEvenIfAllRebuildFail 是「单个交易所重建
// 失败不得中断其余交易所」的确定性主防线（裁决 R28）：与下面两条按遍历顺序
// 抽样断言的用例不同，这条让全部交易所都失败——Go 的 map 遍历顺序无论怎么
// 随机，正确实现（遇错只 Warn、继续遍历）总会尝试全部 n 次；一旦回归成
// 「遇错即 return」，不管哪个交易所先被访问到，总尝试次数都只会是 1，
// 与 n 恒不相等，不依赖任何一次具体的遍历顺序就能稳定转红，不是抽样式的
// 概率性验证。
func TestReload_AllExchangesAttemptedEvenIfAllRebuildFail(t *testing.T) {
	const n = 3
	var total atomic.Int32
	allDone := make(chan struct{})

	mgrs := make(map[string]Rebuilder, n)
	plans := make(map[string][]exchange.ConnPlan, n)
	for _, name := range []string{"a", "b", "c"} {
		mgrs[name] = &stubRebuilder{err: errMockRebuild, onCall: func(int32) {
			if total.Add(1) == n {
				close(allDone)
			}
		}}
		plans[name] = []exchange.ConnPlan{{URL: "wss://" + name}}
	}

	svc := &stubReloadService{plans: plans}
	c := NewReload(svc, mgrs, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	waitClosed(t, allDone, "全部交易所的 Rebuild 都失败时，未见到全部 n 次尝试——疑似遇错即 return，提前中断了后续交易所")

	if got := total.Load(); got != n {
		t.Fatalf("Rebuild 总调用次数 = %d, want %d（每个交易所都必须被尝试到）", got, n)
	}
}

// TestReload_SingleExchangeRebuildFailureDoesNotStopOthers 锚定「单个交易所
// 重建失败不得中断其余交易所」：mgrA 恒失败，其余两家（mgrB、mgrC）都要被
// 调用到——断言全部而不是只断言其中一个，把「失败项恰好排在遍历顺序末尾从而
// 掩盖 bug」的漏检概率从 2 家时的 1/2 压到 3 家时的 1/3（仍非绝对，主防线是
// 上面那条 all-fail 用例，裁决 R28）。
func TestReload_SingleExchangeRebuildFailureDoesNotStopOthers(t *testing.T) {
	doneA, doneB, doneC := make(chan struct{}), make(chan struct{}), make(chan struct{})
	mgrA := &stubRebuilder{err: errMockRebuild, onCall: func(n int32) {
		if n == 1 {
			close(doneA)
		}
	}}
	mgrB := &stubRebuilder{onCall: func(n int32) {
		if n == 1 {
			close(doneB)
		}
	}}
	mgrC := &stubRebuilder{onCall: func(n int32) {
		if n == 1 {
			close(doneC)
		}
	}}

	svc := &stubReloadService{plans: map[string][]exchange.ConnPlan{
		"a": {{URL: "wss://a"}},
		"b": {{URL: "wss://b"}},
		"c": {{URL: "wss://c"}},
	}}
	c := NewReload(svc, map[string]Rebuilder{"a": mgrA, "b": mgrB, "c": mgrC}, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	waitClosed(t, doneA, "交易所 a 的 Rebuild 未被调用")
	waitClosed(t, doneB, "交易所 a 重建失败后，交易所 b 未被继续调用")
	waitClosed(t, doneC, "交易所 a 重建失败后，交易所 c 未被继续调用")
}

// TestReload_MissingManagerSkipsButContinues 锚定 mgrs 里缺失某交易所时只
// 跳过该交易所，不影响其余交易所被正常重建（防御性分支：Plans 与 mgrs 理论
// 上应始终同键，但装配漂移不该演变成 panic）。与上一条同理：断言两个存在的
// manager（b、c）都被调用到，而不是只断言其中一个，降低「缺失项恰好排在
// 遍历顺序末尾从而掩盖 bug」的漏检概率——这条与 Rebuild 失败分支走的是
// 同一个 for 循环的不同分支（!ok 分支），但没有 all-fail 那样的确定性构造
// 可用（全部缺失时无从计数任何一次 Rebuild 调用），因此仍是概率性验证。
func TestReload_MissingManagerSkipsButContinues(t *testing.T) {
	doneB, doneC := make(chan struct{}), make(chan struct{})
	mgrB := &stubRebuilder{onCall: func(n int32) {
		if n == 1 {
			close(doneB)
		}
	}}
	mgrC := &stubRebuilder{onCall: func(n int32) {
		if n == 1 {
			close(doneC)
		}
	}}

	svc := &stubReloadService{plans: map[string][]exchange.ConnPlan{
		"missing": {{URL: "wss://missing"}},
		"b":       {{URL: "wss://b"}},
		"c":       {{URL: "wss://c"}},
	}}
	c := NewReload(svc, map[string]Rebuilder{"b": mgrB, "c": mgrC}, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	waitClosed(t, doneB, "mgrs 缺失一个交易所不该连累其余交易所 b 被跳过")
	waitClosed(t, doneC, "mgrs 缺失一个交易所不该连累其余交易所 c 被跳过")
}

// TestReload_Stop_ToleratesNotYetStarted 锚定 pkg/app.Component 的约定：Stop
// 可能在组件尚未真正运行时被调用，实现须容忍，不panic、不阻塞。
func TestReload_Stop_ToleratesNotYetStarted(t *testing.T) {
	c := NewReload(&stubReloadService{}, nil, time.Hour, quietLogger())

	if err := c.Stop(context.Background()); err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
}

// TestReload_StopWaitsForInFlightReloadOnceToFinish 锚定评审 Important 4 的
// 修复：Stop 必须等当前这一轮 reloadOnce（包括其中对 Rebuilder.Rebuild 的
// 调用）真正跑完才能返回，不能像旧版本那样无条件立即返回 nil——旧行为会
// 打开一个窗口：Stop 提前返回后 pkg/app 紧接着调用下一个组件（某个
// stream.Manager）的 Stop，而这次仍在跑的 Rebuild 可能恰好在 Manager.Stop
// 已经清空连接、释放锁之后才真正执行，凭空建出一批不受任何 Stop 管辖的新
// 连接（细则见 reload.Stop 的方法注释）。
func TestReload_StopWaitsForInFlightReloadOnceToFinish(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var closeOnce sync.Once
	mgr := &stubRebuilder{onCall: func(n int32) {
		closeOnce.Do(func() { close(started) })
		<-release // 卡住，模拟 Rebuild 还没跑完
	}}
	svc := &stubReloadService{plans: map[string][]exchange.ConnPlan{"a": {{URL: "wss://a"}}}}
	c := NewReload(svc, map[string]Rebuilder{"a": mgr}, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Start(ctx) }()

	waitClosed(t, started, "Rebuild 未被调用")
	cancel() // 触发停机：根 ctx 取消

	stopDone := make(chan error, 1)
	go func() { stopDone <- c.Stop(context.Background()) }()

	select {
	case <-stopDone:
		t.Fatal("Rebuild 仍未返回（release 未放行），Stop 不该提前返回")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop 返回值 = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("release 放行后 Stop 应及时返回，未在预期时间内返回")
	}
}

// TestReload_StopReturnsErrorWhenLoopNeverReturns 锚定 Stop 的宽限期语义：
// 循环永不返回（Rebuild 永久卡住）时，Stop 必须在调用方给的 ctx 到期后报错
// 返回，而不是死等——这个 ctx 是 pkg/app 全部组件共享的停机预算，死等会
// 拖累按逆序排在后面的组件挤不出停机时间。
func TestReload_StopReturnsErrorWhenLoopNeverReturns(t *testing.T) {
	started := make(chan struct{})
	var closeOnce sync.Once
	mgr := &stubRebuilder{onCall: func(n int32) {
		closeOnce.Do(func() { close(started) })
		select {} // 永久卡住，不响应任何信号
	}}
	svc := &stubReloadService{plans: map[string][]exchange.ConnPlan{"a": {{URL: "wss://a"}}}}
	c := NewReload(svc, map[string]Rebuilder{"a": mgr}, time.Hour, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	waitClosed(t, started, "Rebuild 未被调用")
	cancel()

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stopCancel()
	if err := c.Stop(stopCtx); err == nil {
		t.Fatal("循环永不返回时 Stop 应在 ctx 到期后报错返回，got nil")
	}
}

// waitClosed 等待 ch 被关闭，超时 1s 视为失败。
func waitClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal(msg)
	}
}
