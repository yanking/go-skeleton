package service

import (
	"context"
	"errors"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/repo"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

// OrderView 查单结果，对外展示字段；CompletedAt 为毫秒时间戳，订单未完成时为 0。
type OrderView struct {
	OrderNo     string
	MchOrderNo  string
	Status      int32
	Amount      int64
	Fee         int64
	ReferenceNo string
	CompletedAt int64
}

// QueryOrder 按平台订单号或商户订单号查询商户自己的订单：两者皆空返回参数错误（10001）；
// 未命中或订单不属于该商户统一返回资源不存在（10002，不区分具体原因防跨商户探测，
// 与 repo.Order.FindForMerchant 的既有语义一致）。
func (s *Payment) QueryOrder(ctx context.Context, m *model.Merchant, orderNo, mchOrderNo string) (OrderView, error) {
	if orderNo == "" && mchOrderNo == "" {
		return OrderView{}, errcode.ErrInvalidParameter
	}

	o, err := s.deps.Orders.FindForMerchant(ctx, m.ID, orderNo, mchOrderNo)
	if err != nil {
		if errors.Is(err, repo.ErrRowNotFound) {
			return OrderView{}, errcode.ErrNotFound
		}
		return OrderView{}, errcode.Wrap(err, errcode.ErrInternal)
	}

	var completedAt int64
	if o.CompletedAt != nil {
		completedAt = o.CompletedAt.UnixMilli()
	}

	return OrderView{
		OrderNo:     o.OrderNo,
		MchOrderNo:  o.MchOrderNo,
		Status:      model.OutStatus(o.Status),
		Amount:      o.Amount,
		Fee:         o.Fee,
		ReferenceNo: o.ReferenceNo,
		CompletedAt: completedAt,
	}, nil
}

// ChannelView 可用渠道条目：同一 (channel_name, currency) 下多个候选实例的限额取并集
// （min 取最小、max 取最大），与 proto AvailableChannel 字段一一对应。
type ChannelView struct {
	ChannelName string
	Currency    string
	LimitMin    int64
	LimitMax    int64
}

// channelViewKey 是 AvailableChannels 聚合候选实例的分组键。
type channelViewKey struct {
	channelName string
	currency    string
}

// AvailableChannels 列出商户可用的渠道（currency 为空即返回全部币种，
// 对齐 ListAvailableChannelsRequest 的契约），按 (channel_name, currency) 聚合候选实例、
// 限额取区间并集；输出顺序取各分组在候选列表中首次出现的顺序
// （BindingRepo.ListCandidates 已按绑定优先级排序）。
func (s *Payment) AvailableChannels(ctx context.Context, m *model.Merchant, currency string) ([]ChannelView, error) {
	candidates, err := s.deps.Bindings.ListCandidates(ctx, m.ID, currency)
	if err != nil {
		return nil, errcode.Wrap(err, errcode.ErrInternal)
	}

	order := make([]channelViewKey, 0, len(candidates))
	groups := make(map[channelViewKey]*ChannelView, len(candidates))
	for _, c := range candidates {
		k := channelViewKey{c.ChannelName, c.Currency}
		if g, ok := groups[k]; ok {
			if c.LimitPaymentMin < g.LimitMin {
				g.LimitMin = c.LimitPaymentMin
			}
			if c.LimitPaymentMax > g.LimitMax {
				g.LimitMax = c.LimitPaymentMax
			}
			continue
		}
		groups[k] = &ChannelView{
			ChannelName: c.ChannelName, Currency: c.Currency,
			LimitMin: c.LimitPaymentMin, LimitMax: c.LimitPaymentMax,
		}
		order = append(order, k)
	}

	views := make([]ChannelView, 0, len(order))
	for _, k := range order {
		views = append(views, *groups[k])
	}
	return views, nil
}
