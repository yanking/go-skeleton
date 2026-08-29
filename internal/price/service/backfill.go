package service

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
	"github.com/yanking/go-skeleton/internal/price/ratelimit"
	"github.com/yanking/go-skeleton/internal/price/stream"
)

// defaultBackfillConcurrency 是 Config.BackfillConcurrency 零值时的兜底并发
// 度，与 configs/price.yaml 里 collector.backfill_concurrency 字段注释「零值
// 取 2」一致。这道兜底同时避免了 errgroup.Group.SetLimit(0) 的死锁：limit 为
// 0 时任何 Go() 调用都拿不到信号量，会永久阻塞在那里。
const defaultBackfillConcurrency = 2

// defaultMaxBackfillWindow 是 Config.MaxBackfillWindow 零值时的兜底回溯窗口，
// 与 configs/price.yaml 里 collector.max_backfill_window 的当前取值（720h，
// 30 天）一致。零值不能被直接当成「回溯 0」使用——time.Now().Add(-0) 等于
// now，会让新标的的第一次补洞一根历史都拿不到，且没有任何报错或日志，是这个
// 项目里最危险的失败形态（静默不做事）；装配层（Task 12）一旦漏接这个字段就
// 会踩中。取与 yaml 文档值一致的默认，而不是任意值，是为了让「漏配」与「按
// 文档默认配置」的实际效果相同，不会因为漏配就产生一个 yaml 里查不到依据的
// 行为。
const defaultMaxBackfillWindow = 720 * time.Hour

// Backfill 对 ex 交易所的一批订阅做「水位线追赶式」补洞：每条订阅各自从
// KlineRepo.MaxOpenTime 查到的已落库最大开盘时间开始，接着往后拉，一路追到
// 调用时刻，而不是「回溯最近 N 分钟」的固定窗口——固定窗口式在 OnReady 的
// in-flight 守卫恰好跳过某次触发时会永久丢掉那段数据，水位线式对此天然免疫
// （起点还在断点上，下次触发照样续上），理由见 stream.OnReady 类型注释与
// Task 5 裁决 R12 的设计输入。
//
// 订阅按 Config.BackfillConcurrency（零值取 defaultBackfillConcurrency）并发
// 处理；单条订阅失败（限速等待被取消、REST 报错、写库失败）只记 Warn 并跳到
// 下一条，不让一个坏标的中断整批——与 Plans() 对单个交易所报错只 Warn 跳过
// 同一个思路。返回的 error 只在 ex 未在 Deps.Exchanges/Deps.Limits 里配置
// （装配期错误）时出现。
func (s *Price) Backfill(ctx context.Context, ex string, subs []exchange.Sub) error {
	impl, ok := s.deps.Exchanges[ex]
	if !ok {
		return fmt.Errorf("补洞: 交易所 %s 未在 Exchanges 中配置", ex)
	}
	limiter, ok := s.deps.Limits[ex]
	if !ok {
		return fmt.Errorf("补洞: 交易所 %s 未配置限速桶", ex)
	}

	concurrency := s.cfg.BackfillConcurrency
	if concurrency <= 0 {
		concurrency = defaultBackfillConcurrency
	}

	// end 取调用时刻，一批订阅共用同一个截止点——各条订阅翻页耗时不同不该
	// 让"追到哪天"因订阅而异。
	end := time.Now().UnixMilli()

	var g errgroup.Group
	g.SetLimit(concurrency)
	for _, sub := range subs {
		g.Go(func() error {
			s.backfillOne(ctx, ex, impl, limiter, sub, end)
			return nil // 单条订阅的失败已在 backfillOne 内部 Warn 处理，不向上传播、不取消其余订阅
		})
	}
	return g.Wait()
}

