package stream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// decoderFunc 把普通函数适配成 Decoder：测试用的手写 mock，按仓库约定不引入
// mock 框架，用具名函数类型省去为每个用例单独定义结构体。
type decoderFunc func(raw []byte) (exchange.Event, error)

// Decode 实现 Decoder。
func (f decoderFunc) Decode(raw []byte) (exchange.Event, error) { return f(raw) }

// quietLogger 返回一个丢弃全部输出的 logger：测试只关心行为断言，不想被
// 生命周期日志（连接中断、重连等）淹没输出。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestConn_ReconnectsAndFiresReadyEachTime 回归锚点：第一次连接被服务端强制
// 断开后，Conn 必须自愈重连并再次收到消息；OnReady 必须在首连与每次重连后
// 各触发一次——后续补洞任务把重放窗口挂在这个回调上，触发次数不对补洞就会漏。
func TestConn_ReconnectsAndFiresReadyEachTime(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n := conns.Add(1)
		if n == 1 {
			c.Close(websocket.StatusInternalError, "强制断开,触发重连") // 第一次立刻断
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"ok":1}`))
		<-r.Context().Done()
	}))
	defer srv.Close()

	var ready atomic.Int32
	got := make(chan struct{}, 1)
	readyCh := make(chan struct{}, 8)
	c := NewConn(
		exchange.ConnPlan{URL: "ws" + strings.TrimPrefix(srv.URL, "http")},
		decoderFunc(func(b []byte) (exchange.Event, error) {
			select {
			case got <- struct{}{}:
			default:
			}
			return exchange.Event{}, nil
		}),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {
			ready.Add(1)
			select {
			case readyCh <- struct{}{}:
			default:
			}
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("重连后未收到消息")
	}
	// OnReady 现在在独立协程里跑（不阻塞读循环启动，见 conn.go 的
	// OnReady 类型注释），与 readLoop 收到消息之间没有先后顺序保证，
	// 因此改为有超时的等待，而不是收到消息后立刻检查一次。
	for ready.Load() < 2 {
		select {
		case <-readyCh:
		case <-time.After(3 * time.Second):
			t.Fatalf("OnReady 触发 %d 次, want ≥2(首连 + 重连各一次)", ready.Load())
		}
	}
}

// attemptRecorder 记录经过它的日志记录里 "attempt" 属性的值：白盒测试用于直接
// 验证 Conn.Run 内部的退避计数。不用量墙钟等待时长断言——Backoff.Next 自带
// 随机抖动，等待时长本身不是确定值，直接读计数才是稳定、不闪烁的断言。
// notify 在每次记到新值后收一条通知，供测试用 select+超时等待，不猜固定睡眠。
type attemptRecorder struct {
	mu       sync.Mutex
	attempts []int64
	notify   chan struct{}
}

func newAttemptRecorder() *attemptRecorder {
	return &attemptRecorder{notify: make(chan struct{}, 64)}
}

// Enabled 实现 slog.Handler：全部级别都收。
func (r *attemptRecorder) Enabled(context.Context, slog.Level) bool { return true }

// Handle 实现 slog.Handler：只挑出 "attempt" 属性记下来。
func (r *attemptRecorder) Handle(_ context.Context, rec slog.Record) error {
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "attempt" {
			r.mu.Lock()
			r.attempts = append(r.attempts, a.Value.Int64())
			r.mu.Unlock()
			select {
			case r.notify <- struct{}{}:
			default:
			}
		}
		return true
	})
	return nil
}

// WithAttrs 实现 slog.Handler：测试不需要分组属性，原样返回自身。
func (r *attemptRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }

// WithGroup 实现 slog.Handler：同上。
func (r *attemptRecorder) WithGroup(string) slog.Handler { return r }

// snapshot 返回目前记到的全部 attempt 值的快照副本。
func (r *attemptRecorder) snapshot() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.attempts...)
}

// TestConn_ResetsBackoffAfterSustainedConnect 回归锚点：Binance 现货单连接
// 每天会被服务端强制断开一次触发重连——断开前连接本是健康的，不是失败的延续。
// 场景：先让若干次拨号连续失败（把退避计数推高），再成功连上一次并稳定存活
// 超过 Backoff.Min（触发 OnReady）随后断开——断开后的首次重连必须回到
// attempt=0，不能延续失败序列已经推高的计数，否则那次例行断开要白等到接近
// Max 才恢复采集，而 K 线是不可丢的数据，这段延迟只能靠 REST 补洞去填。
func TestConn_ResetsBackoffAfterSustainedConnect(t *testing.T) {
	const failCount = 3
	// 存活门槛给足margin（10x Min），避免调度抖动把「真连上」误判成「秒断」。
	backoff := Backoff{Min: 5 * time.Millisecond, Max: 500 * time.Millisecond}
	const sustainedFor = 60 * time.Millisecond

	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := conns.Add(1)
		if int(n) <= failCount {
			// 直接拒绝握手，制造真实的拨号失败（不走 websocket.Accept）。
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		if int(n) == failCount+1 {
			time.Sleep(sustainedFor) // 稳定存活一段时间，才算「真连上」
			c.Close(websocket.StatusNormalClosure, "")
			return
		}
		<-r.Context().Done() // 之后的连接稳定住，测试收尾用
	}))
	defer srv.Close()

	rec := newAttemptRecorder()
	c := NewConn(
		exchange.ConnPlan{URL: "ws" + strings.TrimPrefix(srv.URL, "http")},
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {},
		slog.New(rec), Policy{Backoff: backoff},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	// 等到「稳定连接断开」之后的那条重连日志出现：failCount 次失败各一条 +
	// 稳定连接断开后一条，共 failCount+1 条。
	for len(rec.snapshot()) <= failCount {
		select {
		case <-rec.notify:
		case <-time.After(3 * time.Second):
			t.Fatalf("重连日志数量不足, got %d, want > %d", len(rec.snapshot()), failCount)
		}
	}

	attempts := rec.snapshot()
	for i := 0; i < failCount; i++ {
		if attempts[i] != int64(i) {
			t.Fatalf("失败序列第 %d 条重连日志 attempt = %d, want %d(未重置前应逐次递增)",
				i, attempts[i], i)
		}
	}
	if got := attempts[failCount]; got != 0 {
		t.Errorf("稳定连接断开后的首次重连 attempt = %d, want 0(应清零，不能延续失败序列的计数)", got)
	}
}

// TestConn_QuickDisconnectDoesNotResetBackoff 回归锚点：握手通过又立刻关闭
// （限流、封禁、维护期常见形态：dial 成功→订阅帧写入成功→ready 触发→
// readLoop 立刻报错）不该被当作「真连上」。判据只看订阅帧 Write 是否返回成功
// 会在这种场景下误清零退避——每轮都只等 Backoff.Next(0)，退避永远不增长，
// 变成对一个拒绝服务地址的无退避快速重试，同时 OnReady 被高频触发，把
// 第 10 个任务的 REST 补洞限速打爆。
func TestConn_QuickDisconnectDoesNotResetBackoff(t *testing.T) {
	const failCount = 2
	// Min 给得比本机 accept+close 的往返延迟大得多，确保「秒断」判定稳定不闪烁。
	backoff := Backoff{Min: 80 * time.Millisecond, Max: time.Second}

	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := conns.Add(1)
		if int(n) <= failCount {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.Close(websocket.StatusNormalClosure, "") // 每次都连上就立刻断开
	}))
	defer srv.Close()

	rec := newAttemptRecorder()
	c := NewConn(
		exchange.ConnPlan{URL: "ws" + strings.TrimPrefix(srv.URL, "http")},
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {},
		slog.New(rec), Policy{Backoff: backoff},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	// 等到「秒断」发生两次之后的重连日志：failCount 条真失败 + 2 条秒断。
	const want = failCount + 2
	for len(rec.snapshot()) < want {
		select {
		case <-rec.notify:
		case <-time.After(3 * time.Second):
			t.Fatalf("重连日志数量不足, got %d, want ≥%d", len(rec.snapshot()), want)
		}
	}

	// 全部 want 条都不该清零，必须逐条从 0 递增——秒断连接混在中间也一样。
	attempts := rec.snapshot()
	for i := 0; i < want; i++ {
		if attempts[i] != int64(i) {
			t.Fatalf("第 %d 条重连日志 attempt = %d, want %d(握手成功又秒断不该清零退避)",
				i, attempts[i], i)
		}
	}
}

// TestConn_DecodeErrorDoesNotDisconnect 回归锚点：交易所会持续推送订阅确认、
// 心跳应答一类与业务无关的帧，Decoder 对它们返回 error 是正常现象，不代表
// 连接坏了。这条行为明写在需求里，此前没有用例覆盖——一旦回归成「解码出错就
// 断连」，会退化成每帧一次重连。
func TestConn_DecodeErrorDoesNotDisconnect(t *testing.T) {
	const badFrame = "不认识的帧"

	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conns.Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(badFrame))
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"ok":1}`))
		<-r.Context().Done() // 连接本身保持存活，只是中间夹了一帧解不动的
	}))
	defer srv.Close()

	got := make(chan struct{}, 1)
	c := NewConn(
		exchange.ConnPlan{URL: "ws" + strings.TrimPrefix(srv.URL, "http")},
		decoderFunc(func(b []byte) (exchange.Event, error) {
			if string(b) == badFrame {
				return exchange.Event{}, errors.New("解不动")
			}
			select {
			case got <- struct{}{}:
			default:
			}
			return exchange.Event{}, nil
		}),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("解码失败的帧之后，同一条连接上的第二帧未被处理——怀疑连接被解码错误断开了")
	}

	// 给可能存在的误重连一个观察窗口：如果解码失败错误地触发了断连重试，
	// 这里会看到 conns 增长到 2 及以上。
	time.Sleep(50 * time.Millisecond)
	if n := conns.Load(); n != 1 {
		t.Errorf("解码失败不该触发重连，观察到 %d 次连接", n)
	}
}

