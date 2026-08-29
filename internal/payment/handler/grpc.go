// Package handler 是 payment 的协议出口：薄壳——调 service、做出口转换，业务逻辑不在此写。
package handler

import (
	"context"

	paymentv1 "github.com/yanking/go-skeleton/gen/payment/v1"
	"github.com/yanking/go-skeleton/internal/payment/service"
	"github.com/yanking/go-skeleton/internal/payment/sign"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

// GRPC 是 paymentv1.PaymentServiceServer 的实现：商户面下单/查单/可用渠道查询，
// 每个 RPC 先按固定字段签名鉴权，再调 service、把出参转回 proto。
type GRPC struct {
	paymentv1.UnimplementedPaymentServiceServer
	svc *service.Payment
}

// NewGRPC 构造 gRPC 出口实现。
func NewGRPC(svc *service.Payment) *GRPC {
	return &GRPC{svc: svc}
}

// CreatePaymentOrder 创建代收订单。
func (h *GRPC) CreatePaymentOrder(ctx context.Context, in *paymentv1.CreatePaymentOrderRequest) (*paymentv1.CreatePaymentOrderResponse, error) {
	fields, sig, err := sign.FieldsFromProto(in)
	if err != nil {
		return nil, errcode.ErrInvalidParameter
	}
	m, err := h.svc.Authenticate(ctx, fields, sig)
	if err != nil {
		return nil, err
	}

	orderNo, payURL, err := h.svc.CreateOrder(ctx, m, service.CreateOrderIn{
		MchOrderNo:  in.GetMchOrderNo(),
		Amount:      in.GetAmount(),
		Currency:    in.GetCurrency(),
		ChannelName: in.GetChannelName(),
		NotifyURL:   in.GetNotifyUrl(),
		PayerName:   in.GetPayerName(),
		PayerPhone:  in.GetPayerPhone(),
		PayerEmail:  in.GetPayerEmail(),
	})
	if err != nil {
		return nil, err
	}
	return &paymentv1.CreatePaymentOrderResponse{OrderNo: orderNo, PayUrl: payURL}, nil
}

// QueryPaymentOrder 查询代收订单。
func (h *GRPC) QueryPaymentOrder(ctx context.Context, in *paymentv1.QueryPaymentOrderRequest) (*paymentv1.QueryPaymentOrderResponse, error) {
	fields, sig, err := sign.FieldsFromProto(in)
	if err != nil {
		return nil, errcode.ErrInvalidParameter
	}
	m, err := h.svc.Authenticate(ctx, fields, sig)
	if err != nil {
		return nil, err
	}

	view, err := h.svc.QueryOrder(ctx, m, in.GetOrderNo(), in.GetMchOrderNo())
	if err != nil {
		return nil, err
	}
	return &paymentv1.QueryPaymentOrderResponse{
		OrderNo:     view.OrderNo,
		MchOrderNo:  view.MchOrderNo,
		Status:      view.Status,
		Amount:      view.Amount,
		Fee:         view.Fee,
		ReferenceNo: view.ReferenceNo,
		CompletedAt: view.CompletedAt,
	}, nil
}

// ListAvailableChannels 查询可用渠道。
func (h *GRPC) ListAvailableChannels(ctx context.Context, in *paymentv1.ListAvailableChannelsRequest) (*paymentv1.ListAvailableChannelsResponse, error) {
	fields, sig, err := sign.FieldsFromProto(in)
	if err != nil {
		return nil, errcode.ErrInvalidParameter
	}
	m, err := h.svc.Authenticate(ctx, fields, sig)
	if err != nil {
		return nil, err
	}

	views, err := h.svc.AvailableChannels(ctx, m, in.GetCurrency())
	if err != nil {
		return nil, err
	}
	out := &paymentv1.ListAvailableChannelsResponse{Channels: make([]*paymentv1.AvailableChannel, 0, len(views))}
	for _, v := range views {
		out.Channels = append(out.Channels, &paymentv1.AvailableChannel{
			ChannelName: v.ChannelName,
			Currency:    v.Currency,
			LimitMin:    v.LimitMin,
			LimitMax:    v.LimitMax,
		})
	}
	return out, nil
}
