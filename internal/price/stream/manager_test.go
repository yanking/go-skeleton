package stream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// newHangingServer 起一个假 ws 服务端：连上后一直挂着不主动断开，用于观察
// Conn 是否被 Manager 真正取消，而不是自然掉线后又自愈重连回来。
func newHangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		c.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestManager_RebuildStopsOldConns 回归锚点：订阅集变更触发 Rebuild 时，旧连接
// 必须真的停下来（其 ctx 被取消）。不这么做的话，重建前后两条连接会同时收同一个
// 流，事件重复入库——这条比新连接连没连上更容易被漏掉，故单独测。
//
// 两次 Rebuild 用的 plans 必须是真的不同（这里用不同的 Subs 区分），否则会
// 被「plans 未变就跳过」这条短路命中——那是另一条行为（见
// TestManager_RebuildIsNoopWhenPlansUnchanged），这里测的是订阅集确实变了
// 的情形。
func TestManager_RebuildStopsOldConns(t *testing.T) {
	srv := newHangingServer(t)

	ready := make(chan struct{}, 1)
	m := NewManager(
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	planV1 := exchange.ConnPlan{URL: url, Subs: []exchange.Sub{{Market: "binance", NativeSymbol: "BTCUSDT"}}}
	if err := m.Rebuild(context.Background(), []exchange.ConnPlan{planV1}); err != nil {
		t.Fatalf("首次 Rebuild 不应失败: %v", err)
	}

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("首次 Rebuild 后未连上")
	}

	m.mu.Lock()
	if len(m.conns) != 1 {
		m.mu.Unlock()
		t.Fatalf("首次 Rebuild 后应有 1 条连接, got %d", len(m.conns))
	}
	oldCtx := m.conns[0].ctx
	m.mu.Unlock()

	if oldCtx.Err() != nil {
		t.Fatal("旧连接的 ctx 在 Rebuild 前不应被取消")
	}

	// 订阅集真的变了：再 Rebuild 一次，旧连接必须被停掉。
	planV2 := exchange.ConnPlan{URL: url, Subs: []exchange.Sub{{Market: "binance", NativeSymbol: "ETHUSDT"}}}
	if err := m.Rebuild(context.Background(), []exchange.ConnPlan{planV2}); err != nil {
		t.Fatalf("第二次 Rebuild 不应失败: %v", err)
	}

	if oldCtx.Err() == nil {
		t.Error("Rebuild 后旧连接的 ctx 应已取消，实际未取消")
	}
}

// TestManager_RebuildIsNoopWhenPlansUnchanged 回归锚点：上层（如按
// reload_interval 周期调用 Rebuild 的重载任务）会拿同样的订阅集反复调用
// Rebuild——plans 没变时必须整体跳过，否则每个周期都要全量断链重连、触发
// 全部 OnReady，变成周期性的全交易所断流 + 补洞风暴。判断责任放在持有当前
// plans 的 Manager 里，不指望调用方自己 diff。
func TestManager_RebuildIsNoopWhenPlansUnchanged(t *testing.T) {
	srv := newHangingServer(t)

	ready := make(chan struct{}, 1)
	m := NewManager(
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)

	plan := exchange.ConnPlan{URL: "ws" + strings.TrimPrefix(srv.URL, "http")}
	if err := m.Rebuild(context.Background(), []exchange.ConnPlan{plan}); err != nil {
		t.Fatalf("首次 Rebuild 不应失败: %v", err)
	}

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("首次 Rebuild 后未连上")
	}

	m.mu.Lock()
	if len(m.conns) != 1 {
		m.mu.Unlock()
		t.Fatalf("首次 Rebuild 后应有 1 条连接, got %d", len(m.conns))
	}
	firstCtx := m.conns[0].ctx
	m.mu.Unlock()

	// 同样的 plans（值相同，独立的切片）再 Rebuild 一次：应整体跳过。
	if err := m.Rebuild(context.Background(), []exchange.ConnPlan{plan}); err != nil {
		t.Fatalf("plans 未变的 Rebuild 不应失败: %v", err)
	}

	m.mu.Lock()
	sameConn := len(m.conns) == 1 && m.conns[0].ctx == firstCtx
	m.mu.Unlock()

	if !sameConn {
		t.Error("plans 未变时 Rebuild 应保留原连接，实际重建了")
	}
	if firstCtx.Err() != nil {
		t.Error("plans 未变时 Rebuild 不应取消旧连接")
	}
}

