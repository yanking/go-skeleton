package stream

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// defaultDialTimeout 单次拨号超时的兜底默认值：Policy.DialTimeout 未配置
// （零值）时使用，仍是「拨号不会无限挂起」的最后一道安全网。真正的运行时取值
// 应经 Policy 由装配层从 config.Exchange 传入，不应长期停留在这个兜底值上。
const defaultDialTimeout = 10 * time.Second

// defaultBackoffMin、defaultBackoffMax 是 Policy.Backoff 未配置（零值）时的
// 兜底默认值。缺这道兜底的后果很具体：Backoff{} 零值下 Next() 恒返回 0，
// Run 的重连循环就变成不带节流的热循环——交易所不可达时以 TCP connect 失败的
// 速度空转，每秒数千次重连、CPU 打满、IP 被交易所封。真正的运行时取值应经
// Policy 由装配层从 config.Exchange 的 reconnect_backoff_min/max 传入。
const (
	defaultBackoffMin = time.Second
	defaultBackoffMax = 60 * time.Second
)

// Handler 处理一帧解码后的中立事件。
type Handler func(exchange.Event)

// OnReady 连接进入可用状态时触发：拨号成功并把 ConnPlan.Subscribe 的订阅帧
// 发送完毕之后调用，每次重连都会再触发一次——首连、断线重连、订阅集重建
// （Manager.Rebuild）由此统一成同一个信号，后续的历史补洞逻辑挂在这个回调上。
//
// 在独立协程里执行，不阻塞读循环启动：coder/websocket 的 pong 只在读路径里
// 发出，只有 Read 已经在跑时服务端心跳才有人应答；补洞这类耗时以十秒到分钟计
// 的操作如果同步执行会让读循环迟迟启动不了，交易所判定客户端失联进而断开，
// 断开又触发重连、重连再次同步卡在这个空档——形成活锁。
//
// ctx 是触发这次 OnReady 的那条连接的 Run(ctx) 原样传入的 ctx，不是
// context.Background()：连接被取消（Manager.stopAllLocked 触发的 cancel，
// 常见于 Rebuild/Stop）时 ctx 随之取消，回调实现须据此让自己的耗时操作
// （REST 补洞等）能被中途打断——否则一次涉及大量翻页的补洞会吃光
// pkg/app.StopTimeout 这份全部组件共享的停机预算：第一个卡在这里的连接
// 耗尽全部预算，后续组件（如 kline writer）拿到的 stopCtx 已经过期，来不及
// 排空在途数据就被判定超时。回调实现取消后不必担心数据丢失：补洞是水位线
// 追赶式的，起点还在断点上，下次触发（重连）会自动续上，见 service.Backfill
// 的类型注释。
//
// 协程的生命周期绑定在这条连接的 Run 调用内，不会跑到 Run 返回之后——Run
// 返回前会等它跑完（见 triggerReady），所以不会在 Manager.Stop 已经成功返回、
// app 按逆序开始关闭 DB/Redis 等基础组件之后，还有游离的 OnReady 协程在写库、
// 撞上已经关闭的连接池；ctx 的传入进一步保证了这个等待是有界的——回调只要
// 遵守 ctx 取消就会尽快退出，不会让 Run 迟迟返回不了。同一条连接同一时刻至多
// 一个 OnReady 在跑：如果上一次触发还没结束，下一次重连触发的 OnReady 会被
// 跳过而不是并发再起一个——这道 in-flight 守卫既防住「握手通过即关闭」这类
// 快速反复重连场景下协程无界累积（即便触发频率已经被 Backoff 压低，同时跑的
// 补洞数量本身也得有上限，否则耗时的补洞叠加耗时的补洞，还会反过来把 REST
// 限速拖得更慢、堆积更多），也顺带保证了同一个 subs 切片不会被两个并发的
// OnReady 同时持有。
//
// 但两处并发仍需实现自己处理：① 该协程会与本连接读循环对 Handler 的调用
// 并发执行——引入 OnReady 前，ready 保证跑在任何 handle(event) 之前；现在
// 没有这个顺序保证了，OnReady 写历史数据与 Handler 写实时数据可能同时命中
// 同一个 (symbol, interval)，需要自己保证幂等/无冲突。② subs 是
// ConnPlan.Subs 的原始切片，同一条连接历次触发传入的是同一个底层数组（不是
// 每次给一份新副本）——in-flight 守卫防住了两次触发同时跑，但如果实现原地
// 排序/修改这个切片，是在改一份跨越多次触发共享的状态，读者不该假设每次
// 拿到的都是"干净"的原始顺序。
type OnReady func(ctx context.Context, subs []exchange.Sub)

