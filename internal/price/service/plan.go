package service

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// Plans 读取全部已启用订阅，按交易所分组后各自调用该交易所的 Plan 生成连接
// 计划，返回值以交易所名（Deps.Exchanges 的键）为键，供上层（Task 11 的重载
// job）逐个喂给对应的 stream.Manager.Rebuild。
//
// 返回值以 Deps.Exchanges 打底：每个已装配的交易所都会在返回值里有一个键，
// 哪怕这一轮它一条订阅都没有（值为空切片，不是缺键）——这是必修 3：job/
// reload.go 的 reloadOnce 只 for range 返回值遍历、逐个交易所调用
// Rebuild(ctx, plans[ex])，如果某交易所因为运维把它全部订阅置
// enabled=false 而彻底从返回值里消失，Rebuild 根本不会被调用，该交易所在管
// 的 ws 连接会一直收流、落库、触发补洞到进程重启为止——「减到零」与
// 「减到非零」在效果上必须一致，都要能触达 Rebuild 让它走清空连接的路径
// （Rebuild(ctx, nil) 经 normalizePlans 产出非 nil 空切片，与已生效的非空
// plans 比较不相等，正常走 stopAllLocked，见 manager.go）。
//
// 订阅表引用了配置里没有的交易所，只 Warn 并跳过那一家、不影响其余交易所——
// 一个配置漂移不该让全部交易所一起连不上，与 Backfill「单条订阅失败只 Warn
// 继续」同一个思路。某家交易所的 Plan 报错时，必须把它从返回值里删除（而不是
// 留一个空切片）：报错不代表「这一轮没有订阅」，若被当成空切片对待，
// reloadOnce 会据此把该交易所已经生效的连接全部清空——一次协议层面的报错
// 就会拆掉一批本来健康的连接，比什么都不做更糟；删除后 reloadOnce 找不到
// 这个键会直接跳过，保留现状，下一轮周期用同样的订阅重试。
func (s *Price) Plans(ctx context.Context) (map[string][]exchange.ConnPlan, error) {
	rows, err := s.deps.Subs.ListEnabled(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取启用中的订阅: %w", err)
	}
	if len(rows) == 0 {
		// 全新部署（订阅表为空）或全部订阅都被关停时，daemon 会一根 K 线都
		// 不采却看起来一切正常（进程正常运行、无错误日志）——留一条醒目的
		// Warn，否则这种情况只能靠人工去查表才能发现。
		s.logger.Warn("没有任何启用中的订阅")
	}

	grouped := make(map[string][]exchange.Sub)
	for _, row := range rows {
		grouped[row.Exchange] = append(grouped[row.Exchange], exchange.Sub{
			Market:       row.Market,
			NativeSymbol: row.NativeSymbol,
			StreamType:   row.StreamType,
			Interval:     row.Interval,
		})
	}

	out := make(map[string][]exchange.ConnPlan, len(s.deps.Exchanges))
	for ex := range s.deps.Exchanges {
		out[ex] = nil // 打底：即便这一轮零订阅，键也必须在场，理由见方法注释
	}

	for ex, subs := range grouped {
		impl, ok := s.deps.Exchanges[ex]
		if !ok {
			s.logger.Warn("订阅引用的交易所未在配置里启用，跳过", "exchange", ex)
			continue
		}
		plans, err := impl.Plan(subs)
		if err != nil {
			s.logger.Warn("生成连接计划失败，跳过该交易所", "exchange", ex, "err", err)
			delete(out, ex) // Plan 报错不等于「应清空」，不能覆盖成空切片，见方法注释
			continue
		}
		out[ex] = plans
	}
	return out, nil
}
