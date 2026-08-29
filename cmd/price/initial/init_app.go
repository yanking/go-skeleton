// Package initial 是 price 服务的装配层：基础组件与业务组件分函数构造
// （createInfra / createServer），App 组装并交给 pkg/app 编排启停；
// oneshot.go 另外产出两个不进 pkg/app 的一次性子命令入口（instruments、
// backfill）。
package initial

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/yanking/go-skeleton/internal/price/config"
	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/exchange/binance"
	"github.com/yanking/go-skeleton/internal/price/exchange/okx"
	"github.com/yanking/go-skeleton/internal/price/job"
	"github.com/yanking/go-skeleton/internal/price/ratelimit"
	"github.com/yanking/go-skeleton/internal/price/repo"
	"github.com/yanking/go-skeleton/internal/price/service"
	"github.com/yanking/go-skeleton/internal/price/stream"
	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"github.com/yanking/go-skeleton/pkg/pgsql"
	"github.com/yanking/go-skeleton/pkg/redis"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

// App 装配 price 的全部组件并阻塞运行，直到 ctx 取消（正常停机）或组件意外退出。
// 返回非 nil 时调用方（cmd）应以非零码退出。
//
// 组装顺序即注册顺序：基础组件在前、业务组件在后——pkg/app 按注册顺序拉起、
// 逆序停止，于是基础组件先起后停，业务组件停机期的遥测与数据操作仍有着落。
func App(ctx context.Context, c config.Config, logger *slog.Logger) error {
	// Logger 是装配期注入项，配置文件不出现，在此填入。
	c.App.Logger = logger
	infra, tel, db, rdb := createInfra(ctx, c, logger)
	components, svc, managers := createServer(c, logger, tel, db, rdb)

	// warmup 做的是真实网络/DB I/O（Plans 读订阅表、Rebuild 拨号），刻意放在
	// createServer 之外、app.New(...).Run 之前单独调用——createServer 因此是
	// 一个不做任何 I/O 的纯构造函数，它返回的组件顺序（尤其是「writer 必须
	// 先于全部 stream.Manager 注册」这条本任务最要紧的不变量）才能脱离网络/
	// DB 依赖，单独用单测覆盖（见 init_app_test.go
	// TestCreateServer_WriterRegisteredBeforeAllStreamManagers）。这条不变量
	// 此前只有一段注释守着——改错了不会编译失败、不会测试失败，只会在生产
	// 停机时挂死（评审 R33）。
	warmup(ctx, svc, managers, logger)

	return app.New(c.App, append(infra, components...)...).Run(ctx)
}

// createInfra 构造基础组件：被业务依赖的资源（遥测、DB、Redis）。
// 句柄解嵌后传给业务构造侧使用，组件本身留在这里注册，保证仓储层拿不到
// 关停资源的能力。
func createInfra(ctx context.Context, c config.Config, logger *slog.Logger) (components []app.Component, tel *telemetry.Telemetry, db *pgsql.DB, rdb *redis.DB) {
	// 遥测 Service 与日志同源（配置文件 log.name），避免两处各写一份服务名。
	c.Telemetry.Service = c.Log.Service
	tel = telemetry.New(ctx, c.Telemetry)
	components = append(components, tel)

	c.Pgsql.TracerProvider = tel.TracerProvider()
	c.Pgsql.Logger = logger
	db = pgsql.New(c.Pgsql)
	components = append(components, db)

	c.Redis.TracerProvider = tel.TracerProvider()
	c.Redis.Logger = logger
	rdb = redis.New(c.Redis)
	components = append(components, rdb)

	return components, tel, db, rdb
}

