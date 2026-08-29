package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
	"github.com/yanking/go-skeleton/internal/price/ratelimit"
)

// errMockKlines 是 mockExchange 预设失败时 Klines 返回的错误，仅用于测试
// 断言比对。
var errMockKlines = errors.New("mock klines 失败")

// mockExchange 是 exchange.Exchange 的测试替身：本文件关心 Klines（补洞），
// instruments_test.go（Task 11）关心 Instruments（交易对导入）；记录每次
// Klines 调用收到的 start/end，并按 klinesFn（未设置则返回空数组、下一页
// 起点 0）决定返回值，Instruments 直接返回预设的 instruments/instrumentsErr；
// 其余方法本包不会触碰，给零值实现满足接口即可。
type mockExchange struct {
	mu             sync.Mutex
	gotStart       int64
	gotEnd         int64
	klinesCalls    int
	klinesFn       func(s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error)
	instruments    []exchange.Instrument
	instrumentsErr error
}

func (e *mockExchange) Name() string { return "m" }

func (e *mockExchange) Plan(subs []exchange.Sub) ([]exchange.ConnPlan, error) { return nil, nil }

func (e *mockExchange) Decode(raw []byte) (exchange.Event, error) { return exchange.Event{}, nil }

func (e *mockExchange) Instruments(ctx context.Context, market string) ([]exchange.Instrument, error) {
	return e.instruments, e.instrumentsErr
}

func (e *mockExchange) Klines(ctx context.Context, s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
	e.mu.Lock()
	e.gotStart = start
	e.gotEnd = end
	e.klinesCalls++
	fn := e.klinesFn
	e.mu.Unlock()
	if fn != nil {
		return fn(s, start, end)
	}
	return nil, 0, nil
}

func (e *mockExchange) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.klinesCalls
}

// TestBackfill_StartsAfterLastStoredKline 锚定水位线追赶式补洞的核心行为：
// 库里已有该标的该周期的历史时，起点必须直接是 MaxOpenTime 查到的「上一根
// 已存 K 线」的开盘时间本身——不在 service 层做「+ 一个周期」的周期换算
// （周期拼写是交易所方言，留在各自 adapter 内部；见 backfill.go
// backfillOne 的注释）。重新拉回这一根是无害的：K 线落库走 Upsert，用同样
// 的值覆盖同一行不产生重复。
func TestBackfill_StartsAfterLastStoredKline(t *testing.T) {
	klines := &mockKlineRepo{maxOpenTime: 1700000000000, has: true}
	ex := &mockExchange{}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.Backfill(context.Background(), "m", []exchange.Sub{sub}); err != nil {
		t.Fatal(err)
	}
	if ex.gotStart != 1700000000000 {
		t.Errorf("起点 = %d, want 上一根已存 K 线的开盘时间本身 = %d", ex.gotStart, int64(1700000000000))
	}
}

// TestBackfill_ResumesFromLastStoredKlineWithoutInfiniteLoop 锚定「起点等于
// last 时不会死循环」：Klines 用 start 本身当开盘时间返回一根、下一页起点为
// 0，翻页必须就此终止——用一个带超时的 goroutine 显式验证 Backfill 会返回，
// 不依赖 go test 自身的整体超时（那样即使真死循环也要等很久、失败信息也不
// 直接指向这里）。
func TestBackfill_ResumesFromLastStoredKlineWithoutInfiniteLoop(t *testing.T) {
	klines := &mockKlineRepo{maxOpenTime: 1700000000000, has: true}
	ex := &mockExchange{
		klinesFn: func(s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
			return []exchange.Kline{{NativeSymbol: s.NativeSymbol, Interval: s.Interval, OpenTime: start}}, 0, nil
		},
	}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}

	done := make(chan error, 1)
	go func() {
		done <- svc.Backfill(context.Background(), "m", []exchange.Sub{sub})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Backfill 未在预期时间内返回，疑似 next==0 后仍在翻页死循环")
	}

	if ex.callCount() != 1 {
		t.Errorf("起点等于 last、next 立即为 0 时应只调用一次 Klines 就终止, 调用次数 = %d, want 1", ex.callCount())
	}
	if ex.gotStart != 1700000000000 {
		t.Errorf("起点 = %d, want last 本身(不做周期换算) = %d", ex.gotStart, int64(1700000000000))
	}
}

