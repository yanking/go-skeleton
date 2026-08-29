package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
	"github.com/yanking/go-skeleton/internal/price/stream"
)

const (
	// klineBatchSize、klineBatchInterval 是 kline 落库的攒批上限：凑够
	// klineBatchSize 条或每隔 klineBatchInterval 未攒满都会触发一次
	// Upsert——避免收线稀疏的时段迟迟不落盘，也避免逐条写造成过多往返。
	klineBatchSize     = 100
	klineBatchInterval = time.Second

	// klineRetryInterval 是批量写库失败后的重试间隔，见 flush 的注释。
	klineRetryInterval = time.Second

	// shutdownFlushTimeout 是收到停机信号后，为把还攒在内存里的最后一批
	// kline 尽力落盘所留的时间上限。必须远小于 pkg/app 的 StopTimeout——那是
	// 全部组件共享的停机预算（默认 30s，不是每个组件各 30s），这里的独立
	// 超时如果调得太大，会连累按注册逆序停止的后续组件挤不出时间；调这个值
	// 之前先确认没有挤占别的组件的停机预算。
	shutdownFlushTimeout = 5 * time.Second
)

// snapshotItem 是 snapshot 队列里的元素：Route 收到事件的那一刻就补上交易所
// 名与本地接收时间，不能等到 RunWriters 真正消费时才盖时间戳——排队等待的
// 时长会把“本地接收时间”这个陈旧度基准悄悄往后推，背压越严重、失真越大。
type snapshotItem struct {
	exchange string // 交易所名（binance/okx 等），来自 RouteFor 的入参
	snapshot exchange.Snapshot
	recvTime time.Time
}

// RouteFor 返回绑定了交易所名 ex 的事件路由入口，直接可作为
// stream.NewManager 的 Handler 参数——每家交易所在装配层各建一个
// stream.Manager（见 manager.go 包注释），ex 由那里的调用点决定，是装配期
// 就已知的静态信息，不必也不该塞进每一帧 exchange.Event 里增加运行时开销
// （exchange.Event 是 binance/okx 两个 adapter 共享的契约，改动会波及已完工
// 的交易所包）。与 Task 10 的 OnReady(ex string) stream.OnReady 对称。
//
// 这是本包路由事件的唯一公开入口：不导出的 route 只接受经此工厂绑定过交易所
// 名的调用，装配层没有第二个「忘了填交易所」也能编译通过的入口可用错。
func (s *Price) RouteFor(ex string) stream.Handler {
	return func(ev exchange.Event) {
		s.route(ex, ev)
	}
}

// route 把一帧解码结果按数据语义分流入队，两条流的背压策略完全不同：
//
//   - kline：队列满时阻塞在这里，直到 RunWriters 腾出位置。收线帧丢一帧就是
//     一个洞，洞只能靠 REST 补洞找回；宁可让上游 ws 读循环变慢，也不能丢。
//   - ticker/depth：队列满时丢弃队列里最旧的一帧，把新帧换进去。它们是快照，
//     语义上只有最新一帧有意义，下一帧很快就到；为一帧过期快照阻塞住同一条
//     连接上的 kline，是拿不可恢复的数据换可恢复的数据，得不偿失。
//
// 两个队列各自独立（Price.klineCh/snapCh），不按连接分——否则 depth 的洪流
// 会连带把同一条连接上的 kline 拖住。
//
// Event 至多一个指针非 nil（见 exchange.Event 类型注释），全 nil 的帧（心跳
// 应答、订阅确认、未收线的 K 线）不入队。ex 是调用方（RouteFor 的闭包）绑定
// 的交易所名，落库/写 Redis 时用于填充交易所维度。
func (s *Price) route(ex string, ev exchange.Event) {
	switch {
	case ev.Kline != nil:
		s.klineCh <- toModelKline(ex, *ev.Kline)
	case ev.Snapshot != nil:
		item := snapshotItem{exchange: ex, snapshot: *ev.Snapshot, recvTime: time.Now()}
		select {
		case s.snapCh <- item:
		default:
			// 队列满：先丢一个最旧的腾位置，再把新帧放进去（按 Step 3 的
			// 要求，先 <-ch 丢一个再发）。这两步不是原子的，且不止一处并发
			// 来源：同一交易所按 MaxStreamsPerConn 会切成多条 ws 连接，每条
			// 连接一个 goroutine，都调用同一个 RouteFor(ex) 闭包并发写同一个
			// snapCh——多个 goroutine 可能同时撞进这个 default 分支，各自
			// 排空一个再发，连续丢出的可能不止一帧，不是精确的“一进一出”。
			// 这在语义上无害：丢的都是已经排到队里、注定被更新帧顶替的陈旧
			// 快照，谁先被排空、一次丢几帧都不影响“只留最新几帧”这条背压
			// 语义，不必加锁强求严格的一对一顺序。
			select {
			case <-s.snapCh:
			default:
			}
			s.snapCh <- item
		}
	}
}

