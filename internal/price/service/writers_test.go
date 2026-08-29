package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
)

// errMockUpsert 是 mockKlineRepo 预设失败时返回的错误，仅用于测试断言比对。
var errMockUpsert = errors.New("mock upsert 失败")

// mockKlineRepo 是 KlineRepo 的测试替身：记录每次 Upsert 收到的批次，可预设
// 前 failFirst 次调用返回错误（模拟写库失败），并对已取消的 ctx 提前短路
// 返回——这一条是精确复现 Critical 缺陷（复用已取消 ctx 导致“尽力落盘”
// 空转）所必需的，真实的 GORM WithContext 就是这个行为。
type mockKlineRepo struct {
	mu        sync.Mutex
	calls     [][]model.Kline
	failFirst int

	// notify 在每次 Upsert 返回前非阻塞地 ping 一下，供测试用 select 等待
	// 调用发生，避免用 sleep 轮询造成的不确定性。
	notify chan struct{}

	// maxOpenTime、has 是 MaxOpenTime 的预设返回值，供 backfill_test.go
	// 驱动「续接上次」与「库为空回溯」两条起点分支；本文件的用例都不设置
	// （零值 has=false），行为与之前一致。
	maxOpenTime int64
	has         bool
}

func newMockKlineRepo() *mockKlineRepo {
	return &mockKlineRepo{notify: make(chan struct{}, 256)}
}

func (r *mockKlineRepo) Upsert(ctx context.Context, rows []model.Kline) error {
	defer func() {
		select {
		case r.notify <- struct{}{}:
		default:
		}
	}()

	if err := ctx.Err(); err != nil {
		// 模拟 database/sql 对已取消 ctx 在真正发请求前就短路返回、不落一
		// 行的行为。
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]model.Kline(nil), rows...))
	if len(r.calls) <= r.failFirst {
		return errMockUpsert
	}
	return nil
}

func (r *mockKlineRepo) MaxOpenTime(ctx context.Context, exchange, market, nativeSymbol, interval string) (int64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxOpenTime, r.has, nil
}

// snapshot 返回目前为止全部 Upsert 调用收到的批次副本，按调用顺序。
func (r *mockKlineRepo) snapshot() [][]model.Kline {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]model.Kline(nil), r.calls...)
}

// mockLatestRepo 是 LatestRepo 的测试替身：可预设 Set 恒定返回 err，用于验证
// Redis 故障不牵连 kline 链路。
type mockLatestRepo struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (r *mockLatestRepo) Set(ctx context.Context, key string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.err
}

// waitForCalls 等到 notify 上累计收到至少 n 次通知，超时则让测试失败。用
// select 而不是 sleep 轮询，避免测试本身引入不必要的不确定性。
func waitForCalls(t *testing.T, notify <-chan struct{}, n int, timeout time.Duration) {
	t.Helper()
	got := 0
	deadline := time.After(timeout)
	for got < n {
		select {
		case <-notify:
			got++
		case <-deadline:
			t.Fatalf("等待第 %d 次 Upsert 调用超时，只观察到 %d 次", n, got)
		}
	}
}

// newWritersTestService 构造一个已起好 RunWriters 的 Price，返回 svc、
// RouteFor("binance") 绑定的入口，以及停止函数（cancel 并等 RunWriters 真正
// 退出）。
func newWritersTestService(t *testing.T, cfg Config, klines *mockKlineRepo, latest *mockLatestRepo) (*Price, func(exchange.Event), func()) {
	t.Helper()
	svc := New(cfg, Deps{Klines: klines, Latest: latest}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.RunWriters(ctx)
	}()

	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("RunWriters 未在停机后及时返回")
		}
	}
	return svc, svc.RouteFor("binance"), stop
}

// TestRunKlineWriter_ShutdownFlushPersistsPendingBatch 是 Critical 缺陷的
// 回归锚点：停机时还攒在内存里、没到攒批阈值的 kline，必须被真正落库，不能
// 因为复用了已取消的 ctx 而无声丢弃。
func TestRunKlineWriter_ShutdownFlushPersistsPendingBatch(t *testing.T) {
	klines := newMockKlineRepo()
	_, route, stop := newWritersTestService(t, Config{KlineQueueSize: 8, SnapshotQueueSize: 1}, klines, &mockLatestRepo{})

	route(exchange.Event{Kline: &exchange.Kline{NativeSymbol: "BTCUSDT", OpenTime: 1}})
	route(exchange.Event{Kline: &exchange.Kline{NativeSymbol: "BTCUSDT", OpenTime: 2}})

	// 不额外等待、不 sleep：不管这两条此刻是已经被 runKlineWriter 读进
	// batch，还是仍原样躺在 s.klineCh 缓冲区里，停机路径都必须把两者都收全
	// 再落库——这正是要锚定的行为，不能靠掐时间点凑巧通过。stop() 内部会
	// cancel 并等 RunWriters 真正返回。
	stop()

	calls := klines.snapshot()
	total := 0
	for _, c := range calls {
		total += len(c)
	}
	if len(calls) == 0 || total != 2 {
		t.Fatalf("停机时应把在途的 2 条 kline 落盘, got calls=%+v(共 %d 条)", calls, total)
	}
}

