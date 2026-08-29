// Package handler 是 channel 服务的协议出口：薄壳——调 service 并返回 errcode，
// proto 与适配器中立类型的互转在此完成，业务逻辑不在此写。
package handler

import (
	"context"

	channelv1 "github.com/yanking/go-skeleton/gen/channel/v1"
	"github.com/yanking/go-skeleton/internal/channel/adapter"
	"github.com/yanking/go-skeleton/internal/channel/service"
)

// GRPC 是 channelv1.ChannelServiceServer 的实现。
type GRPC struct {
	channelv1.UnimplementedChannelServiceServer
	svc *service.ChannelSvc
}

// NewGRPC 构造 gRPC 出口实现。
func NewGRPC(svc *service.ChannelSvc) *GRPC {
	return &GRPC{svc: svc}
}

// toRoute proto 路由三元组转中立形态。
func toRoute(pb *channelv1.Route) adapter.Route {
	return adapter.Route{
		ChannelName: pb.GetChannelName(),
		MerchantNo:  pb.GetMerchantNo(),
		Currency:    pb.GetCurrency(),
	}
}

// toInfo 元数据转 proto 形态。
func toInfo(g adapter.General) *channelv1.ChannelInfo {
	return &channelv1.ChannelInfo{
		ChannelName:            g.Route.ChannelName,
		MerchantNo:             g.Route.MerchantNo,
		Currency:               g.Route.Currency,
		ChannelLevel:           g.ChannelLevel,
		CallbackHeaders:        g.CallbackHeaders,
		CallbackDataSource:     g.CallbackDataSource,
		CallbackReturn:         g.CallbackReturn,
		CallbackIpWhitelist:    g.CallbackIPWhitelist,
		PayoutSupports:         g.PayoutSupports,
		LimitPaymentMin:        g.LimitPaymentMin,
		LimitPaymentMax:        g.LimitPaymentMax,
		LimitPayoutMin:         g.LimitPayoutMin,
		LimitPayoutMax:         g.LimitPayoutMax,
		PaymentCommissionRate:  g.PaymentCommissionRate,
		PaymentCommissionExtra: g.PaymentCommissionExtra,
		PayoutCommissionRate:   g.PayoutCommissionRate,
		PayoutCommissionExtra:  g.PayoutCommissionExtra,
	}
}

// ListChannels 全量渠道元数据。
func (h *GRPC) ListChannels(ctx context.Context, _ *channelv1.ListChannelsRequest) (*channelv1.ListChannelsResponse, error) {
	list := h.svc.ListChannels(ctx)
	out := &channelv1.ListChannelsResponse{Channels: make([]*channelv1.ChannelInfo, 0, len(list))}
	for _, g := range list {
		out.Channels = append(out.Channels, toInfo(g))
	}
	return out, nil
}

// PaymentOrder 代收下单。
func (h *GRPC) PaymentOrder(ctx context.Context, in *channelv1.PaymentOrderRequest) (*channelv1.PaymentOrderResponse, error) {
	out, err := h.svc.PaymentOrder(ctx, toRoute(in.GetRoute()), adapter.PaymentOrderIn{
		OrderNo:   in.GetOrderNo(),
		Amount:    in.GetAmount(),
		Name:      in.GetName(),
		Phone:     in.GetPhone(),
		Email:     in.GetEmail(),
		NotifyURL: in.GetNotifyUrl(),
		Deeplink:  in.GetDeeplink(),
		Timeout:   in.GetTimeout(),
	})
	if err != nil {
		return nil, err
	}
	return &channelv1.PaymentOrderResponse{
		Url:               out.URL,
		OutOrderNo:        out.OutOrderNo,
		Response:          out.Response,
		TripartiteAccount: out.TripartiteAccount,
	}, nil
}

// PayoutOrder 代付下单。
func (h *GRPC) PayoutOrder(ctx context.Context, in *channelv1.PayoutOrderRequest) (*channelv1.PayoutOrderResponse, error) {
	out, err := h.svc.PayoutOrder(ctx, toRoute(in.GetRoute()), adapter.PayoutOrderIn{
		WayCode:   in.GetWayCode(),
		OrderNo:   in.GetOrderNo(),
		Amount:    in.GetAmount(),
		Name:      in.GetName(),
		Phone:     in.GetPhone(),
		Email:     in.GetEmail(),
		BankName:  in.GetBankName(),
		BankCode:  in.GetBankCode(),
		AccountNo: in.GetAccountNo(),
		NotifyURL: in.GetNotifyUrl(),
	})
	if err != nil {
		return nil, err
	}
	return &channelv1.PayoutOrderResponse{
		OutOrderNo:        out.OutOrderNo,
		Response:          out.Response,
		TripartiteAccount: out.TripartiteAccount,
	}, nil
}