// createServer 构造业务组件：按交易所各建一个 stream.Manager、订阅集重载
// job、kline/snapshot 写入器。纯构造，不做任何网络/DB I/O——真正有 I/O 的
// 装配期预热步骤拆到 warmup，由 App 在拿到这里的返回值之后单独调用（理由见
// warmup 与 App 的注释）。除了组件切片，也把 svc 与 managers 原样交还给
// 调用方——warmup 要用它们跑首轮 Plans + Rebuild。
func createServer(c config.Config, logger *slog.Logger, tel *telemetry.Telemetry, db *pgsql.DB, rdb *redis.DB) (components []app.Component, svc *service.Price, managers map[string]*stream.Manager) {
	// 出站 HTTP 客户端：两个交易所 adapter 共用，接遥测后下游调用埋 client span。
	hc := httpc.New(httpc.Config{TracerProvider: tel.TracerProvider()})

	deps := service.Deps{
		Instruments: repo.NewInstrument(db.DB),
		Subs:        repo.NewSubscription(db.DB),
		Klines:      repo.NewKline(db.DB),
		Latest:      repo.NewLatest(rdb.UniversalClient),
		Exchanges:   map[string]exchange.Exchange{},
		Limits:      map[string]*ratelimit.Bucket{},
	}

	// policies 与 deps.Exchanges/deps.Limits 同键，供下面按交易所各建一个
	// stream.Manager 时取用；三者必须在同一处、按同一份 config.Exchange 循环
	// 构造，避免交易所维度散落在多处、悄悄漏掉某一项。
	policies := make(map[string]stream.Policy, len(c.Exchanges))
	for name, cfg := range c.Exchanges {
		// Enabled 是运维不删配置块、临时关停单个交易所的开关，零值 false 就是
		// 「跳过」——不读它的后果是运维改了 yaml 以为关停生效，实际这里仍然
		// 照常拨号，与本任务约束 2「配置里有字段却没人读」是同一类问题。跳过
		// 时必须留一条日志：漏写 enabled: true（零值 false 不直觉地等于关停）
		// 与显式关停在效果上完全一样，进程照常起来、一条连接都不建，日志里
		// 若什么都不留，运维会在这里排查很久（评审 Important 2）。
		if !cfg.Enabled {
			logger.Info("交易所已关停，跳过装配", "exchange", name)
			continue
		}

		impl, err := buildExchange(name, cfg, hc)
		if err != nil {
			// 交易所名不认识（yaml 拼写错误）或必填字段缺失，都是装配期配置
			// 错误——宪法第 1 条允许 panic，同 pkg/pgsql.New、两个 adapter 的
			// New 既有约定，好过带病上线后每次拨号都失败。
			logger.Error("构造交易所客户端失败", "exchange", name, "err", err)
			panic(err)
		}
		deps.Exchanges[name] = impl

		// 每个交易所一个共享限速桶：常驻的自动补洞（OnReady 触发）与
		// backfill 子命令共用同一份配置构造出的同样的桶（本任务约束 3；两者
		// 是不同进程，无法共享同一个 *ratelimit.Bucket 实例，见 ratelimit
		// 包注释对「共用」在跨进程语境下的准确表述）。
		deps.Limits[name] = ratelimit.New(cfg.RESTPerSecond, cfg.RESTBurst)

		// DialTimeout/ReconnectBackoffMin/Max 此前全仓无人读取，stream 包
		// 一直在用包内兜底默认——这里把 config.Exchange 的值真正接进
		// stream.Policy，兜底默认从此只在漏配（零值）时才生效（本任务约束 2）。
		policies[name] = stream.Policy{
			DialTimeout: cfg.DialTimeout,
			Backoff:     stream.Backoff{Min: cfg.ReconnectBackoffMin, Max: cfg.ReconnectBackoffMax},
		}
	}

	svc = service.New(service.Config{
		KlineQueueSize:      c.Collector.KlineQueueSize,
		SnapshotQueueSize:   c.Collector.SnapshotQueueSize,
		MaxBackfillWindow:   c.Collector.MaxBackfillWindow,
		BackfillConcurrency: c.Collector.BackfillConcurrency,
	}, deps, logger)

	// ↓↓↓ 注册顺序里最要紧的一条：writer 必须排在全部 stream.Manager 之前。
	//
	// pkg/app 按注册顺序拉起、逆序停止。writer 排前面 → 停机时按逆序，
	// 全部 Manager.Stop() 先跑完（等到其名下每条 ws 连接的读循环真正退出，
	// 即不再有 goroutine 调用 route()），writerComponent.Stop() 才被调用去
	// 取消 RunWriters 用的内部 ctx（细则见 writerComponent 类型注释）。
	// 顺序反了：writer 会先于 Manager 停止消费 klineCh，此时仍在跑的读循环
	// 一旦收到已收线的 K 线，会永久阻塞在 route() 里往 klineCh 发送
	// （kline 队列满即阻塞是刻意设计，不可丢），进程停机就此挂死。
	// 这条顺序本身没有任何代码强制，纯靠这里的注册顺序兑现，改动前须读完
	// 这段注释——TestCreateServer_WriterRegisteredBeforeAllStreamManagers 会
	// 在回归时抓到顺序被改错。
	components = append(components, newWriterComponent(svc))

	managers = make(map[string]*stream.Manager, len(deps.Exchanges))
	for name, impl := range deps.Exchanges {
		mgr := stream.NewManager(impl, svc.RouteFor(name), svc.OnReady(name), logger, policies[name])
		managers[name] = mgr
		components = append(components, mgr)
	}
	if len(managers) == 0 {
		// exchanges 段缺失、或全部交易所都被 enabled: false 关停时会走到这——
		// daemon 仍会正常起来并一直跑，只是不建立任何 ws 连接、空转采集。这类
		// 极端情况理论上不该发生在生产配置里，但既然可能发生，就不能只靠上面
		// 逐个交易所的 Info 日志——留一条更醒目的 Warn，方便运维一眼看出「不是
		// 没在跑，是没什么可跑的」。
		logger.Warn("没有任何已启用的交易所，daemon 不会建立任何 ws 连接，将空转", "exchanges_configured", len(c.Exchanges))
	}

	// map[string]*stream.Manager 转 map[string]job.Rebuilder：Go 不支持 map
	// 元素类型的隐式转换，必须逐项显式转（本任务约束 4）。
	rebuilders := make(map[string]job.Rebuilder, len(managers))
	for name, mgr := range managers {
		rebuilders[name] = mgr
	}
	components = append(components, job.NewReload(svc, rebuilders, c.Collector.ReloadInterval, logger))

	return components, svc, managers
}