// TestManager_StopRespectsCtx 回归锚点：app.Component 的约定是「Stop 须在
// ctx 到期前返回」，且这个宽限期是全部组件共享的。这里手工构造一条「赖着不
// 退出」的连接——cancel 被调用了，但 done 永不关闭（对应现实里被下游背压卡住
// 的 handle，例如「kline 队列满即阻塞上游」导致 Run 一直不返回）——断言 Stop
// 在 ctx 到期时放弃等待并返回 error，而不是拖累后面组件的停机预算。
func TestManager_StopRespectsCtx(t *testing.T) {
	m := NewManager(
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {}, func(context.Context, []exchange.Sub) {},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)
	stuck := managedConn{
		ctx:    context.Background(),
		cancel: func() {},
		done:   make(chan struct{}), // 永不关闭，模拟卡住的连接
	}
	m.mu.Lock()
	m.conns = []managedConn{stuck}
	m.mu.Unlock()

	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Stop(stopCtx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("有连接卡住时 Stop 应返回 error，实际返回 nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 在 ctx 到期后仍未返回——违反 app.Component 的「须在 ctx 到期前返回」约定")
	}
}

// TestManager_RebuildReturnsErrorWithoutStartingNewConnsWhenOldConnStuck
// 回归锚点：旧连接的 handle 回调被下游背压永久卡住时（对应「kline 队列满即
// 阻塞上游，不可丢」的真实语义）——readLoop 收到帧后同步调 handle，handle 不
// 返回，即便 ctx 已被 cancel()，Run 的 goroutine 也走不到下一次 ctx 检查，
// 卡死不退出。Rebuild 全程持有 m.mu，若像 Stop 修复前那样无限期等待，会连累
// 后续所有调用（包括 Stop）一起卡在拿锁上。用一个很短超时的 ctx 调用 Rebuild，
// 必须在 ctx 到期后返回 error 而不是永久挂住，并且不能在没停干净旧连接的情况
// 下起新连接——那样新旧两条连接会同时收同一个流，事件重复入库。
func TestManager_RebuildReturnsErrorWithoutStartingNewConnsWhenOldConnStuck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"stuck":1}`))
		<-r.Context().Done()
	}))
	defer srv.Close()

	handleEntered := make(chan struct{}, 1)
	blockHandle := make(chan struct{})
	t.Cleanup(func() { close(blockHandle) }) // 放行卡住的 handle，避免 goroutine 残留到进程退出

	ready := make(chan struct{}, 1)
	m := NewManager(
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {
			select {
			case handleEntered <- struct{}{}:
			default:
			}
			<-blockHandle // 模拟下游背压永久卡住
		},
		func(context.Context, []exchange.Sub) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	planV1 := exchange.ConnPlan{URL: url, Subs: []exchange.Sub{{Market: "binance"}}}
	if err := m.Rebuild(context.Background(), []exchange.ConnPlan{planV1}); err != nil {
		t.Fatalf("首次 Rebuild 不应失败: %v", err)
	}

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("首次 Rebuild 后未连上")
	}
	select {
	case <-handleEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("未观察到 handle 被调用——旧连接没有真的卡住，测试场景没搭起来")
	}

	m.mu.Lock()
	if len(m.conns) != 1 {
		m.mu.Unlock()
		t.Fatalf("首次 Rebuild 后应有 1 条连接, got %d", len(m.conns))
	}
	oldCtx := m.conns[0].ctx
	m.mu.Unlock()

	// 订阅集变了，触发重建；但旧连接卡住退不出来，给一个很短的超时。
	// Rebuild 调用包一层 goroutine+超时 select：修复回归成无限期等待时，
	// 这里能在 3 秒内就把它当失败报出来，而不是让整条 go test 命令挂到
	// 默认的 10 分钟超时才失败。
	planV2 := exchange.ConnPlan{URL: url, Subs: []exchange.Sub{{Market: "okx"}}}
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	rebuildErr := make(chan error, 1)
	go func() { rebuildErr <- m.Rebuild(stopCtx, []exchange.ConnPlan{planV2}) }()

	var err error
	select {
	case err = <-rebuildErr:
	case <-time.After(3 * time.Second):
		t.Fatal("Rebuild 在 ctx 到期后仍未返回——旧连接卡住时不该无限期持锁等待")
	}
	if err == nil {
		t.Error("旧连接卡住时 Rebuild 应返回 error，实际返回 nil")
	}

	// 比 ctx 的同一性，不只比数量：如果回归成"stopAllLocked 报错也照样起
	// 新连接"，conns 数量同样是 1，只比数量的话这条测试会假通过。
	m.mu.Lock()
	sameConn := len(m.conns) == 1 && m.conns[0].ctx == oldCtx
	m.mu.Unlock()
	if !sameConn {
		t.Error("Rebuild 超时不该起新连接，应保留原来那条（同一个 ctx 实例），实际变了")
	}
}

// TestManager_RebuildAfterTimeoutDoesNotShortCircuitOnSamePlans 回归锚点：
// Rebuild 超时后，m.plans 不能停留在旧值。完整故障序列：① Rebuild(bg, P1)
// 成功；② 订阅集变成 P2，Rebuild(ctx, P2) 把 P1 的连接全部 cancel，某条被
// 背压卡住导致超时——此时 m.plans 若还停留在 P1，而 C1 里没卡住的连接已经
// 退出，采集已经中断；③ 订阅集之后"抖回" P1（标的短暂下架又上架、
// Exchange.Plan() 输出顺序变化等），Rebuild(ctx, P1) 若被短路命中直接跳过，
// Manager 会自认为在跑 P1，实际一条连接都没有——这家交易所的采集永久静默
// 停摆，只留一条几分钟前的 error 日志。本测试断言第③步真的重建了连接，
// 而不是被短路跳过。
func TestManager_RebuildAfterTimeoutDoesNotShortCircuitOnSamePlans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"stuck":1}`))
		<-r.Context().Done()
	}))
	defer srv.Close()

	handleEntered := make(chan struct{}, 1)
	blockHandle := make(chan struct{})

	ready := make(chan struct{}, 8)
	m := NewManager(
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {
			select {
			case handleEntered <- struct{}{}:
			default:
			}
			<-blockHandle // 模拟下游背压永久卡住
		},
		func(context.Context, []exchange.Sub) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	planV1 := exchange.ConnPlan{URL: url, Subs: []exchange.Sub{{Market: "binance"}}}
	// 步骤①：首次 Rebuild 成功。
	if err := m.Rebuild(context.Background(), []exchange.ConnPlan{planV1}); err != nil {
		t.Fatalf("首次 Rebuild 不应失败: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("首次 Rebuild 后未连上")
	}
	select {
	case <-handleEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("未观察到 handle 被调用——旧连接没有真的卡住，测试场景没搭起来")
	}

	m.mu.Lock()
	stuckCtx := m.conns[0].ctx
	m.mu.Unlock()

	// 步骤②：订阅集变成 P2，触发重建；旧连接卡住退不出来，制造超时失败。
	planV2 := exchange.ConnPlan{URL: url, Subs: []exchange.Sub{{Market: "okx"}}}
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	rebuildErr := make(chan error, 1)
	go func() { rebuildErr <- m.Rebuild(stopCtx, []exchange.ConnPlan{planV2}) }()
	select {
	case err := <-rebuildErr:
		if err == nil {
			t.Fatal("旧连接卡住时 Rebuild 应返回 error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Rebuild 未在超时后返回")
	}

	// 放行卡住的 handle，让被 cancel 过的旧连接真正退出——不这么做的话，
	// 下面用 context.Background() 重试会在它的 done channel 上永久等待。
	close(blockHandle)

	// 步骤③：订阅集"抖回" P1（值与首次调用完全一样）。这次必须真的重建，
	// 而不是被短路命中——同样包一层超时兜底，回归成挂住时不拖累整个测试。
	rebuildErr2 := make(chan error, 1)
	go func() { rebuildErr2 <- m.Rebuild(context.Background(), []exchange.ConnPlan{planV1}) }()
	select {
	case err := <-rebuildErr2:
		if err != nil {
			t.Fatalf("超时后用原 plans 重试不应失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("用原 plans 重试的 Rebuild 未在合理时间内返回——怀疑被短路命中后又卡在别处，或压根没重建")
	}

	// 比 ctx 的同一性，不只比数量：如果 Rebuild 被短路命中直接返回，
	// m.conns 仍然引用着步骤②里那条已经 cancel 过的旧连接——数量同样是 1，
	// 只比数量的话这条测试会假通过（这正是它要防的那类 bug 本身也可能踩中
	// 的坑）。
	m.mu.Lock()
	rebuilt := len(m.conns) == 1 && m.conns[0].ctx != stuckCtx
	m.mu.Unlock()
	if !rebuilt {
		t.Error("超时后用原 plans 重试应重建出一条全新连接（不该被短路跳过、沿用已经 cancel 过的旧连接）")
	}
}

// TestManager_RebuildShortCircuitsRegardlessOfPlansOrder 回归锚点：同一个
// 逻辑订阅集，两次调用里 []ConnPlan 的顺序、以及每条 ConnPlan 内 Subs 的顺序
// 都不一样（对应上游查询没写 ORDER BY，或者 Exchange.Plan() 内部用 map 分组
// ——Go 的 map 迭代顺序每次随机）——短路逻辑必须认出这是同一个逻辑订阅集，
// 不该被顺序抖动骗过去重建，否则周期性重载会退化回每次都全量断流 + 补洞
// 风暴，且恰好是 TestManager_RebuildAfterTimeoutDoesNotShortCircuitOnSamePlans
// 那个故障序列的点火源。
func TestManager_RebuildShortCircuitsRegardlessOfPlansOrder(t *testing.T) {
	srv := newHangingServer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ready := make(chan struct{}, 8)
	m := NewManager(
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {
			select {
			case ready <- struct{}{}:
			default:
			}
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)

	subA := exchange.Sub{Market: "binance", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	subB := exchange.Sub{Market: "binance", NativeSymbol: "ETHUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	planX := exchange.ConnPlan{URL: url, Subs: []exchange.Sub{subA, subB}}
	planY := exchange.ConnPlan{URL: url + "?y", Subs: []exchange.Sub{subB}}

	if err := m.Rebuild(context.Background(), []exchange.ConnPlan{planX, planY}); err != nil {
		t.Fatalf("首次 Rebuild 不应失败: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("首次 Rebuild 后未连上")
	}

	m.mu.Lock()
	if len(m.conns) != 2 {
		m.mu.Unlock()
		t.Fatalf("首次 Rebuild 后应有 2 条连接, got %d", len(m.conns))
	}
	firstCtxA, firstCtxB := m.conns[0].ctx, m.conns[1].ctx
	m.mu.Unlock()

	// 外层顺序倒过来 [planY, planX]，且 planX 内部 Subs 也倒过来 [subB, subA]
	// ——值完全一样，只是顺序不同。
	planXReordered := exchange.ConnPlan{URL: planX.URL, Subs: []exchange.Sub{subB, subA}}
	if err := m.Rebuild(context.Background(), []exchange.ConnPlan{planY, planXReordered}); err != nil {
		t.Fatalf("plans 顺序不同但内容相同的 Rebuild 不应失败: %v", err)
	}

	m.mu.Lock()
	sameConns := len(m.conns) == 2 &&
		((m.conns[0].ctx == firstCtxA && m.conns[1].ctx == firstCtxB) ||
			(m.conns[0].ctx == firstCtxB && m.conns[1].ctx == firstCtxA))
	m.mu.Unlock()

	if !sameConns {
		t.Error("顺序不同但内容相同的 plans 应被短路命中，实际重建了连接")
	}
}