// TestBackoff_ZeroValueMeansNoThrottling 锚定 Critical 1 的根因：Backoff{}
// 零值本身就会产生「不带节流」的行为——重连没有任何等待。这条测试证明为什么
// Policy.resolve 必须给零值 Backoff 兜底默认，不能指望调用方总会填对
// reconnect_backoff_min/max。
func TestBackoff_ZeroValueMeansNoThrottling(t *testing.T) {
	var b Backoff
	for attempt := 0; attempt < 5; attempt++ {
		if d := b.Next(attempt); d != 0 {
			t.Fatalf("Backoff{} 零值 Next(%d) = %v, want 0（这就是热循环的根源）", attempt, d)
		}
	}
}

// TestNewConn_ResolvesZeroPolicyToSafeDefaults 回归锚点：调用方传入零值
// Policy（未配置 DialTimeout/Backoff，对应 config.Exchange 漏配
// reconnect_backoff_min/max 的现实场景）时，NewConn 构造出的 Conn 实际使用的
// 策略不能是零值——否则拨号无限期挂起、重连变成不带节流的热循环。
func TestNewConn_ResolvesZeroPolicyToSafeDefaults(t *testing.T) {
	c := NewConn(
		exchange.ConnPlan{},
		decoderFunc(func([]byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {}, func(context.Context, []exchange.Sub) {},
		quietLogger(), Policy{},
	)
	if c.policy.DialTimeout <= 0 {
		t.Errorf("NewConn 用零值 Policy 构造后 DialTimeout = %v, 应已套上安全默认", c.policy.DialTimeout)
	}
	if c.policy.Backoff.Min <= 0 || c.policy.Backoff.Max <= 0 {
		t.Errorf("NewConn 用零值 Policy 构造后 Backoff = %+v, 应已套上安全默认", c.policy.Backoff)
	}
}

// TestConn_MissingPingEveryDoesNotPanic 回归锚点：ConnPlan.ClientPing 非空但
// PingEvery 未设置（后续交易所实现容易漏配的组合，ConnPlan 对二者没有耦合
// 约束）。修复前 time.NewTicker(0) 会在 goroutine 里 panic，且是在 goroutine
// 里，调用方无法 recover，会直接打死整个 go test 进程（表现为进程崩溃而不是
// 普通的 FAIL）——本测试能跑完本身就是回归证据。
func TestConn_MissingPingEveryDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	c := NewConn(
		exchange.ConnPlan{
			URL:        "ws" + strings.TrimPrefix(srv.URL, "http"),
			ClientPing: []byte("ping"),
			PingEvery:  0, // 故意漏配
		},
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Run(ctx) }()

	// 给连接一点时间跑起来——这段时间足以触发修复前会 panic 的路径。
	// 跑到这里没有崩溃即是本测试的核心断言。
	time.Sleep(30 * time.Millisecond)
	cancel()
}

// TestConn_OnReadyDoesNotBlockPingLoop 回归锚点：coder/websocket 的 pong
// 只在读路径里发出，只有 readLoop 已经在跑 Read 才会有人应答；心跳协程与
// readLoop 都不该被同步执行的 OnReady 卡住。这里让 OnReady 模拟耗时的历史
// 补洞（sleep 一段较长时间），断言心跳帧必须在这段耗时窗口内就送达服务端——
// 如果 OnReady 是同步执行，心跳协程要等它跑完才会启动，肯定赶不上这个窗口。
func TestConn_OnReadyDoesNotBlockPingLoop(t *testing.T) {
	const onReadySleep = 200 * time.Millisecond
	const pingEvery = 10 * time.Millisecond

	pingReceived := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		if _, _, err := c.Read(r.Context()); err == nil { // 阻塞等客户端发来的第一帧（心跳帧）
			select {
			case pingReceived <- struct{}{}:
			default:
			}
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewConn(
		exchange.ConnPlan{
			URL:        "ws" + strings.TrimPrefix(srv.URL, "http"),
			ClientPing: []byte("ping"),
			PingEvery:  pingEvery,
		},
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) { time.Sleep(onReadySleep) }, // 模拟耗时补洞
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case <-pingReceived:
	case <-time.After(onReadySleep):
		t.Fatal("在 OnReady 耗时窗口内未收到心跳帧——怀疑心跳被同步执行的 OnReady 卡住了")
	}
}

// TestConn_RunWaitsForInFlightOnReadyBeforeReturning 回归锚点：OnReady 的
// 协程生命周期必须绑定在 Run 内——ctx 取消后，如果 OnReady 还没跑完，Run
// 不能提前返回。不这么做的话，Manager.Stop 会在 OnReady 仍在写库时就已经
// 成功返回，随后 app 按逆序开始关闭 DB/Redis 等基础组件，这个游离的协程会
// 撞上已经关闭的连接池。
func TestConn_RunWaitsForInFlightOnReadyBeforeReturning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	readyStarted := make(chan struct{}, 1)
	releaseReady := make(chan struct{})
	c := NewConn(
		exchange.ConnPlan{URL: "ws" + strings.TrimPrefix(srv.URL, "http")},
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {
			select {
			case readyStarted <- struct{}{}:
			default:
			}
			<-releaseReady // 卡住，模拟仍在跑的耗时补洞
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case <-readyStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("OnReady 未被触发")
	}

	cancel() // 取消 ctx——此时 OnReady 仍卡在 releaseReady 上

	select {
	case <-runDone:
		t.Fatal("Run 在 in-flight 的 OnReady 跑完之前就返回了")
	case <-time.After(50 * time.Millisecond):
		// 符合预期：ctx 已取消，但 Run 仍在等 OnReady。
	}

	close(releaseReady) // 放行 OnReady

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("OnReady 跑完后 Run 仍未返回")
	}
}