// TestRunKlineWriter_FlushesAtBatchSizeThreshold 锚定攒批的第一个触发条件：
// 凑够 klineBatchSize 条立即触发一次 Upsert，不等定时器。
func TestRunKlineWriter_FlushesAtBatchSizeThreshold(t *testing.T) {
	klines := newMockKlineRepo()
	_, route, stop := newWritersTestService(t, Config{KlineQueueSize: klineBatchSize + 10, SnapshotQueueSize: 1}, klines, &mockLatestRepo{})
	defer stop()

	for i := 0; i < klineBatchSize; i++ {
		route(exchange.Event{Kline: &exchange.Kline{NativeSymbol: "BTCUSDT", OpenTime: int64(i)}})
	}

	waitForCalls(t, klines.notify, 1, time.Second)

	calls := klines.snapshot()
	if len(calls) != 1 || len(calls[0]) != klineBatchSize {
		t.Fatalf("凑够 klineBatchSize 条应恰好触发一次 %d 条的 Upsert, got calls=%v", klineBatchSize, callLens(calls))
	}
}

// TestRunKlineWriter_FlushesAtIntervalWhenBelowThreshold 锚定攒批的第二个
// 触发条件：不足 klineBatchSize 条，但超过 klineBatchInterval 也要落库，不能
// 让稀疏的收线迟迟不落盘。
func TestRunKlineWriter_FlushesAtIntervalWhenBelowThreshold(t *testing.T) {
	klines := newMockKlineRepo()
	_, route, stop := newWritersTestService(t, Config{KlineQueueSize: 8, SnapshotQueueSize: 1}, klines, &mockLatestRepo{})
	defer stop()

	route(exchange.Event{Kline: &exchange.Kline{NativeSymbol: "BTCUSDT", OpenTime: 1}})

	waitForCalls(t, klines.notify, 1, klineBatchInterval+time.Second)

	calls := klines.snapshot()
	if len(calls) != 1 || len(calls[0]) != 1 {
		t.Fatalf("未攒满但超过 klineBatchInterval 应触发一次 1 条的 Upsert, got calls=%v", callLens(calls))
	}
}

// TestRunKlineWriter_RetriesOnUpsertFailureUntilSuccess 锚定写库失败的处理
// 方式：原地重试直到成功，不丢弃、不跳过——kline 不可丢。
func TestRunKlineWriter_RetriesOnUpsertFailureUntilSuccess(t *testing.T) {
	klines := newMockKlineRepo()
	klines.failFirst = 2 // 前两次调用失败，第三次成功

	_, route, stop := newWritersTestService(t, Config{KlineQueueSize: 8, SnapshotQueueSize: 1}, klines, &mockLatestRepo{})
	defer stop()

	route(exchange.Event{Kline: &exchange.Kline{NativeSymbol: "BTCUSDT", OpenTime: 1}})

	// 第一次尝试要等 klineBatchInterval 定时器触发（只有 1 条，够不着攒批
	// 阈值）；之后每次失败按 klineRetryInterval 重试，等到第 3 次调用。
	waitForCalls(t, klines.notify, 3, klineBatchInterval+2*klineRetryInterval+2*time.Second)

	calls := klines.snapshot()
	if len(calls) < 3 {
		t.Fatalf("应观察到至少 3 次 Upsert 调用（前两次失败 + 第三次成功）, got %d 次", len(calls))
	}
	for i, c := range calls[:3] {
		if len(c) != 1 || c[0].NativeSymbol != "BTCUSDT" {
			t.Errorf("第 %d 次调用的数据 = %+v, want 1 条 BTCUSDT（重试期间 batch 不能变、不能丢）", i+1, c)
		}
	}
}

// TestRunWriters_LatestFailureDoesNotBlockKlineWriter 锚定两条写链路的故障
// 域相互独立：Redis 一直写失败，不能拖慢/卡住 kline 的批量落库。
func TestRunWriters_LatestFailureDoesNotBlockKlineWriter(t *testing.T) {
	klines := newMockKlineRepo()
	latest := &mockLatestRepo{err: errors.New("redis 不可达")}

	_, route, stop := newWritersTestService(t, Config{KlineQueueSize: klineBatchSize + 10, SnapshotQueueSize: 1}, klines, latest)
	defer stop()

	for i := 0; i < 5; i++ {
		route(exchange.Event{Snapshot: &exchange.Snapshot{
			NativeSymbol: "BTCUSDT", StreamType: exchange.StreamDepth, EventTime: int64(i)}})
	}
	for i := 0; i < klineBatchSize; i++ {
		route(exchange.Event{Kline: &exchange.Kline{NativeSymbol: "BTCUSDT", OpenTime: int64(i)}})
	}

	waitForCalls(t, klines.notify, 1, time.Second)

	calls := klines.snapshot()
	if len(calls) != 1 || len(calls[0]) != klineBatchSize {
		t.Fatalf("Redis 写失败不应牵连 kline 落库, got calls=%v", callLens(calls))
	}
}

// callLens 把 mockKlineRepo.snapshot() 的结果精简成每次调用的条数，方便
// 失败信息里看批次大小而不是整批数据。
func callLens(calls [][]model.Kline) []int {
	out := make([]int, len(calls))
	for i, c := range calls {
		out[i] = len(c)
	}
	return out
}