// PaymentQuery 代收查询。入出参与代付同构，buf lint 要求每 RPC 独立，
// 字段演进时须同步 PayoutQuery。
func (h *GRPC) PaymentQuery(ctx context.Context, in *channelv1.PaymentQueryRequest) (*channelv1.PaymentQueryResponse, error) {
	out, err := h.svc.PaymentQuery(ctx, toRoute(in.GetRoute()), adapter.QueryIn{
		OrderNo:    in.GetOrderNo(),
		OutOrderNo: in.GetOutOrderNo(),
	})
	if err != nil {
		return nil, err
	}
	return &channelv1.PaymentQueryResponse{
		Status:      out.Status,
		Amount:      out.Amount,
		OutOrderNo:  out.OutOrderNo,
		Response:    out.Response,
		ReferenceNo: out.ReferenceNo,
	}, nil
}

// PayoutQuery 代付查询。入出参与代收同构，buf lint 要求每 RPC 独立，
// 字段演进时须同步 PaymentQuery。
func (h *GRPC) PayoutQuery(ctx context.Context, in *channelv1.PayoutQueryRequest) (*channelv1.PayoutQueryResponse, error) {
	out, err := h.svc.PayoutQuery(ctx, toRoute(in.GetRoute()), adapter.QueryIn{
		OrderNo:    in.GetOrderNo(),
		OutOrderNo: in.GetOutOrderNo(),
	})
	if err != nil {
		return nil, err
	}
	return &channelv1.PayoutQueryResponse{
		Status:      out.Status,
		Amount:      out.Amount,
		OutOrderNo:  out.OutOrderNo,
		Response:    out.Response,
		ReferenceNo: out.ReferenceNo,
	}, nil
}

// PaymentCallback 代收回调验签。入出参与代付同构，buf lint 要求每 RPC 独立，
// 字段演进时须同步 PayoutCallback。
func (h *GRPC) PaymentCallback(ctx context.Context, in *channelv1.PaymentCallbackRequest) (*channelv1.PaymentCallbackResponse, error) {
	out, err := h.svc.PaymentCallback(ctx, toRoute(in.GetRoute()), in.GetHeader(), in.GetData())
	if err != nil {
		return nil, err
	}
	return &channelv1.PaymentCallbackResponse{
		OrderNo:      out.OrderNo,
		OutOrderNo:   out.OutOrderNo,
		CallbackType: out.CallbackType,
		Amount:       out.Amount,
		ReferenceNo:  out.ReferenceNo,
	}, nil
}

// PayoutCallback 代付回调验签。入出参与代收同构，buf lint 要求每 RPC 独立，
// 字段演进时须同步 PaymentCallback。
func (h *GRPC) PayoutCallback(ctx context.Context, in *channelv1.PayoutCallbackRequest) (*channelv1.PayoutCallbackResponse, error) {
	out, err := h.svc.PayoutCallback(ctx, toRoute(in.GetRoute()), in.GetHeader(), in.GetData())
	if err != nil {
		return nil, err
	}
	return &channelv1.PayoutCallbackResponse{
		OrderNo:      out.OrderNo,
		OutOrderNo:   out.OutOrderNo,
		CallbackType: out.CallbackType,
		Amount:       out.Amount,
		ReferenceNo:  out.ReferenceNo,
	}, nil
}

// BalanceQuery 商户余额查询。
func (h *GRPC) BalanceQuery(ctx context.Context, in *channelv1.BalanceQueryRequest) (*channelv1.BalanceQueryResponse, error) {
	out, err := h.svc.BalanceQuery(ctx, toRoute(in.GetRoute()))
	if err != nil {
		return nil, err
	}
	return &channelv1.BalanceQueryResponse{Balance: out.Balance, FrozenBalance: out.FrozenBalance}, nil
}