// TestConn_OnReadyCtxCancelsWithConnRunCtx 回归锚点（必修 2）：OnReady 收到的
// ctx 必须是触发它的这条连接的 Run(ctx)，不能是 context.Background() 一类
// 不会随连接生命周期变化的 ctx——否则补洞这类耗时操作不可取消，会吃光
// pkg/app.StopTimeout 这份全部组件共享的停机预算（细则见 conn.go OnReady
// 类型注释）。这里捕获 OnReady 收到的 ctx，取消连接的 Run ctx 后断言它
// 也随之进入 Done 状态。
func TestConn_OnReadyCtxCancelsWithConnRunCtx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	var gotCtx context.Context
	readyEntered := make(chan struct{}, 1)
	c := NewConn(
		exchange.ConnPlan{URL: "ws" + strings.TrimPrefix(srv.URL, "http")},
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(ctx context.Context, subs []exchange.Sub) {
			gotCtx = ctx
			select {
			case readyEntered <- struct{}{}:
			default:
			}
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	select {
	case <-readyEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("OnReady 未被触发")
	}

	if gotCtx == nil {
		t.Fatal("OnReady 未收到 ctx")
	}
	if gotCtx.Err() != nil {
		t.Fatal("连接 ctx 取消前，OnReady 收到的 ctx 就已经是 Done 状态")
	}

	cancel() // 取消这条连接的 Run ctx

	select {
	case <-gotCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("连接 ctx 取消后，OnReady 收到的 ctx 未随之取消——两者未绑定，补洞将不可取消")
	}

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未在 ctx 取消后返回")
	}
}

