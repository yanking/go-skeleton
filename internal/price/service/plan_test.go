package service

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
)

// warnRecorder 是一个极简 slog.Handler，只记录"是否出现过 Warn 级别记录"，
// 供 TestPlans_WarnsWhenNoEnabledSubscriptionsAtAll 断言用；不关心具体消息
// 内容与属性，比 stream 包的 attemptRecorder 更轻量，本包目前只需要这一个
// 布尔判据。
type warnRecorder struct {
	sawWarn bool
}

func (r *warnRecorder) Enabled(_ context.Context, level slog.Level) bool { return true }

func (r *warnRecorder) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level == slog.LevelWarn {
		r.sawWarn = true
	}
	return nil
}

func (r *warnRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *warnRecorder) WithGroup(string) slog.Handler { return r }

// mockSubscriptionRepo 是 SubscriptionRepo 的测试替身：Plans() 直接返回预设
// 的 rows/err，不触碰真实 DB。
type mockSubscriptionRepo struct {
	rows []model.Subscription
	err  error
}

func (r *mockSubscriptionRepo) ListEnabled(ctx context.Context) ([]model.Subscription, error) {
	return r.rows, r.err
}

// planStubExchange 是 exchange.Exchange 的测试替身，只用于 Plans() 相关用例：
// 与 backfill_test.go 的 mockExchange 不同，这里需要按用例配置不同的 Plan
// 行为（正常产出/报错），不复用 mockExchange 是为了不影响它已有的固定行为
// （其 Plan 恒定返回 (nil, nil)，被其它文件的用例依赖）。
type planStubExchange struct {
	planFn func(subs []exchange.Sub) ([]exchange.ConnPlan, error)
}

func (e *planStubExchange) Name() string { return "stub" }

func (e *planStubExchange) Plan(subs []exchange.Sub) ([]exchange.ConnPlan, error) {
	if e.planFn != nil {
		return e.planFn(subs)
	}
	return nil, nil
}

func (e *planStubExchange) Decode(raw []byte) (exchange.Event, error) { return exchange.Event{}, nil }

func (e *planStubExchange) Instruments(ctx context.Context, market string) ([]exchange.Instrument, error) {
	return nil, nil
}

func (e *planStubExchange) Klines(ctx context.Context, s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
	return nil, 0, nil
}