// Decoder 把一帧原始 WebSocket 消息解码为中立事件，由具体交易所实现
// （exchange.Exchange 的 Decode 方法签名与本接口一致）。
type Decoder interface {
	Decode(raw []byte) (exchange.Event, error)
}

// Backoff 指数退避参数：重试间隔按 2^attempt 从 Min 增长、封顶 Max，
// 并叠加随机抖动避免大量连接同时重连造成惊群。
type Backoff struct {
	Min time.Duration // 首次重试的基准间隔
	Max time.Duration // 间隔上限
}

// Next 返回第 attempt 次重试（从 0 开始计数）前应等待的时长：先计出
// min(Min*2^attempt, Max)，再从 [0, 该值] 里取随机值（full jitter），
// 使同一批连接的重试时间点被打散。
func (b Backoff) Next(attempt int) time.Duration {
	d := b.Min
	for i := 0; i < attempt && d < b.Max; i++ {
		d *= 2
		if d <= 0 { // 溢出保护：翻倍到超出 time.Duration 表示范围
			d = b.Max
			break
		}
	}
	if d > b.Max {
		d = b.Max
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(d) + 1))
}

// Policy 一条连接的运行策略：拨号超时与断线重连的退避参数，两者同属
// config.Exchange 同一段配置，合并成一个结构体传给 NewConn，避免签名继续
// 叠加位置参数。DialTimeout、Backoff.Min、Backoff.Max 为零值时均退回包内
// 安全默认——运维漏配不会变成「无限挂起的拨号」或「不带节流的热重连循环」。
type Policy struct {
	DialTimeout time.Duration // 单次拨号的超时上限；零值退回 defaultDialTimeout
	Backoff     Backoff       // 断线重连的退避参数；Min/Max 零值分别退回默认
}

// resolve 给零值字段套上包内安全默认，返回值即 Conn 实际使用的策略。
func (p Policy) resolve() Policy {
	if p.DialTimeout <= 0 {
		p.DialTimeout = defaultDialTimeout
	}
	if p.Backoff.Min <= 0 {
		p.Backoff.Min = defaultBackoffMin
	}
	if p.Backoff.Max <= 0 {
		p.Backoff.Max = defaultBackoffMax
	}
	return p
}

// Conn 管理一条 WebSocket 连接的完整生命周期：拨号、发送订阅帧、心跳、读循环、
// 断线后按 Policy.Backoff 退避重连。
type Conn struct {
	plan   exchange.ConnPlan
	dec    Decoder
	handle Handler
	ready  OnReady
	logger *slog.Logger
	policy Policy

	// readyWG、readyBusy 把 OnReady 协程的生命周期与并发度绑定在本连接上，
	// 细则见 triggerReady 与 OnReady 的类型注释；跨越本连接历次重连共用同一份
	// （字段在 Conn 而不是每次 runOnce 现起），这样「上一次触发还没跑完」这件
	// 事才能跨重连感知到。
	readyWG   sync.WaitGroup
	readyBusy atomic.Bool
}

// NewConn 构造一条连接的生命周期管理器。policy 的零值字段会在此处套上包内
// 安全默认（见 Policy.resolve），调用方误配置零值不会导致拨号无限挂起或
// 退避失效变成热循环。
func NewConn(plan exchange.ConnPlan, dec Decoder, h Handler, ready OnReady, logger *slog.Logger, policy Policy) *Conn {
	return &Conn{plan: plan, dec: dec, handle: h, ready: ready, logger: logger, policy: policy.resolve()}
}