// TestBackfill_FallsBackToMaxWindowWhenEmpty 锚定库为空时的起点：必须回溯
// Config.MaxBackfillWindow，不能取 0——否则新标的第一次补洞会拉全部历史。
func TestBackfill_FallsBackToMaxWindowWhenEmpty(t *testing.T) {
	klines := &mockKlineRepo{} // has 零值 false：库里没有任何已存 K 线
	ex := &mockExchange{}
	window := 720 * time.Hour
	svc := New(Config{MaxBackfillWindow: window}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}

	before := time.Now().Add(-window).UnixMilli()
	if err := svc.Backfill(context.Background(), "m", []exchange.Sub{sub}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(-window).UnixMilli()

	if ex.gotStart == 0 {
		t.Fatal("起点不能是 0，否则会拉取全部历史")
	}
	if ex.gotStart < before || ex.gotStart > after {
		t.Errorf("起点 = %d, want 落在 [now-window] 附近的区间 [%d, %d]", ex.gotStart, before, after)
	}
}

// TestBackfill_FallsBackToDefaultWindowWhenConfigZero 锚定
// Config.MaxBackfillWindow 零值时的兜底：不能静默退化成「不回溯」
// （time.Now().Add(-0) == now，会让新标的一根历史都拿不到、且没有任何报错
// 或日志——这是本项目里最危险的失败形态）。零值必须兜底到与
// configs/price.yaml 文档值一致的 defaultMaxBackfillWindow。
func TestBackfill_FallsBackToDefaultWindowWhenConfigZero(t *testing.T) {
	klines := &mockKlineRepo{} // has 零值 false：库里没有任何已存 K 线
	ex := &mockExchange{}
	svc := New(Config{}, Deps{ // MaxBackfillWindow 未显式配置，零值
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}

	before := time.Now().Add(-defaultMaxBackfillWindow).UnixMilli()
	if err := svc.Backfill(context.Background(), "m", []exchange.Sub{sub}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(-defaultMaxBackfillWindow).UnixMilli()

	if ex.gotStart == 0 {
		t.Fatal("起点不能是 0，否则会拉取全部历史")
	}
	if ex.gotStart < before || ex.gotStart > after {
		t.Errorf("起点 = %d, want 落在 [now-defaultMaxBackfillWindow] 附近的区间 [%d, %d]", ex.gotStart, before, after)
	}
	// 单独断言"没有退化成 now"：即便上面的区间断言因为某种巧合通过，这里
	// 再直接锚定起点距今至少要有大半个默认窗口那么久，否则说明零值兜底根本
	// 没生效、实际把 0 当成了"不回溯"。
	if gap := time.Now().UnixMilli() - ex.gotStart; gap < int64(defaultMaxBackfillWindow.Milliseconds())/2 {
		t.Errorf("起点距今仅 %dms，看起来 MaxBackfillWindow 零值兜底未生效(退化成了 now)", gap)
	}
}

// TestBackfill_SkipsNonKlineSubs 锚定 ticker/depth 订阅没有历史可补：不得
// 调用 Klines。
func TestBackfill_SkipsNonKlineSubs(t *testing.T) {
	klines := &mockKlineRepo{}
	ex := &mockExchange{}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	subs := []exchange.Sub{
		{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamTicker},
		{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamDepth},
	}
	if err := svc.Backfill(context.Background(), "m", subs); err != nil {
		t.Fatal(err)
	}
	if ex.callCount() != 0 {
		t.Errorf("ticker/depth 订阅没有历史可补，Klines 调用次数 = %d, want 0", ex.callCount())
	}
}

// TestBackfill_PagesUntilNextIsZero 锚定翻页行为：沿 Klines 返回的下一页
// 起点续接，直到它返回 0；每一页都要落库，且 Source 必须标记为补洞回填
// （不是实时流），与 route.go 的 toModelKline 默认值区分开。
func TestBackfill_PagesUntilNextIsZero(t *testing.T) {
	klines := newMockKlineRepo()
	ex := &mockExchange{
		klinesFn: func(s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
			if start == 1000 {
				return []exchange.Kline{{NativeSymbol: "BTCUSDT", Interval: "1m", OpenTime: 1000}}, 2000, nil
			}
			return []exchange.Kline{{NativeSymbol: "BTCUSDT", Interval: "1m", OpenTime: 2000}}, 0, nil
		},
	}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	klines.maxOpenTime, klines.has = 1000, true // 起点直接取 last 本身，令首个起点恰好是 1000

	if err := svc.Backfill(context.Background(), "m", []exchange.Sub{sub}); err != nil {
		t.Fatal(err)
	}

	if ex.callCount() != 2 {
		t.Fatalf("应翻两页（次数由 next!=0/next==0 决定）, Klines 调用次数 = %d, want 2", ex.callCount())
	}

	calls := klines.snapshot()
	var total []model.Kline
	for _, c := range calls {
		total = append(total, c...)
	}
	if len(total) != 2 {
		t.Fatalf("两页各一条，应写入 2 条, got %d", len(total))
	}
	for _, k := range total {
		if k.Source != model.KlineSourceBackfill {
			t.Errorf("Source = %d, want model.KlineSourceBackfill(%d)", k.Source, model.KlineSourceBackfill)
		}
	}
}

// TestBackfill_ContinuesAfterSingleSubFailure 锚定「单条订阅失败只 Warn 并
// 继续」：同一次 Backfill 调用里，一个坏标的报错，不能中断同批其余订阅的
// 补洞，Backfill 本身也不该把这类订阅级失败当作整批失败返回。
func TestBackfill_ContinuesAfterSingleSubFailure(t *testing.T) {
	klines := newMockKlineRepo()
	ex := &mockExchange{
		klinesFn: func(s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
			if s.NativeSymbol == "BADUSDT" {
				return nil, 0, errMockKlines
			}
			return []exchange.Kline{{NativeSymbol: s.NativeSymbol, Interval: "1m", OpenTime: 1}}, 0, nil
		},
	}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour, BackfillConcurrency: 1}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	badSub := exchange.Sub{Market: "spot", NativeSymbol: "BADUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	goodSub := exchange.Sub{Market: "spot", NativeSymbol: "GOODUSDT", StreamType: exchange.StreamKline, Interval: "1m"}

	if err := svc.Backfill(context.Background(), "m", []exchange.Sub{badSub, goodSub}); err != nil {
		t.Fatalf("单条订阅失败不该让 Backfill 整体返回 error, got %v", err)
	}

	total := 0
	for _, c := range klines.snapshot() {
		total += len(c)
	}
	if total != 1 {
		t.Fatalf("坏订阅失败不该阻止同批好订阅写库, 落库条数 = %d, want 1", total)
	}
}

// TestBackfill_WaitsOnRateLimitBucket 锚定「每次 REST 调用前必须
// Limits[ex].Wait(ctx)」：ctx 预先取消时 Wait 立即返回 context.Canceled
// （golang.org/x/time/rate 的既有行为，见 Wait 源码对已取消 ctx 的短路），
// Klines 必须一次都不被调用，且失败按「单条订阅失败只 Warn」处理、不让
// Backfill 整体报错。
func TestBackfill_WaitsOnRateLimitBucket(t *testing.T) {
	klines := &mockKlineRepo{}
	ex := &mockExchange{}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.Backfill(ctx, "m", []exchange.Sub{sub}); err != nil {
		t.Fatalf("限速等待失败属于单条订阅失败, Backfill 不该报错, got %v", err)
	}
	if ex.callCount() != 0 {
		t.Errorf("限速桶应在 Klines 调用前拦下, 调用次数 = %d, want 0", ex.callCount())
	}
}

// TestBackfillRange_UsesExplicitStartAndEndNotWaterline 锚定 BackfillRange 与
// Backfill 的核心区别：起点/止点必须原样取自调用方传入的 start/end，完全不查
// MaxOpenTime——即便库里已有更新的历史（mockKlineRepo.maxOpenTime 故意设成
// 与传入 start 不同的值），也不能被水位线悄悄接管。
func TestBackfillRange_UsesExplicitStartAndEndNotWaterline(t *testing.T) {
	klines := &mockKlineRepo{maxOpenTime: 999999999999, has: true} // 故意设置一个与 start 不同的水位线
	ex := &mockExchange{}
	svc := New(Config{}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.BackfillRange(context.Background(), "m", sub, 1000, 2000); err != nil {
		t.Fatal(err)
	}
	if ex.gotStart != 1000 {
		t.Errorf("起点 = %d, want 显式传入的 1000（不查 MaxOpenTime）", ex.gotStart)
	}
	if ex.gotEnd != 2000 {
		t.Errorf("止点 = %d, want 显式传入的 2000", ex.gotEnd)
	}
}

// TestBackfillRange_PagesUntilNextIsZero 锚定翻页行为与 Backfill 共用同一套
// 落库/续接逻辑：沿 Klines 返回的下一页起点续接直到 0，且 Source 标记为补洞
// 回填。
func TestBackfillRange_PagesUntilNextIsZero(t *testing.T) {
	klines := newMockKlineRepo()
	ex := &mockExchange{
		klinesFn: func(s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
			if start == 1000 {
				return []exchange.Kline{{NativeSymbol: "BTCUSDT", Interval: "1m", OpenTime: 1000}}, 2000, nil
			}
			return []exchange.Kline{{NativeSymbol: "BTCUSDT", Interval: "1m", OpenTime: 2000}}, 0, nil
		},
	}
	svc := New(Config{}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.BackfillRange(context.Background(), "m", sub, 1000, 3000); err != nil {
		t.Fatal(err)
	}

	if ex.callCount() != 2 {
		t.Fatalf("应翻两页, Klines 调用次数 = %d, want 2", ex.callCount())
	}
	var total []model.Kline
	for _, c := range klines.snapshot() {
		total = append(total, c...)
	}
	if len(total) != 2 {
		t.Fatalf("两页各一条，应写入 2 条, got %d", len(total))
	}
	for _, k := range total {
		if k.Source != model.KlineSourceBackfill {
			t.Errorf("Source = %d, want model.KlineSourceBackfill(%d)", k.Source, model.KlineSourceBackfill)
		}
	}
}

// TestBackfillRange_ReturnsErrorForNonKlineStreamType 锚定：BackfillRange 一次
// 只处理一条订阅，没有「其余订阅继续」的批量语义，误传 ticker/depth 必须
// 直接报错，不能像 Backfill 批量场景里那样静默跳过。
func TestBackfillRange_ReturnsErrorForNonKlineStreamType(t *testing.T) {
	klines := &mockKlineRepo{}
	ex := &mockExchange{}
	svc := New(Config{}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamTicker}
	if err := svc.BackfillRange(context.Background(), "m", sub, 1000, 2000); err == nil {
		t.Fatal("StreamType 非 kline 应报错，got nil")
	}
	if ex.callCount() != 0 {
		t.Errorf("不该调用 Klines, 调用次数 = %d, want 0", ex.callCount())
	}
}

// TestBackfillRange_ReturnsErrorWhenExchangeUnconfigured 锚定装配错误：ex 未
// 出现在 Deps.Exchanges 里应直接报错。
func TestBackfillRange_ReturnsErrorWhenExchangeUnconfigured(t *testing.T) {
	svc := New(Config{}, Deps{
		Exchanges: map[string]exchange.Exchange{},
		Limits:    map[string]*ratelimit.Bucket{},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.BackfillRange(context.Background(), "missing", sub, 1000, 2000); err == nil {
		t.Fatal("交易所未配置应报错，got nil")
	}
}

// TestBackfillRange_ReturnsErrorWhenLimiterUnconfigured 锚定装配错误：ex 在
// Exchanges 里但 Limits 缺失应直接报错，而不是空指针 panic。
func TestBackfillRange_ReturnsErrorWhenLimiterUnconfigured(t *testing.T) {
	ex := &mockExchange{}
	svc := New(Config{}, Deps{
		Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits:    map[string]*ratelimit.Bucket{},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.BackfillRange(context.Background(), "m", sub, 1000, 2000); err == nil {
		t.Fatal("限速桶未配置应报错，got nil")
	}
}

// TestBackfillRange_ReturnsErrorWhenKlinesFails 锚定评审 Important 1：REST
// 拉取失败时 BackfillRange 必须返回非 nil error，不能像旧版本那样恒定
// return nil——那样会让 backfill 子命令在下游全程报错时依然打印退出码 0，
// 操作者据此误判「洞已经补上了」。
func TestBackfillRange_ReturnsErrorWhenKlinesFails(t *testing.T) {
	klines := &mockKlineRepo{}
	ex := &mockExchange{klinesFn: func(s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
		return nil, 0, errMockKlines
	}}
	svc := New(Config{}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.BackfillRange(context.Background(), "m", sub, 1000, 2000); err == nil {
		t.Fatal("Klines 报错时 BackfillRange 应返回非 nil error，got nil")
	}
}

// TestBackfillRange_ReturnsErrorWhenUpsertFails 锚定同一条约束的另一个失败点：
// 写库失败同样必须原样上抛，不能被 pullAndUpsert 内部的 Warn 悄悄吞掉。
func TestBackfillRange_ReturnsErrorWhenUpsertFails(t *testing.T) {
	klines := &mockKlineRepo{failFirst: 1}
	ex := &mockExchange{klinesFn: func(s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
		return []exchange.Kline{{NativeSymbol: s.NativeSymbol, Interval: s.Interval, OpenTime: start}}, 0, nil
	}}
	svc := New(Config{}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.BackfillRange(context.Background(), "m", sub, 1000, 2000); err == nil {
		t.Fatal("写库失败时 BackfillRange 应返回非 nil error，got nil")
	}
}

// TestBackfillRange_ReturnsErrorWhenRateLimitWaitFails 锚定第三个失败点：
// 限速等待失败（ctx 预先取消）同样必须原样上抛。
func TestBackfillRange_ReturnsErrorWhenRateLimitWaitFails(t *testing.T) {
	klines := &mockKlineRepo{}
	ex := &mockExchange{}
	svc := New(Config{}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.BackfillRange(ctx, "m", sub, 1000, 2000); err == nil {
		t.Fatal("限速等待失败时 BackfillRange 应返回非 nil error，got nil")
	}
	if ex.callCount() != 0 {
		t.Errorf("限速桶应在 Klines 调用前拦下, 调用次数 = %d, want 0", ex.callCount())
	}
}

// TestOnReady_PropagatesCtxToBackfill 回归锚点（必修 2）：OnReady(ex) 返回的
// 闭包必须把收到的 ctx 原样转手给 Backfill，不能自己换成
// context.Background()——否则连接 ctx 取消（Manager.Stop/Rebuild）时，
// 挂在这个回调上的补洞不会被打断，会吃光 pkg/app.StopTimeout 这份全部组件
// 共享的停机预算（细则见 stream/conn.go OnReady 类型注释）。用一个预先取消
// 的 ctx 调用这个闭包：限速桶 Wait(ctx) 应立即失败，Klines 一次都不该被
// 调用——如果闭包内部偷偷换成了 context.Background()，限速等待不会失败，
// Klines 就会被调用，与 TestBackfill_WaitsOnRateLimitBucket 同一个判据。
func TestOnReady_PropagatesCtxToBackfill(t *testing.T) {
	klines := &mockKlineRepo{}
	ex := &mockExchange{}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	svc.OnReady("m")(ctx, []exchange.Sub{sub})

	if ex.callCount() != 0 {
		t.Errorf("ctx 若被正确传递，限速桶应在 Klines 调用前拦下, 调用次数 = %d, want 0", ex.callCount())
	}
}

// TestBackfill_RespectsConcurrencyLimit 锚定并发度取 Config.BackfillConcurrency：
// 多条订阅同时补洞时，同时在跑的 Klines 调用数不能超过配置值。
func TestBackfill_RespectsConcurrencyLimit(t *testing.T) {
	const concurrency = 2
	const subCount = 6

	var mu sync.Mutex
	inFlight, peak := 0, 0
	ex := &mockExchange{
		klinesFn: func(s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			time.Sleep(20 * time.Millisecond) // 制造并发窗口，放大越界即可观察到的概率

			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil, 0, nil
		},
	}
	klines := &mockKlineRepo{}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour, BackfillConcurrency: concurrency}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	subs := make([]exchange.Sub, subCount)
	for i := range subs {
		subs[i] = exchange.Sub{Market: "spot", NativeSymbol: "SYM", StreamType: exchange.StreamKline, Interval: "1m"}
	}

	if err := svc.Backfill(context.Background(), "m", subs); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > concurrency {
		t.Errorf("同时在跑的 Klines 调用数峰值 = %d, want <= %d(Config.BackfillConcurrency)", peak, concurrency)
	}
}