// TestPlans_ReturnsPlansForExchangeWithSubscriptions 验证有订阅的交易所能
// 正常拿到 Plan() 产出的连接计划。
func TestPlans_ReturnsPlansForExchangeWithSubscriptions(t *testing.T) {
	wantPlans := []exchange.ConnPlan{{URL: "ws://x"}}
	ex := &planStubExchange{planFn: func(subs []exchange.Sub) ([]exchange.ConnPlan, error) {
		if len(subs) != 1 {
			t.Fatalf("传给 Plan 的订阅数 = %d, want 1", len(subs))
		}
		return wantPlans, nil
	}}
	subs := &mockSubscriptionRepo{rows: []model.Subscription{
		{Exchange: "binance", Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m", Enabled: true},
	}}
	svc := New(Config{}, Deps{
		Subs:      subs,
		Exchanges: map[string]exchange.Exchange{"binance": ex},
	}, testLogger())

	got, err := svc.Plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plans, ok := got["binance"]
	if !ok {
		t.Fatal(`结果里缺 "binance" 键`)
	}
	if len(plans) != 1 || plans[0].URL != "ws://x" {
		t.Errorf("plans = %+v, want %+v", plans, wantPlans)
	}
}

// TestPlans_ReturnsEmptySliceNotMissingKeyForExchangeWithNoSubscriptions 锚定
// 必修 3 的核心行为：已装配（在 Deps.Exchanges 里）但这一轮零订阅的交易所，
// 必须在返回值里存在一个空切片，而不是彻底缺键——否则 job/reload.go 的
// for range 遍历不到它，该交易所在管的 ws 连接永远不会被 Rebuild 停掉。
func TestPlans_ReturnsEmptySliceNotMissingKeyForExchangeWithNoSubscriptions(t *testing.T) {
	ex := &planStubExchange{}
	subs := &mockSubscriptionRepo{} // 订阅表里没有任何行(如运维把 okx 全部订阅置 enabled=false)
	svc := New(Config{}, Deps{
		Subs:      subs,
		Exchanges: map[string]exchange.Exchange{"okx": ex},
	}, testLogger())

	got, err := svc.Plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plans, ok := got["okx"]
	if !ok {
		t.Fatal(`零订阅的已装配交易所应仍在结果里存在一个键，实际缺键`)
	}
	if len(plans) != 0 {
		t.Errorf("plans = %+v, want 空切片", plans)
	}
}

// TestPlans_DeletesExchangeFromResultWhenPlanFails 锚定必修 3 的另一半：Plan
// 报错时该交易所必须从结果里删除（不是留一个空切片）——留空切片会被
// reloadOnce 误读成"这一轮应该清空"，把一次协议层面的报错放大成"这家交易所
// 全停"，比什么都不做更糟。
func TestPlans_DeletesExchangeFromResultWhenPlanFails(t *testing.T) {
	ex := &planStubExchange{planFn: func(subs []exchange.Sub) ([]exchange.ConnPlan, error) {
		return nil, errors.New("mock plan 失败")
	}}
	subs := &mockSubscriptionRepo{rows: []model.Subscription{
		{Exchange: "binance", Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m", Enabled: true},
	}}
	svc := New(Config{}, Deps{
		Subs:      subs,
		Exchanges: map[string]exchange.Exchange{"binance": ex},
	}, testLogger())

	got, err := svc.Plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["binance"]; ok {
		t.Error(`Plan 报错的交易所应从结果里删除，实际仍留有一个键(哪怕值是空切片)`)
	}
}

// TestPlans_MixesOKAndFailedAndIdleExchanges 综合场景：同一次调用里三家
// 交易所分别处于「有订阅且 Plan 成功」「零订阅」「Plan 报错」三种状态，互不
// 干扰——一家的失败或空闲不该影响其它两家的结果。
func TestPlans_MixesOKAndFailedAndIdleExchanges(t *testing.T) {
	ok := &planStubExchange{planFn: func(subs []exchange.Sub) ([]exchange.ConnPlan, error) {
		return []exchange.ConnPlan{{URL: "ws://ok"}}, nil
	}}
	idle := &planStubExchange{}
	failing := &planStubExchange{planFn: func(subs []exchange.Sub) ([]exchange.ConnPlan, error) {
		return nil, errors.New("mock plan 失败")
	}}
	subs := &mockSubscriptionRepo{rows: []model.Subscription{
		{Exchange: "ok", Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m", Enabled: true},
		{Exchange: "failing", Market: "spot", NativeSymbol: "ETHUSDT", StreamType: exchange.StreamKline, Interval: "1m", Enabled: true},
	}}
	svc := New(Config{}, Deps{
		Subs:      subs,
		Exchanges: map[string]exchange.Exchange{"ok": ok, "idle": idle, "failing": failing},
	}, testLogger())

	got, err := svc.Plans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plans, exist := got["ok"]; !exist || len(plans) != 1 {
		t.Errorf(`got["ok"] = %+v, exist=%v, want 1 条 plan`, plans, exist)
	}
	if plans, exist := got["idle"]; !exist || len(plans) != 0 {
		t.Errorf(`got["idle"] = %+v, exist=%v, want 空切片且键存在`, plans, exist)
	}
	if _, exist := got["failing"]; exist {
		t.Error(`got["failing"] 应被删除，实际仍存在`)
	}
}

// TestPlans_WarnsWhenNoEnabledSubscriptionsAtAll 锚定必修 3 补的那条 Warn：
// 订阅表里没有任何启用中的行时（全新部署、或全部订阅都被关停），必须留一条
// 醒目日志——否则 daemon 一根 K 线不采却看起来一切正常。
func TestPlans_WarnsWhenNoEnabledSubscriptionsAtAll(t *testing.T) {
	rec := &warnRecorder{}
	subs := &mockSubscriptionRepo{}
	svc := New(Config{}, Deps{
		Subs:      subs,
		Exchanges: map[string]exchange.Exchange{},
	}, slog.New(rec))

	if _, err := svc.Plans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rec.sawWarn {
		t.Error("订阅表为空时应记一条 Warn，实际未记")
	}
}