// backfillOne 补齐单条订阅的缺口：定起点、循环「限速等待 + 拉取 + 落库」，
// 直到 Klines 返回的下一页起点为 0。ticker/depth 订阅没有历史可补，直接跳过，
// 不得调用 Klines。
//
// 起点直接取 backfillStart 返回值（库里已有数据时就是上一根已存 K 线自己的
// 开盘时间，不做「+ 一个周期」的换算），第一轮请求会把这一根重新拉回来——
// 这是刻意的：K 线落库走 Upsert，唯一键是 (exchange, market, native_symbol,
// interval, open_time) 五列，用相同的值重新写一遍同一行是无害的全量覆盖，
// 不会产生重复行；换来的是 service 层不需要认识任何交易所的周期拼写方言。
// 周期换算（数字+单位、大小写、utc 后缀等）属于「交易所方言」（design.md
// 不变量第 4 条：symbol 形态/周期拼写/分页方向/限速规则/连接上限不出交易所
// 包），本就该留在各自的 adapter 内部——binance/rest.go 的 nextOpenTime、
// okx/rest.go 的 addPeriods 已经各自实现了它，Klines 的分页续接点直接用的
// 就是这份实现，service 层重复一份只会带来「adapter 改了周期拼写、这里悄悄
// 漂移」的维护风险，不应该在这一层再算一次。
func (s *Price) backfillOne(ctx context.Context, ex string, impl exchange.Exchange, limiter *ratelimit.Bucket, sub exchange.Sub, end int64) {
	if sub.StreamType != exchange.StreamKline {
		return
	}

	logCtx := []any{"exchange", ex, "market", sub.Market, "native_symbol", sub.NativeSymbol, "interval", sub.Interval}

	start, err := s.backfillStart(ctx, ex, sub)
	if err != nil {
		s.logger.Warn("补洞定起点失败，跳过该订阅", append(logCtx, "err", err)...)
		return
	}

	// backfillOne 服务的是「批量补洞、一条坏订阅不拖累其余订阅」的语义
	// （Backfill 的既有约定与全部既有用例），因此这里刻意丢弃 pullAndUpsert
	// 的返回值——失败已经在 pullAndUpsert 内部 Warn 过，上抛到这里也无处可去
	// （调用方 Backfill 是 errgroup 里跑的一条 goroutine，早已被约定成
	// 「返回 nil、只靠日志暴露失败」，见 Backfill 的 g.Go 闭包注释）。
	// BackfillRange（下方）服务的是完全不同的单条订阅场景，会原样上抛这个
	// 返回值，不能把两种语义混在同一个函数里。
	_ = s.pullAndUpsert(ctx, ex, impl, limiter, sub, start, end, logCtx)
}

// pullAndUpsert 是补洞的核心翻页循环：限速等待 + 拉取 + 落库，直到 Klines
// 返回的下一页起点为 0，成功耗尽全部分页后返回 nil。被 backfillOne（起点取
// 水位线，daemon 断线重连后的自动补洞用）与 BackfillRange（起点/止点由调用
// 方显式给定，backfill 子命令用）共用——两者只是起点、止点的来源不同，翻页
// 与落库逻辑必须完全一致，不该各写一份、悄悄漂移出两套行为。
//
// 单条订阅内任何一步失败（限速等待、REST 报错、写库失败）都会被 Warn 一次
// 并终止这条订阅的补洞，同时把这个 error 原样返回——是否需要理会这个返回值
// 由调用方按自己的语义决定：backfillOne 的批量场景选择丢弃（一条坏订阅不
// 该中断同批其余订阅），BackfillRange 的单订阅场景选择原样上抛（没有
// 「其余订阅」可继续，调用方需要知道这次补洞到底成没成）。之前的版本在这里
// 直接吞掉失败、恒定不返回错误，导致 BackfillRange 无论内部翻页失败与否
// 都只能返回 nil——backfill 子命令因此会在下游全程 500 或用户中途 Ctrl-C
// 时依然打印退出码 0，操作者据此误判「洞已经补上了」；这是一处真实的评审
// 发现（Important 1），不是风格调整。
func (s *Price) pullAndUpsert(ctx context.Context, ex string, impl exchange.Exchange, limiter *ratelimit.Bucket, sub exchange.Sub, start, end int64, logCtx []any) error {
	for {
		if err := limiter.Wait(ctx); err != nil {
			s.logger.Warn("补洞等待限速桶失败，跳过该订阅", append(logCtx, "err", err)...)
			return fmt.Errorf("等待限速桶: %w", err)
		}

		klines, next, err := impl.Klines(ctx, sub, start, end)
		if err != nil {
			s.logger.Warn("补洞拉取历史 K 线失败，跳过该订阅", append(logCtx, "err", err)...)
			return fmt.Errorf("拉取历史 K 线: %w", err)
		}

		if len(klines) > 0 {
			rows := make([]model.Kline, len(klines))
			for i, k := range klines {
				rows[i] = toBackfillModelKline(ex, k)
			}
			if err := s.deps.Klines.Upsert(ctx, rows); err != nil {
				s.logger.Warn("补洞写入 K 线失败，跳过该订阅", append(logCtx, "err", err)...)
				return fmt.Errorf("写入 K 线: %w", err)
			}
		}

		if next == 0 {
			return nil
		}
		start = next
	}
}

