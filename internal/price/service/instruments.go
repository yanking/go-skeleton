package service

import (
	"context"
	"fmt"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
)

// spotMarket 本服务本期只做现货（见 configs/price.yaml 顶部注释）。
// exchange.Exchange.Instruments 的 market 参数只回填到返回值的 Market
// 字段（实际请求哪个市场由各 adapter 自己的 REST 路径决定，见
// binance/rest.go、okx/rest.go），这里传什么就贴什么标签，与两个 adapter
// 包内部同名常量（market = "spot"）表达同一件事。
const spotMarket = "spot"

// ImportInstruments 全量同步 ex 交易所的现货交易对：拉取 → 写入
// InstrumentRepo → 把本轮不再返回的标的标记为已下架。不删行是因为历史 K
// 线仍按 (exchange, market, native_symbol) 引用着这些标的，删行会让已落库
// 的历史数据失去可查的标的元信息；标记下架保留了这层引用。
//
// 拉取失败或写库失败都直接上抛、不再继续——不能拿一份不完整或没落地的名单
// 去判断"哪些标的这轮没出现"，那样会把还没来得及覆盖的旧标的误标下架。
func (s *Price) ImportInstruments(ctx context.Context, ex string) error {
	impl, ok := s.deps.Exchanges[ex]
	if !ok {
		return fmt.Errorf("导入交易对: 交易所 %s 未在 Exchanges 中配置", ex)
	}

	instruments, err := impl.Instruments(ctx, spotMarket)
	if err != nil {
		return fmt.Errorf("导入交易对: 拉取 %s 全量交易对: %w", ex, err)
	}

	rows := make([]model.Instrument, len(instruments))
	keep := make([]string, len(instruments))
	for i, ins := range instruments {
		rows[i] = toModelInstrument(ex, ins)
		keep[i] = ins.NativeSymbol
	}

	if err := s.deps.Instruments.UpsertAll(ctx, rows); err != nil {
		return fmt.Errorf("导入交易对: 写入标的: %w", err)
	}

	// keep 为空（本轮一个标的都没拉到）时 MarkDelistedExcept 会把该交易所
	// 下全部标的标为下架，是「交易所整体下线」场景下的预期行为，见其注释。
	if err := s.deps.Instruments.MarkDelistedExcept(ctx, ex, spotMarket, keep); err != nil {
		return fmt.Errorf("导入交易对: 标记下架标的: %w", err)
	}
	return nil
}

// toModelInstrument 把交易所返回的中立交易对转换成落库用的表模型；Trading
// 映射到 Status，UpdatedAt 显式盖时间戳，与 route.go 的 toModelKline 同一个
// 约定。CreatedAt 留零值不设——交给数据库列默认值，仅在首次插入时生效
// （见 repo.Instrument.UpsertAll 的冲突覆盖列不含 created_at）。
func toModelInstrument(ex string, ins exchange.Instrument) model.Instrument {
	status := int32(model.InstrumentStatusTrading)
	if !ins.Trading {
		status = int32(model.InstrumentStatusDelisted)
	}
	return model.Instrument{
		Exchange:     ex,
		Market:       ins.Market,
		NativeSymbol: ins.NativeSymbol,
		Symbol:       ins.Symbol,
		Base:         ins.Base,
		Quote:        ins.Quote,
		Status:       status,
		UpdatedAt:    time.Now(),
	}
}