// toModelKline 把中立 K 线转换成落库用的表模型；Source 固定标记为实时流，
// 与补洞任务（Task 10，标记 model.KlineSourceBackfill）区分来源。Exchange
// 由调用方（route，经 RouteFor 绑定）显式传入——exchange.Kline 本身不携带
// 交易所身份，两个 adapter 包都把 Market 字段填成子市场常量 "spot"（见
// internal/price/exchange/binance/binance.go、internal/price/exchange/okx/
// okx.go 顶部的 market 常量注释），不是交易所名，与这里需要的维度是两回事。
func toModelKline(ex string, k exchange.Kline) model.Kline {
	return model.Kline{
		Exchange:     ex,
		Market:       k.Market,
		NativeSymbol: k.NativeSymbol,
		Interval:     k.Interval,
		OpenTime:     k.OpenTime,
		Open:         k.Open,
		High:         k.High,
		Low:          k.Low,
		Close:        k.Close,
		Volume:       k.Volume,
		QuoteVolume:  k.QuoteVolume,
		Source:       model.KlineSourceStream,
		UpdatedAt:    time.Now(),
	}
}

// latestPayload 是写入 Redis 的最终 JSON 形状：在 adapter 给的交易所事件
// 时间、归一化报文主体之外，补上本地接收时间——两个时间戳都带，且不设 TTL
// （理由见 repo.Latest.Set 与 design.md §6：设了 TTL，断流后 key 消失，消费方
// 看到的是“没有这个标的”，是误导；不设 TTL，消费方看到的是“有，但数据是
// N 分钟前的”，后者才是事实——前者该报配置错误，后者该报采集故障，两种情况
// 消费方的处置完全不同，陈旧判定的阈值属于消费方策略，不该由采集方用 TTL
// 替它决定。不要因为“看起来像忘了设过期时间”而顺手加上）。
type latestPayload struct {
	EventTime int64           `json:"event_time"` // 交易所事件时间，UTC 毫秒；交易所报文不带时间戳则为 0
	RecvTime  int64           `json:"recv_time"`  // 本地接收时间，UTC 毫秒——即 route 收到这一帧的时刻
	Payload   json.RawMessage `json:"payload"`    // 归一化后的报文主体，形状见 exchange.Snapshot 类型注释
}

// buildLatestPayload 组装写 Redis 用的 key 与最终 JSON 值。
func buildLatestPayload(item snapshotItem) (key string, payload []byte, err error) {
	key = latestKey(item.exchange, item.snapshot.Market, item.snapshot.NativeSymbol, item.snapshot.StreamType)
	out := latestPayload{
		EventTime: item.snapshot.EventTime,
		RecvTime:  item.recvTime.UnixMilli(),
		Payload:   item.snapshot.Payload,
	}
	payload, err = json.Marshal(out)
	if err != nil {
		return "", nil, fmt.Errorf("序列化最新行情: %w", err)
	}
	return key, payload, nil
}

// latestKey 拼出 repo.Latest.Set 要求的 key 形状
// price:{exchange}:{market}:{symbol}:{stream}。ex 来自 RouteFor 绑定的交易所
// 名（经 snapshotItem.exchange 带到这里）。symbol 段用 NativeSymbol：归一化
// 符号（model.Instrument.Symbol）需要按 (exchange, market, native_symbol) 反查
// price_instruments，但 repo.Instrument 目前只有 UpsertAll/MarkDelistedExcept
// 两个方法、没有单行查询，本层不能为了凑这个另起一个 repo 方法签名（见
// AGENTS.md「仓储接口方法集以 repo 实际签名为唯一来源」）。
func latestKey(ex, market, symbol, stream string) string {
	return fmt.Sprintf("price:%s:%s:%s:%s", ex, market, symbol, stream)
}

// RunWriters 消费 kline 与 snapshot 两个队列，分别批量落库与写 Redis，直到
// ctx 取消。两条流各起一个 goroutine、互不阻塞——Redis 不可达时 ticker/depth
// 链路的重试/等待不会拖慢 kline 的批量落库，反之亦然（design.md §7 故障矩阵：
// 两条链路的故障域是分开的）。
func (s *Price) RunWriters(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.runKlineWriter(ctx)
	}()
	go func() {
		defer wg.Done()
		s.runSnapshotWriter(ctx)
	}()
	wg.Wait()
	return nil
}

