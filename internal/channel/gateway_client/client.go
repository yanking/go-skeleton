// Package gatewayclient 是 gateway-rpc 的直连客户端：补单 job 经此拉取网关侧
// 未完结订单并回推状态变更。契约镜像自 gateway-backend（api/gateway/v1）。
package gatewayclient

import (
	"context"
	"fmt"

	gatewayv1 "github.com/yanking/go-skeleton/gen/gateway/v1"
	"github.com/yanking/go-skeleton/internal/channel/adapter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client gateway-rpc 客户端。内网东西向直连，TLS 由 mesh/网络层保障。
type Client struct {
	cc   *grpc.ClientConn
	stub gatewayv1.GatewayServiceClient
}

// New 按 target（host:port）建连；建连失败当场报错（装配期暴露）。
func New(target string) (*Client, error) {
	cc, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("连接 gateway-rpc %s: %w", target, err)
	}
	return &Client{cc: cc, stub: gatewayv1.NewGatewayServiceClient(cc)}, nil
}

// Close 关闭底层连接。
func (c *Client) Close() error { return c.cc.Close() }

// UnfinishedOrder 网关侧一条未完结订单。
type UnfinishedOrder struct {
	OrderNo    string
	OutOrderNo string
	Amount     int64
}

// UnfinishedOrders 拉取指定渠道商户的未完结订单（payments 代收、payouts 代付）。
func (c *Client) UnfinishedOrders(ctx context.Context, route adapter.Route, period int32) (payments, payouts []UnfinishedOrder, err error) {
	reply, err := c.stub.TripartiteUnfinishedOrders(ctx, &gatewayv1.TripartiteUnfinishedOrdersRequest{
		ChannelName:   route.ChannelName,
		MerchantNo:    route.MerchantNo,
		Currency:      route.Currency,
		PaymentPeriod: period,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("拉取未完结订单: %w", err)
	}
	return toOrders(reply.GetPayments()), toOrders(reply.GetPayouts()), nil
}

// OrderCallback 回推一条订单状态变更。
func (c *Client) OrderCallback(ctx context.Context, route adapter.Route, orderType int32,
	orderNo, outOrderNo string, amount int64, callbackType int32, referenceNo string) error {
	_, err := c.stub.TripartiteOrderCallback(ctx, &gatewayv1.TripartiteOrderCallbackRequest{
		ChannelName:  route.ChannelName,
		MerchantNo:   route.MerchantNo,
		Currency:     route.Currency,
		OrderType:    orderType,
		OrderNo:      orderNo,
		OutOrderNo:   outOrderNo,
		Amount:       amount,
		CallbackType: callbackType,
		ReferenceNo:  referenceNo,
	})
	if err != nil {
		return fmt.Errorf("回推订单状态: %w", err)
	}
	return nil
}

func toOrders(items []*gatewayv1.TripartiteUnfinishedOrdersItem) []UnfinishedOrder {
	out := make([]UnfinishedOrder, 0, len(items))
	for _, item := range items {
		out = append(out, UnfinishedOrder{
			OrderNo:    item.GetOrderNo(),
			OutOrderNo: item.GetOutOrderNo(),
			Amount:     item.GetAmount(),
		})
	}
	return out
}
