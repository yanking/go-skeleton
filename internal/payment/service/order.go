package service

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	channelclient "github.com/yanking/go-skeleton/internal/payment/channel_client"
	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/repo"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

// CreateOrderIn 代收下单入参，字段对应 proto CreatePaymentOrderRequest 的业务字段
// （app_id/timestamp/sign 已在鉴权阶段消费，不出现在此）。
type CreateOrderIn struct {
	MchOrderNo  string
	Amount      int64 // 分
	Currency    string
	ChannelName string // 指定渠道名，空即按绑定优先级自动选路
	NotifyURL   string // 终态异步通知商户的地址，空即不通知
	PayerName   string
	PayerPhone  string
	PayerEmail  string
}

// CreateOrder 代收下单：先落单（status=已创建）再按静态绑定优先级逐实例派单，
// 首个下单成功的实例即为最终归属；先落单是为了杜绝"派单成功但从未落库"的孤儿单——
// 派单结果只需回填已存在的行，不必再处理落库失败。
func (s *Payment) CreateOrder(ctx context.Context, m *model.Merchant, in CreateOrderIn) (orderNo, payURL string, err error) {
	if in.MchOrderNo == "" || in.Amount <= 0 || in.Currency == "" {
		return "", "", errcode.ErrInvalidParameter
	}
	if in.Amount < m.LimitMin || in.Amount > m.LimitMax {
		return "", "", ErrAmountOutOfLimit
	}

	candidates, err := s.deps.Bindings.ListCandidates(ctx, m.ID, in.Currency)
	if err != nil {
		return "", "", errcode.Wrap(err, errcode.ErrInternal)
	}

	if in.ChannelName != "" {
		filtered := make([]model.ChannelInstance, 0, len(candidates))
		for _, c := range candidates {
			if c.ChannelName == in.ChannelName {
				filtered = append(filtered, c)
			}
		}
		candidates = filtered
		if len(candidates) == 0 {
			return "", "", ErrChannelNotBound
		}
	}

	usable := make([]model.ChannelInstance, 0, len(candidates))
	for _, c := range candidates {
		if in.Amount >= c.LimitPaymentMin && in.Amount <= c.LimitPaymentMax {
			usable = append(usable, c)
		}
	}
	if len(usable) == 0 {
		return "", "", ErrAmountOutOfLimit
	}

	orderNo, err = s.createOrderRow(ctx, m, in)
	if err != nil {
		return "", "", err
	}

	return s.dispatch(ctx, orderNo, in, usable)
}

// createOrderRow 落库一笔新订单；order_no 与 (merchant_id, mch_order_no) 共用同一个
// repo.ErrDuplicate 哨兵，无法从冲突本身区分撞的是哪个唯一键，故先按
// (merchant_id, mch_order_no) 反查：命中即真实的商户重复下单（50001）；未命中则说明
// 冲突来自 order_no 生成碰撞，换号重试一次（撞两次的概率可忽略，不再重试）。
func (s *Payment) createOrderRow(ctx context.Context, m *model.Merchant, in CreateOrderIn) (string, error) {
	order := &model.PaymentOrder{
		OrderNo:    newOrderNo(),
		MerchantID: m.ID,
		MchOrderNo: in.MchOrderNo,
		Amount:     in.Amount,
		Fee:        fee(in.Amount, m.FeeRate, m.FeeExtra),
		Currency:   in.Currency,
		Status:     model.OrderStatusCreated,
		NotifyURL:  in.NotifyURL,
	}

	err := s.deps.Orders.Create(ctx, order)
	if err == nil {
		return order.OrderNo, nil
	}
	if !errors.Is(err, repo.ErrDuplicate) {
		return "", errcode.Wrap(err, errcode.ErrInternal)
	}

	_, findErr := s.deps.Orders.FindForMerchant(ctx, m.ID, "", in.MchOrderNo)
	switch {
	case findErr == nil:
		return "", ErrDuplicateOrder
	case !errors.Is(findErr, repo.ErrRowNotFound):
		return "", errcode.Wrap(findErr, errcode.ErrInternal)
	}

	order.OrderNo = newOrderNo()
	if err := s.deps.Orders.Create(ctx, order); err != nil {
		return "", errcode.Wrap(err, errcode.ErrInternal)
	}
	return order.OrderNo, nil
}

// dispatch 按候选实例优先级逐个派单，首个成功即回填订单并返回；单个实例失败只记
// 日志与最后一次错误、继续尝试下一个，全部失败才判定整单下单失败。
func (s *Payment) dispatch(ctx context.Context, orderNo string, in CreateOrderIn, candidates []model.ChannelInstance) (string, string, error) {
	var lastErr error
	for _, c := range candidates {
		notifyURL := s.cfg.CallbackBaseURL + "/callbacks/payment/" + strconv.FormatInt(c.ID, 10)
		out, err := s.deps.Channel.CreateOrder(ctx, channelclient.Route{
			ChannelName: c.ChannelName,
			MerchantNo:  c.MerchantNo,
			Currency:    c.Currency,
		}, channelclient.OrderIn{
			OrderNo:    orderNo,
			Amount:     in.Amount,
			PayerName:  in.PayerName,
			PayerPhone: in.PayerPhone,
			PayerEmail: in.PayerEmail,
			NotifyURL:  notifyURL,
		})
		if err != nil {
			lastErr = err
			s.logger.Warn("渠道下单失败，尝试下一候选", "order", orderNo, "instance", c.ID, "channel", c.ChannelName, "err", err)
			continue
		}

		// bool 为 false 表示订单状态已被并发流程推进（如回调抢先到达），非本次调用的错误，
		// 支付链接依然是刚从渠道拿到的有效值，正常返回给商户。
		if _, err := s.deps.Orders.MarkSent(ctx, orderNo, c.ID, out.OutOrderNo, out.PayURL, out.Response); err != nil {
			// 渠道已受理下单（out_order_no/pay_url 均已生成），但本地落库失败——
			// 资金路径出现"渠道有单、本地无痕"的对账缺口，必须留痕定位；行为不变，仍判失败。
			s.logger.Error("渠道已受理但本地落库失败，需人工对账", "order", orderNo, "instance", c.ID, "out_order_no", out.OutOrderNo, "err", err)
			return "", "", errcode.Wrap(err, errcode.ErrInternal)
		}
		return orderNo, out.PayURL, nil
	}

	if _, err := s.deps.Orders.MarkFailedDispatch(ctx, orderNo); err != nil {
		s.logger.Error("标记订单下单失败异常", "order", orderNo, "err", err)
	}
	return "", "", errcode.Wrap(lastErr, ErrNoAvailableChannel)
}

// newOrderNo 生成平台订单号：毫秒时间戳 + 6 位随机数，足够低碰撞率；
// 万一撞上 uniq_order_no 由调用方（createOrderRow）重试一次。
func newOrderNo() string {
	return fmt.Sprintf("P%d%06d", time.Now().UnixMilli(), rand.IntN(1_000_000))
}

// fee 按千分位费率计算手续费，四舍五入到分：先乘费率再 +500 后整除 1000 等价于
// 四舍五入（+500 把 0.5‰ 的进位边界移到整除截断处），extra 为固定单笔加收。
func fee(amount int64, rate, extra int32) int64 {
	return (amount*int64(rate)+500)/1000 + int64(extra)
}