// runKlineWriter 攒批消费 kline 队列，凑够 klineBatchSize 条或每隔
// klineBatchInterval 未攒满都触发一次落库。
func (s *Price) runKlineWriter(ctx context.Context) {
	batch := make([]model.Kline, 0, klineBatchSize)
	timer := time.NewTimer(klineBatchInterval)
	defer timer.Stop()

	// flush 把当前批次用 flushCtx 写库；失败就原地重试直到成功或 flushCtx
	// 取消，绝不清空 batch，也绝不在重试期间去读 s.klineCh 收新数据。kline
	// 不可丢是整个采集侧最硬的一条约束：重试期间还继续排空 klineCh，只会把
	// “写不进去”这个故障用“内存无限膨胀”的方式悄悄掩盖掉；正确做法是让
	// klineCh 憋满，背压沿着 route 一路传导回 ws 读循环——这是刻意的代价
	// （design.md §7 故障矩阵：「PG 不可达 → kline 写入重试，队列阻塞」）。
	//
	// flushCtx 由调用方传入而不是直接闭包捕获外层 ctx：正常路径（攒够
	// klineBatchSize 或定时器触发）用的是 runKlineWriter 收到的 ctx，此时
	// 它还没取消；但停机路径（ctx.Done() 触发的最后一次 flush）必须换一个
	// 还没取消的 ctx——GORM 的 WithContext 底层走 database/sql，对已 Done
	// 的 ctx 在真正发请求之前就直接短路返回 context canceled，根本不会碰
	// 数据库。曾经这里直接复用外层已取消的 ctx，“尽力落盘”在实践中是空转：
	// 在途这批 kline 被无声丢弃，日志却打着“重试中”——这条错误已经在评审里
	// 被抓到过，不要再犯。
	flush := func(flushCtx context.Context) {
		if len(batch) == 0 {
			return
		}
		for {
			if err := s.deps.Klines.Upsert(flushCtx, batch); err != nil {
				s.logger.Error("批量写入 K 线失败，重试中", "count", len(batch), "err", err)
				select {
				case <-flushCtx.Done():
					return
				case <-time.After(klineRetryInterval):
					continue
				}
			}
			batch = batch[:0]
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			// 停机前先把还排在 klineCh 里、尚未进 batch 的数据也一并收进来
			// ——装配层按约定顺序停机（先停 ws 连接、再停这里），此时不应
			// 再有新事件写入，非阻塞排空不会无限等待。
		drainRemaining:
			for {
				select {
				case k := <-s.klineCh:
					batch = append(batch, k)
				default:
					break drainRemaining
				}
			}
			// 用从 context.Background() 派生、带独立超时的 ctx 做最后一次
			// flush，不能复用上面已经 Done 的 ctx（原因见 flush 的注释）。
			// 重试循环的退出条件也随之变成这个新 ctx 的 Done，所以最多重试
			// shutdownFlushTimeout 这么久，不会在停机窗口里死等成功。
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
			flush(shutdownCtx)
			cancel()
			return
		case k := <-s.klineCh:
			batch = append(batch, k)
			if len(batch) >= klineBatchSize {
				if !timer.Stop() {
					<-timer.C
				}
				flush(ctx)
				timer.Reset(klineBatchInterval)
			}
		case <-timer.C:
			flush(ctx)
			timer.Reset(klineBatchInterval)
		}
	}
}

// runSnapshotWriter 逐条消费 snapshot 队列并写 Redis，不攒批——它们是快照，
// 攒批只会让消费方看到更旧的值，与“只留最新一帧”的语义相反。
func (s *Price) runSnapshotWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-s.snapCh:
			key, payload, err := buildLatestPayload(item)
			if err != nil {
				s.logger.Error("组装最新行情失败，丢弃该帧",
					"exchange", item.exchange, "market", item.snapshot.Market, "native_symbol", item.snapshot.NativeSymbol,
					"stream_type", item.snapshot.StreamType, "err", err)
				continue
			}
			if err := s.deps.Latest.Set(ctx, key, payload); err != nil {
				// Redis 不可达：直接丢弃并只记日志，不重试——ticker/depth 是
				// 快照，下一帧很快就到；为一帧陈旧数据重试会拖慢同队列后面
				// 的新帧，得不偿失（design.md §7 故障矩阵：「Redis 不可达 →
				// ticker/depth 直接丢弃并计数」，日志即计数口径）。
				s.logger.Warn("写入最新行情失败，丢弃该帧", "key", key, "err", err)
			}
		}
	}
}