// BackfillRange 对单条订阅在调用方显式给定的 [start, end] 闭区间内补洞，供
// backfill 子命令使用（design.md §8：「按显式区间分页拉」）；与 Backfill
// （水位线追赶式，daemon 断线重连后的自动补洞用，起点查 MaxOpenTime、止点
// 恒为调用时刻）语义不同——这里完全不查库，起点与止点原样来自调用方，因此
// 可以对同一区间反复补（事故恢复、回填历史是典型场景，design.md §8）。区间
// 闭合方式与两个 adapter 的 Klines 实现对齐（start/end 都是包含边界，见
// binance/rest.go、okx/rest.go 各自对 startTime/endTime 或 after/before 的
// 注释），不是本方法自己另定的约定。
//
// sub.StreamType 非 kline 直接报错：ticker/depth 没有历史可补，子命令的
// --interval 参数本就只对 kline 有意义，误传其它类型是调用方参数错误，不是
// 可以静默跳过的「批量任务里一条坏订阅」——BackfillRange 一次只处理一条
// 订阅，没有「其余订阅」可继续。ex 未在 Exchanges/Limits 里配置同样报错，
// 两者都属装配错误。
//
// 订阅内部的失败（限速等待、REST 报错、写库失败）与 backfillOne 一样经
// pullAndUpsert 统一 Warn，但这里原样把 error 上抛给调用方——不能像
// backfillOne 那样丢弃：本方法一次只处理一条订阅，没有「其余订阅仍需继续」
// 这回事，调用方（backfill 子命令）需要靠这个返回值知道这次补洞到底成没成，
// 静默返回 nil 会让下游全程报错或用户中途 Ctrl-C 时命令依然打印退出码 0，
// 操作者据此误判「洞已经补上了」（评审 Important 1）。
func (s *Price) BackfillRange(ctx context.Context, ex string, sub exchange.Sub, start, end int64) error {
	if sub.StreamType != exchange.StreamKline {
		return fmt.Errorf("补洞: 订阅类型 %s 没有历史可补，仅支持 %s", sub.StreamType, exchange.StreamKline)
	}
	impl, ok := s.deps.Exchanges[ex]
	if !ok {
		return fmt.Errorf("补洞: 交易所 %s 未在 Exchanges 中配置", ex)
	}
	limiter, ok := s.deps.Limits[ex]
	if !ok {
		return fmt.Errorf("补洞: 交易所 %s 未配置限速桶", ex)
	}

	logCtx := []any{"exchange", ex, "market", sub.Market, "native_symbol", sub.NativeSymbol, "interval", sub.Interval}
	return s.pullAndUpsert(ctx, ex, impl, limiter, sub, start, end, logCtx)
}

// backfillStart 定补洞起点：库里已有该标的该周期的历史时，直接取
// MaxOpenTime 查到的上一根开盘时间本身（不做周期换算，理由见 backfillOne
// 注释）；库里从未有过时，回溯 Config.MaxBackfillWindow（零值兜底取
// defaultMaxBackfillWindow）——不能取 0，否则新标的第一次补洞会拉不到任何
// 历史。
func (s *Price) backfillStart(ctx context.Context, ex string, sub exchange.Sub) (int64, error) {
	last, has, err := s.deps.Klines.MaxOpenTime(ctx, ex, sub.Market, sub.NativeSymbol, sub.Interval)
	if err != nil {
		return 0, err
	}
	if has {
		return last, nil
	}

	window := s.cfg.MaxBackfillWindow
	if window <= 0 {
		window = defaultMaxBackfillWindow
	}
	return time.Now().Add(-window).UnixMilli(), nil
}

// toBackfillModelKline 把补洞任务从 REST 拉到的中立 K 线转换成落库用的表
// 模型：复用 toModelKline 做字段映射，只把 Source 改标记为补洞回填——与
// route.go 里实时流默认写入的 model.KlineSourceStream 区分来源，两条路径写
// 的是同一张表、同一主键，靠 Source 供排查时分辨数据从哪条路径落库。
func toBackfillModelKline(ex string, k exchange.Kline) model.Kline {
	m := toModelKline(ex, k)
	m.Source = model.KlineSourceBackfill
	return m
}

// OnReady 返回绑定了交易所名 ex 的 stream.OnReady 回调，可直接挂给
// stream.NewConn/stream.NewManager——ws 连接每次进入可用状态（首连、断线
// 重连、订阅集重建）都会触发一次，据此对这批订阅做一次补洞。ex 是装配期
// 已知的静态信息，靠闭包捕获，不塞进 stream.OnReady 的参数里——与 RouteFor
// 对称，理由见其注释。
//
// ctx 原样转手给 Backfill，不再替换成 context.Background()（必修 2）：
// stream.OnReady 的 ctx 是触发它的那条连接的 Run(ctx)，连接被取消时
// （Manager.Stop/Rebuild）会跟着取消——这条链路必须完整，否则一次涉及大量
// 翻页的补洞会不受停机信号约束，吃光 pkg/app.StopTimeout 这份全部组件共享的
// 停机预算，拖累排在后面的组件（细则见 stream/conn.go OnReady 类型注释）。
// 补洞被中途取消不会丢数据：起点是水位线（KlineRepo.MaxOpenTime），下次
// 触发（重连）会自动从断点续上，见 Backfill 的类型注释。Backfill 内部单条
// 订阅失败只 Warn、不中断整批（包括限速等待/REST 请求因 ctx 取消而失败的
// 情形），这里返回的 error 只可能是 ex 未在 Exchanges/Limits 里配置这类
// 装配错误，同样只记日志——回调类型本身没有返回值可用。
func (s *Price) OnReady(ex string) stream.OnReady {
	return func(ctx context.Context, subs []exchange.Sub) {
		if err := s.Backfill(ctx, ex, subs); err != nil {
			s.logger.Error("补洞失败", "exchange", ex, "err", err)
		}
	}
}