// warmup 装配期主动跑一次 Plans + Rebuild：reload job 的首轮虽然也会立即
// 执行（job.NewReload 文档），但那要等到 app.Run() 真正拉起该组件的 goroutine
// 才触发；从进程起来到那一刻之间会有一段窗口，Manager 里一条连接都没有。
// 这里提前建好连接、缩掉这段空窗，此后交给 reload job 按周期维持。是本次
// 装配链路里唯一有真实网络/DB I/O 的步骤，因此独立于 createServer 之外
// （细则见 App 与 createServer 的注释）。
//
// 读取计划失败（最常见的是 DB 暂不可达）只 Warn、不阻断装配——这是运行期
// 故障，不是装配期配置错误：pkg/pgsql 包注释明确「DB 比进程晚就绪是常态」，
// 装配期不 ping、建连失败留给首个查询暴露，此处的 Plans() 正是那个首个
// 查询，失败了也不该整个进程直接死掉（本任务约束 5）；reload job 的下一
// 个周期会自动重试。
func warmup(ctx context.Context, svc *service.Price, managers map[string]*stream.Manager, logger *slog.Logger) {
	plans, err := svc.Plans(ctx)
	if err != nil {
		logger.Warn("装配期读取初始连接计划失败，等待 reload job 下一轮重试", "err", err)
		return
	}
	for name, mgr := range managers {
		p, ok := plans[name]
		if !ok {
			continue
		}
		if err := mgr.Rebuild(ctx, p); err != nil {
			logger.Warn("装配期建立初始连接失败，等待 reload job 下一轮重试", "exchange", name, "err", err)
		}
	}
}

// buildExchange 按交易所名与其配置构造对应的 exchange.Exchange 实现；目前只
// 认 binance/okx，新增交易所时在这里加一个 case。cfg.HTTP 缺失等装配期错误
// 由 binance.New/okx.New 内部直接 panic（既有约定），本函数只处理「交易所名
// 不在支持列表内」这一种失败，返回 error 而不是自己 panic——调用方（daemon
// 的 createServer、oneshot 的两个子命令）对同一种失败的处理方式不同：daemon
// 视为装配期错误直接 panic，子命令沿用户输入错误的既有惯例把 error 原样
// 上抛给 CLI 调用方，不适合在这个共享的构造函数里替调用方下这个判断。
func buildExchange(name string, cfg config.Exchange, hc *httpc.Client) (exchange.Exchange, error) {
	switch name {
	case "binance":
		return binance.New(binance.Config{
			WSURL:             cfg.WSURL,
			RESTURL:           cfg.RESTURL,
			MaxStreamsPerConn: cfg.MaxStreamsPerConn,
			ImportQuotes:      cfg.ImportQuotes,
			HTTP:              hc,
		}), nil
	case "okx":
		return okx.New(okx.Config{
			WSURL:             cfg.WSURL,
			RESTURL:           cfg.RESTURL,
			MaxStreamsPerConn: cfg.MaxStreamsPerConn,
			ImportQuotes:      cfg.ImportQuotes,
			HTTP:              hc,
		}), nil
	default:
		return nil, fmt.Errorf("不支持的交易所 %q（目前只认 binance/okx）", name)
	}
}