// Run 阻塞运行连接的生命周期，直到 ctx 被取消才返回 nil——这是唯一的正常退出
// 路径。拨号失败或运行中断线都按 Policy.Backoff 退避后原地重试，不当作错误
// 返回：交易所会定时强制断开连接（如 Binance 现货文档写明单连接有生命周期
// 上限），把断线当异常处理会长出两套逻辑，当作普通断线则重连路径每天都被
// 真实演练。
//
// 退避计数只在「连续失败」期间累积：一旦某次连接真正活起来（订阅完成后存活
// 时长超过 Policy.Backoff.Min，细则见 runOnce），退避计数清零——那类每天例行
// 强制断开的连接活了一整天，断开前是健康的，不是失败的延续，断开后不该背着
// 之前失败序列的包袱等到接近 Max 才恢复采集。反过来，握手通过又立刻断开
// （限流、封禁、维护期常见形态）不算数，否则会退化成对一个拒绝服务地址的
// 无退避快速重试。
func (c *Conn) Run(ctx context.Context) error {
	attempt := 0
	for ctx.Err() == nil {
		connected, err := c.runOnce(ctx)
		if connected {
			attempt = 0
		}
		if err == nil || ctx.Err() != nil {
			// runOnce 仅在 ctx 已取消时返回 nil；ctx 在 runOnce 执行期间被取消
			// 也可能让它带着一个「拨号被取消」之类的 error 返回——这两种情况
			// 都不是真断线，不记重连日志、不等待退避，下一轮循环条件即退出。
			continue
		}
		c.logger.WarnContext(ctx, "ws 连接中断，准备重连",
			"url", c.plan.URL, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(c.policy.Backoff.Next(attempt)):
		}
		attempt++
	}
	// 等最后一次触发的 OnReady 跑完再返回：Run 的 goroutine 一旦返回，
	// 上层（Manager 的 done channel）就认定这条连接彻底退出了；如果这里不等，
	// Manager.Stop 可能在 OnReady 仍在写库时就已经成功返回，随后 app 按逆序
	// 关闭 DB/Redis 等基础组件，这个游离的协程会撞上已经关闭的连接池。
	c.readyWG.Wait()
	return nil
}

// runOnce 拨号一次并运行到断开或 ctx 取消。connected 表示这次连接是否「真正
// 活起来」，用于决定 Run 要不要清零退避计数——不是「订阅帧 Write 有没有返回
// 成功」，而是「订阅完成到断开为止活了多久」：服务端握手通过又立刻关闭（限流、
// 封禁、维护期常见形态）不该被当作成功，否则每轮都清零退避、变成对一个拒绝
// 服务地址的无退避快速重试，同时 OnReady 被高频重复触发，把 REST 补洞的限速
// 打爆。存活时长超过 Policy.Backoff.Min 才算数——这仍完整保住「连接活了一整天
// 才被例行断开」的场景。err 仅当 ctx 已取消时为 nil。
func (c *Conn) runOnce(ctx context.Context) (connected bool, err error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return false, fmt.Errorf("拨号 %s: %w", c.plan.URL, err)
	}
	defer conn.CloseNow()

	if err := c.subscribe(ctx, conn); err != nil {
		return false, fmt.Errorf("发送订阅帧: %w", err)
	}

	// 连上且订阅帧已发完，进入可用状态：触发 OnReady。放到独立协程执行、
	// 不等它返回，且带 in-flight 守卫，理由见 OnReady 的类型注释。ctx 就是
	// runOnce 收到的这条连接的 Run(ctx)，不额外派生——OnReady 要能被连接
	// 取消打断，用的必须是这同一个 ctx（见 OnReady 类型注释）。
	c.triggerReady(ctx, c.plan.Subs)

	if c.plan.ClientPing != nil {
		if c.plan.PingEvery <= 0 {
			// ConnPlan 对 ClientPing 与 PingEvery 没有耦合约束：具体交易所实现
			// 若设了 ClientPing 却忘了配 PingEvery，time.NewTicker 会 panic，
			// 且是在 goroutine 里，调用方无法 recover，直接打死整个进程——
			// 违反宪法条款 1（panic 只允许装配期配置错误，这是运行期数据）。
			// 记日志、跳过心跳协程即可：没有客户端心跳这条连接大概率会被交易所
			// 断开，走正常的断线重连路径，不必让进程崩溃。
			c.logger.ErrorContext(ctx, "ConnPlan.ClientPing 非空但 PingEvery 未设置，跳过心跳协程",
				"url", c.plan.URL)
		} else {
			pingCtx, stopPing := context.WithCancel(ctx)
			defer stopPing()
			go c.pingLoop(pingCtx, conn)
		}
	}

	connectedAt := time.Now()
	err = c.readLoop(ctx, conn)
	connected = time.Since(connectedAt) >= c.policy.Backoff.Min
	return connected, err
}

