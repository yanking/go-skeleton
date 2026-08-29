package initial

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yanking/go-skeleton/internal/price/config"
	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/ratelimit"
	"github.com/yanking/go-skeleton/internal/price/repo"
	"github.com/yanking/go-skeleton/internal/price/service"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"github.com/yanking/go-skeleton/pkg/pgsql"
)

// BackfillArgs 是 backfill 子命令的显式参数：对齐 exchange.Sub 的四个维度
// （Market/Symbol 即 NativeSymbol/Interval）外加显式的补洞区间。与 daemon
// 断线重连后触发的自动补洞（service.Price.Backfill，水位线追赶式）不是
// 同一回事——这里的起止点完全由调用方给定，不查库，用于事故恢复、回填历史
// 等一次性场景（design.md §8）。
type BackfillArgs struct {
	Exchange, Market, Symbol, Interval string
	From, To                           time.Time
}

// oneshotExchange 是 Instruments/Backfill 两个子命令共用的构造步骤：按
// config.Config.Exchanges 里 ex 对应的段构造交易所客户端与共享限速桶。
// 不校验 cfg.Enabled——子命令是操作者对着 --exchange 显式点名的一次性动作
// （如「daemon 因故已经关停这家交易所、但仍要补一段历史」），不该被
// daemon 用的「是否常驻连接」开关连带挡住。
func oneshotExchange(c config.Config, hc *httpc.Client, ex string) (exchange.Exchange, *ratelimit.Bucket, error) {
	cfg, ok := c.Exchanges[ex]
	if !ok {
		return nil, nil, fmt.Errorf("交易所 %q 未在配置文件的 exchanges 段声明", ex)
	}
	impl, err := buildExchange(ex, cfg, hc)
	if err != nil {
		return nil, nil, fmt.Errorf("构造交易所客户端: %w", err)
	}
	// 与常驻进程同一份配置构造出同样的桶；子命令是独立进程，不可能与 daemon
	// 共享同一个 *ratelimit.Bucket 实例，「共用」在跨进程语境下的含义是
	// 「同一份配置」，见 ratelimit 包注释与本任务约束 3。
	limiter := ratelimit.New(cfg.RESTPerSecond, cfg.RESTBurst)
	return impl, limiter, nil
}

// Instruments 跑一次全量交易对导入，跑完返回；不进 pkg/app，进程本该在
// 调用方（main）随后退出。只造本次需要的资源：pgsql（写交易对表）、httpc、
// 目标交易所的客户端与限速桶、service——不建 redis 连接，ImportInstruments
// 不写 Redis。
func Instruments(ctx context.Context, c config.Config, logger *slog.Logger, ex string) error {
	c.Pgsql.Logger = logger
	db := pgsql.New(c.Pgsql)
	// 子命令不进 pkg/app，没有 App.Stop 帮忙收尾——自己负责关闭连接池；
	// db.Stop 内部不看传入的 ctx（直接关底层 *sql.DB），用
	// context.Background() 而不是 ctx 是为了不受调用方 ctx 已取消（如子命令
	// 跑到一半收到信号）影响，defer 阶段仍能正常关闭。
	defer db.Stop(context.Background())

	hc := httpc.New(httpc.Config{})
	impl, limiter, err := oneshotExchange(c, hc, ex)
	if err != nil {
		return err
	}

	svc := service.New(service.Config{}, service.Deps{
		Instruments: repo.NewInstrument(db.DB),
		Exchanges:   map[string]exchange.Exchange{ex: impl},
		Limits:      map[string]*ratelimit.Bucket{ex: limiter},
	}, logger)

	return svc.ImportInstruments(ctx, ex)
}

// Backfill 按显式区间补一段历史 K 线，跑完返回；不进 pkg/app。只造本次需要
// 的资源：pgsql（写 K 线表）、httpc、目标交易所的客户端与限速桶、
// service——同样不建 redis 连接，补洞不写 Redis。
func Backfill(ctx context.Context, c config.Config, logger *slog.Logger, args BackfillArgs) error {
	c.Pgsql.Logger = logger
	db := pgsql.New(c.Pgsql)
	// 理由同 Instruments：子命令自己负责关闭连接池，用 context.Background()
	// 不受调用方 ctx 已取消影响。
	defer db.Stop(context.Background())

	hc := httpc.New(httpc.Config{})
	impl, limiter, err := oneshotExchange(c, hc, args.Exchange)
	if err != nil {
		return err
	}

	svc := service.New(service.Config{}, service.Deps{
		Klines:    repo.NewKline(db.DB),
		Exchanges: map[string]exchange.Exchange{args.Exchange: impl},
		Limits:    map[string]*ratelimit.Bucket{args.Exchange: limiter},
	}, logger)

	sub := exchange.Sub{
		Market:       args.Market,
		NativeSymbol: args.Symbol,
		StreamType:   exchange.StreamKline,
		Interval:     args.Interval,
	}
	return svc.BackfillRange(ctx, args.Exchange, sub, args.From.UnixMilli(), args.To.UnixMilli())
}
