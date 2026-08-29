package handler

import (
	"context"

	gatewayv1 "github.com/yanking/go-skeleton/gen/gateway/v1"
	"github.com/yanking/go-skeleton/internal/payment/service"
)

// Gateway 是 gatewayv1.GatewayServiceServer 的实现：channel 服务补单对账入口
// （gateway-rpc 契约，东西向内部调用，无商户签名鉴权）。
type Gateway struct {
	gatewayv1.UnimplementedGatewayServiceServer
	svc *service.Payment
}

// NewGateway 构造 gateway 出口实现。
func NewGateway(svc *service.Payment) *Gateway {
	return &Gateway{svc: svc}
}

// TripartiteUnfinishedOrders 拉取指定渠道路由窗口期内的未完结代收单；本服务未接入
// 代付，payouts 恒空。
func (h *Gateway) TripartiteUnfinishedOrders(ctx context.Context, in *gatewayv1.TripartiteUnfinishedOrdersRequest) (*gatewayv1.TripartiteUnfinishedOrdersResponse, error) {
	rows, err := h.svc.UnfinishedOrders(ctx, in.GetChannelName(), in.GetMerchantNo(), in.GetCurrency(), in.GetPaymentPeriod())
	if err != nil {
		return nil, err
	}

	payments := make([]*gatewayv1.TripartiteUnfinishedOrdersItem, 0, len(rows))
	for _, o := range rows {
		payments = append(payments, &gatewayv1.TripartiteUnfinishedOrdersItem{
			OrderNo:    o.OrderNo,
			OutOrderNo: o.OutOrderNo,
			Amount:     o.Amount,
		})
	}
	return &gatewayv1.TripartiteUnfinishedOrdersResponse{Payments: payments}, nil
}

// TripartiteOrderCallback 处理 channel 侧补单回推：落回调留痕并推进状态机。
// 不可收敛（金额不符/矛盾态）与幂等重复均视为处理成功（转移表语义已在 service 内化），
// 仅基础设施错误才返回 gRPC error，供 channel 下轮重试。
func (h *Gateway) TripartiteOrderCallback(ctx context.Context, in *gatewayv1.TripartiteOrderCallbackRequest) (*gatewayv1.TripartiteOrderCallbackResponse, error) {
	err := h.svc.ApplyReconcilePush(ctx, in.GetChannelName(), in.GetMerchantNo(), in.GetCurrency(),
		in.GetOrderType(), in.GetOrderNo(), in.GetOutOrderNo(), in.GetAmount(), in.GetCallbackType(), in.GetReferenceNo())
	if err != nil {
		return nil, err
	}
	return &gatewayv1.TripartiteOrderCallbackResponse{}, nil
}
