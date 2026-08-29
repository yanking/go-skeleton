package service

import (
	"context"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
)

// reconcileOrderTypePayment 补单回推的订单类型：1 代收（对齐
// gatewayv1.TripartiteOrderCallbackRequest.order_type）。本服务当前只处理代收，
// 代付未接入，收到其他取值视为无需处理，见 ApplyReconcilePush。
const reconcileOrderTypePayment int32 = 1

// UnfinishedOrders 拉取指定渠道路由（channel_name/merchant_no/currency）下、窗口期内
// （当前时间 - periodMinutes 分钟起）创建的未完结订单，供 channel 侧补单对账 RPC
// TripartiteUnfinishedOrders 使用。route 查不到实例或查询本身失败均原样上抛，
// 由调用方（gateway handler）按 gRPC 协议翻译，本方法不做 errcode 转换。
func (s *Payment) UnfinishedOrders(ctx context.Context, channelName, merchantNo, currency string, periodMinutes int32) ([]model.PaymentOrder, error) {
	inst, err := s.deps.Instances.FindByRoute(ctx, channelName, merchantNo, currency)
	if err != nil {
		return nil, err
	}

	since := time.Now().Add(-time.Duration(periodMinutes) * time.Minute)
	return s.deps.Orders.ListUnfinished(ctx, inst.ID, since)
}

// ApplyReconcilePush 处理 channel 侧补单回推（对账拉单后确认的终态结果）：封装
// 「落 callbacks 表（source=对账回推）+ 调 ApplyChannelResult 收敛状态机」，
// 供 gateway handler TripartiteOrderCallback 调用。order_type 非代收（本服务当前
// 只支持代收，代付未接入）直接忽略、返回 nil，不当错误处理。
//
// 返回值语义对齐 ApplyChannelResult：仅当底层基础设施出错（route 查不到实例、落库
// 失败、订单不存在等）才返回非 nil error，供 handler 转 gRPC error 令 channel 下轮
// 重试；不可收敛（金额不符/矛盾态）与幂等重复均已在 ApplyChannelResult 内标无效/
// 幂等处理，此处一律视为处理成功、返回 nil（同 §8「不可收敛也返回成功」语义）。
func (s *Payment) ApplyReconcilePush(ctx context.Context, channelName, merchantNo, currency string, orderType int32, orderNo, outOrderNo string, amount int64, callbackType int32, referenceNo string) error {
	if orderType != reconcileOrderTypePayment {
		return nil
	}

	inst, err := s.deps.Instances.FindByRoute(ctx, channelName, merchantNo, currency)
	if err != nil {
		return err
	}

	cb := &model.Callback{
		ChannelInstanceID: inst.ID,
		Source:            model.CallbackSourceReconcile,
		OrderNo:           orderNo,
		Status:            model.CallbackStatusReceived,
	}
	if err := s.deps.Callbacks.Create(ctx, cb); err != nil {
		return err
	}

	_, err = s.ApplyChannelResult(ctx, ChannelResult{
		InstanceID:   inst.ID,
		OrderNo:      orderNo,
		OutOrderNo:   outOrderNo,
		CallbackType: callbackType,
		Amount:       amount,
		ReferenceNo:  referenceNo,
	})
	return err
}