// writerRunner 是 writerComponent 依赖的能力，*service.Price 实现。定义成
// 接口是为了能在装配层单独给这个薄封装写测试，不必在测试里搭一整套
// service.Price 的依赖（DB、Redis、限速桶等）。
type writerRunner interface {
	RunWriters(ctx context.Context) error
}

// writerComponent 把 service.Price.RunWriters 包装成 app.Component，驱动它的
// ctx 与 pkg/app 传给 Start 的根 ctx 刻意解耦——根 ctx 取消（收到停机信号）时
// 全部组件几乎同时收到通知，若这里也直接用根 ctx，写协程会与 ws
// stream.Manager 同时进入各自的停机流程：RunWriters 的停机排空最多只等
// shutdownFlushTimeout（5s）就会返回、彻底停止消费 klineCh，但此时 Manager
// 名下的连接可能还没断——它们的读循环仍可能在往 klineCh 发送已收线的 K 线
// （无缓冲时是阻塞发送，见 route.go），一旦 writer 已经不再消费，这些
// goroutine 就会永久卡住，进程停机挂死。
//
// 正确的驱动方式是让 Start 阻塞用自己持有的内部 ctx 跑 RunWriters，只有
// Stop 被调用时才取消它——这样 RunWriters 何时开始排空完全由「本组件的
// Stop 何时被调用」决定，而不是「根 ctx 何时取消」。装配层把本组件注册在
// 全部 stream.Manager 之前，pkg/app 逆序停止时 Manager.Stop()
// （等全部连接读循环真正退出）先跑完，本组件的 Stop 才被调用，由此保证
// RunWriters 排空并返回之前，不会再有 goroutine 尝试往 klineCh 发送——这条
// 顺序保证的另一半兑现在 createServer 的注册顺序里，两处须对着看。
type writerComponent struct {
	run    writerRunner
	ctx    context.Context
	cancel context.CancelFunc

	// started、done 供 Stop 判断该怎么等：started 在 Start 真正开始运行时
	// 置位，done 在 RunWriters 返回后关闭。两者都是本组件自己的状态，不依赖
	// 调用方的 ctx——理由与 job.reload 的同名字段完全一致（同仓范本，照其
	// 形态写）：Start 从未被调用过时 Stop 无事可等，若不加这道判断、无条件
	// 等 done，用 context.Background() 调用 Stop（如按 pkg/app.Component
	// 约定「Stop 可能在组件尚未真正运行时被调用」的场景）会永久阻塞。
	started atomic.Bool
	done    chan struct{}
}

// newWriterComponent 构造 writer 组件；内部 ctx 独立于任何外部 ctx，仅由
// Stop 取消。
func newWriterComponent(run writerRunner) *writerComponent {
	ctx, cancel := context.WithCancel(context.Background())
	return &writerComponent{run: run, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// Name 实现 app.Component。
func (w *writerComponent) Name() string { return "price-writer" }

// Start 实现 app.Component：阻塞运行 RunWriters，用的是本组件内部持有的
// ctx，不是入参——理由见类型注释。
func (w *writerComponent) Start(context.Context) error {
	w.started.Store(true)
	defer close(w.done)
	return w.run.RunWriters(w.ctx)
}

// Stop 实现 app.Component：取消内部 ctx 触发 RunWriters 的停机排空与落盘，
// 等它真正返回，最多等到 ctx（app 共享的停机预算）到期；Start 从未被调用过
// （started 仍是零值 false）时无事可等，直接返回 nil——满足
// pkg/app.Component 「Stop 可能在组件尚未真正运行时被调用，实现须容忍」的
// 约定，与 job.reload.Stop 同一处理方式。
func (w *writerComponent) Stop(ctx context.Context) error {
	w.cancel()
	if !w.started.Load() {
		return nil
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("停止写入组件: 宽限期耗尽，RunWriters 未在预期时间内返回: %w", ctx.Err())
	}
}
