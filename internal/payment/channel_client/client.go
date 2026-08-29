// Package channelclient 是 channel 服务的直连客户端：payment 服务调用 channel
// 服务完成下单、回调验签、拉实例元数据等操作。协议中立层，把 pb 类型转换成
// 服务内部的中立类型，pb 类型不出本层。
package channelclient

import (
	"context"
	"fmt"

	channelv1 "github.com/yanking/go-skeleton/gen/channel/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client channel 服务客户端。内网东西向直连，TLS 由 mesh/网络层保障。
type Client struct {
	cc   *grpc.ClientConn
	stub channelv1.ChannelServiceClient
}

// New 按 target（host:port）建连；建连失败当场报错（装配期暴露）。
func New(target string) (*Client, error) {
	cc, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("连接 channel 服务 %s: %w", target, err)
	}
	return &Client{cc: cc, stub: channelv1.NewChannelServiceClient(cc)}, nil
}

// Close 关闭底层连接。
func (c *Client) Close() error {
	return c.cc.Close()
}

// Route 路由三元组：定位渠道商户实例。
type Route struct {
	ChannelName string
	MerchantNo  string
	Currency    string
}

// OrderIn 下单入参。
type OrderIn struct {
	OrderNo    string
	Amount     int64
	PayerName  string
	PayerPhone string
	PayerEmail string
	NotifyURL  string
}

// OrderOut 下单出参。
type OrderOut struct {
	PayURL     string
	OutOrderNo string
	Response   string
}

// CreateOrder 代收下单：生成支付链接。
func (c *Client) CreateOrder(ctx context.Context, r Route, in OrderIn) (OrderOut, error) {
	reply, err := c.stub.PaymentOrder(ctx, &channelv1.PaymentOrderRequest{
		Route: &channelv1.Route{
			ChannelName: r.ChannelName,
			MerchantNo:  r.MerchantNo,
			Currency:    r.Currency,
		},
		OrderNo:   in.OrderNo,
		Amount:    in.Amount,
		Name:      in.PayerName,
		Phone:     in.PayerPhone,
		Email:     in.PayerEmail,
		NotifyUrl: in.NotifyURL,
		// 本轮不暴露 deeplink 与 timeout，按零值传给渠道
		Deeplink: false,
		Timeout:  0,
	})
	if err != nil {
		return OrderOut{}, fmt.Errorf("下单: %w", err)
	}
	return OrderOut{
		PayURL:     reply.GetUrl(),
		OutOrderNo: reply.GetOutOrderNo(),
		Response:   reply.GetResponse(),
	}, nil
}

// CallbackOut 回调验签出参。
type CallbackOut struct {
	OrderNo      string
	OutOrderNo   string
	CallbackType int32
	Amount       int64
	ReferenceNo  string
}

// VerifyCallback 回调验签：解析三方回调报文、验证签名。
func (c *Client) VerifyCallback(ctx context.Context, r Route, header map[string]string, data string) (CallbackOut, error) {
	reply, err := c.stub.PaymentCallback(ctx, &channelv1.PaymentCallbackRequest{
		Route: &channelv1.Route{
			ChannelName: r.ChannelName,
			MerchantNo:  r.MerchantNo,
			Currency:    r.Currency,
		},
		Header: header,
		Data:   data,
	})
	if err != nil {
		return CallbackOut{}, fmt.Errorf("验签回调: %w", err)
	}
	return CallbackOut{
		OrderNo:      reply.GetOrderNo(),
		OutOrderNo:   reply.GetOutOrderNo(),
		CallbackType: reply.GetCallbackType(),
		Amount:       reply.GetAmount(),
		ReferenceNo:  reply.GetReferenceNo(),
	}, nil
}

// Instance 渠道商户实例的元数据。
type Instance struct {
	ChannelName         string
	MerchantNo          string
	Currency            string
	LimitPaymentMin     int64
	LimitPaymentMax     int64
	CallbackHeaders     []string
	CallbackDataSource  int32
	CallbackReturn      string
	CallbackIPWhitelist string
}

// ListInstances 拉取全量渠道商户实例元数据。
func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	reply, err := c.stub.ListChannels(ctx, &channelv1.ListChannelsRequest{})
	if err != nil {
		return nil, fmt.Errorf("拉取渠道实例: %w", err)
	}
	instances := make([]Instance, 0, len(reply.GetChannels()))
	for _, ch := range reply.GetChannels() {
		instances = append(instances, Instance{
			ChannelName:         ch.GetChannelName(),
			MerchantNo:          ch.GetMerchantNo(),
			Currency:            ch.GetCurrency(),
			LimitPaymentMin:     ch.GetLimitPaymentMin(),
			LimitPaymentMax:     ch.GetLimitPaymentMax(),
			CallbackHeaders:     ch.GetCallbackHeaders(),
			CallbackDataSource:  ch.GetCallbackDataSource(),
			CallbackReturn:      ch.GetCallbackReturn(),
			CallbackIPWhitelist: ch.GetCallbackIpWhitelist(),
		})
	}
	return instances, nil
}