// TestConn_TriggerReadySkipsWhilePreviousStillRunning 回归锚点：同一条连接
// 同一时刻至多一个 OnReady 在跑——上一次触发还没结束时，新的触发必须被跳过，
// 而不是并发再起一个。不这么做的话，「握手通过即关闭」这类快速反复重连场景
// 会让协程无界累积：触发频率被 Backoff 压低不等于同时跑的补洞数量有上限，
// 耗时的补洞叠加耗时的补洞，还会反过来把 REST 限速拖得更慢、堆积更多。
// 直接测 triggerReady，不经过真实网络。
func TestConn_TriggerReadySkipsWhilePreviousStillRunning(t *testing.T) {
	// 注意：断言不依赖 c.readyWG.Wait() 做同步——那正是本测试要验证的机制
	// 本身，如果实现回归成不再用 readyWG 追踪协程，Wait() 会退化成立即返回，
	// 用它来同步会让测试看不出第二次触发其实偷偷跑了。改用 entered 这个
	// channel 加有超时的 select 独立确认。
	entered := make(chan struct{}, 8)
	var calls atomic.Int32
	release := make(chan struct{})

	c := NewConn(
		exchange.ConnPlan{},
		decoderFunc(func(b []byte) (exchange.Event, error) { return exchange.Event{}, nil }),
		func(exchange.Event) {},
		func(context.Context, []exchange.Sub) {
			calls.Add(1)
			entered <- struct{}{}
			<-release
		},
		quietLogger(), Policy{Backoff: Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond}},
	)

	c.triggerReady(context.Background(), nil)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("第一次 triggerReady 未进入 OnReady")
	}

	c.triggerReady(context.Background(), nil) // 上一次还没跑完，这次应该被跳过

	// 给"万一没被跳过、第二次也并发跑了"一个观察窗口。
	select {
	case <-entered:
		t.Error("第二次 triggerReady 不该进入 OnReady（上一次还没跑完，应被 in-flight 守卫跳过）")
	case <-time.After(50 * time.Millisecond):
		// 符合预期：没有第二次并发调用。
	}

	close(release)

	// 等第一次真正跑完——不依赖 readyWG，直接轮询 calls 的值。
	deadline := time.After(3 * time.Second)
	for calls.Load() < 1 {
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatal("OnReady 未完成")
		}
	}
	// release 后再留一个小窗口，确认没有迟到的第二次调用冒出来。
	time.Sleep(20 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Errorf("OnReady 实际被调用 %d 次, want 1（第二次触发应被 in-flight 守卫跳过）", got)
	}
}