// triggerReady 触发 OnReady：只在没有上一次触发仍在跑的情况下才起新协程，
// 起的协程用 c.readyWG 追踪——Run 返回前会 Wait() 等它跑完，把协程的生命
// 周期与并发度都绑定在本连接上，细则见 OnReady 的类型注释。ctx 原样透传给
// c.ready，不在这里派生或替换——它是否会被取消、何时取消，由调用方（runOnce）
// 决定。
func (c *Conn) triggerReady(ctx context.Context, subs []exchange.Sub) {
	if !c.readyBusy.CompareAndSwap(false, true) {
		c.logger.Warn("上一次 OnReady 尚未跑完，跳过本次触发", "url", c.plan.URL)
		return
	}
	c.readyWG.Add(1)
	go func() {
		defer c.readyWG.Done()
		defer c.readyBusy.Store(false)
		c.ready(ctx, subs)
	}()
}

// dial 带超时拨号，避免网络异常时无限期挂起整条重连循环。
func (c *Conn) dial(ctx context.Context) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, c.policy.DialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, c.plan.URL, nil)
	if err != nil {
		return nil, err
	}
	// coder/websocket 单帧读取上限默认 32KiB（库内 Conn.SetReadLimit 的默认值），
	// 本包未调整：当前只订 depth20/books5 一类有限档位的深度快照，单帧报文在
	// 1-2KB 量级，离默认上限还远。若后续订阅加深档位（如全量增量深度）导致单帧
	// 逼近或超过 32KiB，需要在此显式调用 conn.SetReadLimit 放宽——届时上限值
	// 大概率要从 ConnPlan 或 config.Exchange 传入，而不是继续硬编码在这里。
	return conn, nil
}

// subscribe 按序发送 ConnPlan.Subscribe 里的订阅帧；为空表示订阅已编进 URL，
// 无需额外发送。
func (c *Conn) subscribe(ctx context.Context, conn *websocket.Conn) error {
	for _, frame := range c.plan.Subscribe {
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			return err
		}
	}
	return nil
}

// pingLoop 按 ConnPlan.PingEvery 周期发送客户端心跳帧；仅当交易所要求客户端
// 主动心跳（ConnPlan.ClientPing 非 nil，如 OKX）且 PingEvery 是正值时由
// runOnce 启动。写失败即说明连接已不可用，读循环会自行感知断线并退出，本函数
// 不重复上报错误。
func (c *Conn) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(c.plan.PingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.Write(ctx, websocket.MessageText, c.plan.ClientPing); err != nil {
				return
			}
		}
	}
}

// readLoop 逐帧读取并解码：解码失败只记日志不断连（一帧解不动不该杀连接，
// 交易所也会推送订阅确认、心跳应答等本就无需处理的帧）；读失败（断线）才返回
// error 触发重连；ctx 取消时返回 nil。
func (c *Conn) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("读取帧: %w", err)
		}
		event, err := c.dec.Decode(raw)
		if err != nil {
			c.logger.WarnContext(ctx, "解码帧失败，跳过", "url", c.plan.URL, "err", err)
			continue
		}
		c.handle(event)
	}
}
