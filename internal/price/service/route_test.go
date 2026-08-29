package service

import (
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// TestRoute_SnapshotQueueDropsOldestWhenFull 锚定 snapshot 队列的背压语义：
// 满了就丢最旧的，因为 ticker/depth 是快照，只有最新一帧有意义。
func TestRoute_SnapshotQueueDropsOldestWhenFull(t *testing.T) {
	svc := New(Config{SnapshotQueueSize: 2, KlineQueueSize: 8}, Deps{}, testLogger())
	route := svc.RouteFor("binance")
	for i := 0; i < 5; i++ {
		route(exchange.Event{Snapshot: &exchange.Snapshot{
			NativeSymbol: "BTCUSDT", StreamType: exchange.StreamDepth, EventTime: int64(i)}})
	}
	// 队列只留最新两帧：弃旧而不是弃新——旧快照没有价值。
	got := svc.drainSnapshotsForTest()
	if len(got) != 2 || got[0].EventTime != 3 || got[1].EventTime != 4 {
		t.Errorf("留下的快照 = %+v, want EventTime 3、4", got)
	}
}

// TestRoute_KlineQueueNeverDrops 锚定 kline 队列的背压语义：满了必须阻塞
// 上游，绝不能丢——收线帧丢一帧就是一个洞，只能靠 REST 补洞找回。
func TestRoute_KlineQueueNeverDrops(t *testing.T) {
	svc := New(Config{SnapshotQueueSize: 1, KlineQueueSize: 3}, Deps{}, testLogger())
	route := svc.RouteFor("binance")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 4; i++ { // 第 4 条必然阻塞，直到有人消费
			route(exchange.Event{Kline: &exchange.Kline{OpenTime: int64(i)}})
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("kline 队列满时 Route 直接返回了——收线帧被丢弃，会在 K 线里留洞")
	case <-time.After(100 * time.Millisecond): // 阻塞住才是对的
	}
	svc.drainKlinesForTest() // 放水后应能收尾
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("消费后 Route 仍未返回")
	}
}

// TestRouteFor_FillsExchangeWithoutCrossContamination 锚定 RouteFor 存在的
// 唯一理由：exchange.Event 本身不携带交易所身份（两个 adapter 包的 Market
// 字段填的是子市场常量 "spot"，不是交易所名），落库的 model.Kline.Exchange
// 必须来自 RouteFor 的入参，且不同交易所各自绑定的闭包互不串——不能只靠肉眼
// 看代码，得有测试守住这条。
func TestRouteFor_FillsExchangeWithoutCrossContamination(t *testing.T) {
	svc := New(Config{KlineQueueSize: 8, SnapshotQueueSize: 1}, Deps{}, testLogger())

	svc.RouteFor("binance")(exchange.Event{Kline: &exchange.Kline{NativeSymbol: "BTCUSDT", OpenTime: 1}})
	svc.RouteFor("okx")(exchange.Event{Kline: &exchange.Kline{NativeSymbol: "BTC-USDT", OpenTime: 2}})

	got := svc.drainKlinesForTest()
	if len(got) != 2 {
		t.Fatalf("落队 K 线条数 = %d, want 2", len(got))
	}
	if got[0].Exchange != "binance" {
		t.Errorf("第一条 Exchange = %q, want %q", got[0].Exchange, "binance")
	}
	if got[1].Exchange != "okx" {
		t.Errorf("第二条 Exchange = %q, want %q", got[1].Exchange, "okx")
	}
}

// TestNew_ZeroQueueSizesTakeDefaults 锚定 Minor 修复：Config.KlineQueueSize/
// SnapshotQueueSize 零值必须兜底到 defaultKlineQueueSize/
// defaultSnapshotQueueSize（与 configs/price.yaml 字段注释「零值取
// 1024/256」一致），不能退化成无缓冲 channel——之前 New 对这两个字段没有
// 任何兜底，是纸面承诺（评审 Minor）。
func TestNew_ZeroQueueSizesTakeDefaults(t *testing.T) {
	svc := New(Config{}, Deps{}, testLogger())

	if got := cap(svc.klineCh); got != defaultKlineQueueSize {
		t.Errorf("klineCh 容量 = %d, want defaultKlineQueueSize(%d)", got, defaultKlineQueueSize)
	}
	if got := cap(svc.snapCh); got != defaultSnapshotQueueSize {
		t.Errorf("snapCh 容量 = %d, want defaultSnapshotQueueSize(%d)", got, defaultSnapshotQueueSize)
	}
}

// TestNew_ConfiguredQueueSizesWin 锚定显式配置的队列容量不被默认值覆盖。
func TestNew_ConfiguredQueueSizesWin(t *testing.T) {
	svc := New(Config{KlineQueueSize: 7, SnapshotQueueSize: 3}, Deps{}, testLogger())

	if got := cap(svc.klineCh); got != 7 {
		t.Errorf("klineCh 容量 = %d, want 7", got)
	}
	if got := cap(svc.snapCh); got != 3 {
		t.Errorf("snapCh 容量 = %d, want 3", got)
	}
}
