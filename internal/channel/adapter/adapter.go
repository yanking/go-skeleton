// Package adapter 是 channel 服务的三方渠道对接层：签名、参数拼装、响应解析、
// 状态码映射四件事不出本层。渠道差异在此收敛——每个渠道一个实现子包，
// service 经 Builders 登记表按 channel_name 构造实例，业务用例不感知渠道细节。
package adapter

import (
	"context"
	"encoding/json"
	"errors"
)

// Route 路由三元组，与 channels 表唯一约束一一对应，定位唯一渠道商户实例。
type Route struct {
	ChannelName string
	MerchantNo  string
	Currency    string
}

// General 渠道商户的通用元数据（channels 表非 platform 列的运行时形态），
// 对上游网关暴露做路由与限额；金额单位分，费率千分位。
type General struct {
	Route                  Route
	ChannelLevel           int32
	CallbackHeaders        []string
	CallbackDataSource     int32
	CallbackReturn         string
	CallbackIPWhitelist    string
	PayoutSupports         []int32
	LimitPaymentMin        int64
	LimitPaymentMax        int64
	LimitPayoutMin         int64
	LimitPayoutMax         int64
	PaymentCommissionRate  int32
	PaymentCommissionExtra int32
	PayoutCommissionRate   int32
	PayoutCommissionExtra  int32
}

// 对外订单状态（查询响应的 Status 取值），与原 enum.OutStatus 一致。
const (
	StatusProcessing int32 = iota + 1
	StatusSuccess
	StatusFailure
)

// 回调类型（CallbackOut.CallbackType 取值），与原 enum.CallbackType 一致。
const (
	CallbackSuccess int32 = iota + 1
	CallbackFailure
)

// 渠道适配的失败哨兵：service 据此翻译为业务 errcode，原始细节挂 cause 链。
var (
	// ErrChannelRejected 下游渠道不可用或业务拒绝（网络错、非 200、业务码失败、响应缺字段）。
	ErrChannelRejected = errors.New("下游渠道请求失败")
	// ErrVerifyFailed 回调验签不通过。
	ErrVerifyFailed = errors.New("回调验签失败")
	// ErrCallbackUnsupported 渠道不提供回调（状态只经查询轮询获知）。
	ErrCallbackUnsupported = errors.New("渠道不支持回调验签")
	// ErrUnknownCallbackStatus 回调报文携带无法识别的订单状态。
	ErrUnknownCallbackStatus = errors.New("回调状态未知")
	// ErrBadResponse 渠道响应无法解析为预期结构。
	ErrBadResponse = errors.New("渠道响应解析失败")
)

// PaymentOrderIn 代收下单入参（协议中立形态，handler 负责与 proto 互转）。
type PaymentOrderIn struct {
	OrderNo   string
	Amount    int64
	Name      string
	Phone     string
	Email     string
	NotifyURL string
	Deeplink  bool
	Timeout   int32
}

// PaymentOrderOut 代收下单结果；Response 保留渠道原始报文供网关侧排障留痕。
type PaymentOrderOut struct {
	URL               string
	OutOrderNo        string
	Response          string
	TripartiteAccount string
}

// PayoutOrderIn 代付下单入参。
type PayoutOrderIn struct {
	WayCode   int32
	OrderNo   string
	Amount    int64
	Name      string
	Phone     string
	Email     string
	BankName  string
	BankCode  string
	AccountNo string
	NotifyURL string
}

// PayoutOrderOut 代付下单结果。
type PayoutOrderOut struct {
	OutOrderNo        string
	Response          string
	TripartiteAccount string
}

// QueryIn 订单查询入参，order_no 与 out_order_no 至少其一。
type QueryIn struct {
	OrderNo    string
	OutOrderNo string
}

// QueryOut 订单查询结果，Status 取 Status* 常量。
type QueryOut struct {
	Status      int32
	Amount      int64
	OutOrderNo  string
	Response    string
	ReferenceNo string
}

// CallbackOut 回调验签结果，CallbackType 取 Callback* 常量。
type CallbackOut struct {
	OrderNo      string
	OutOrderNo   string
	CallbackType int32
	Amount       int64
	ReferenceNo  string
}

// BalanceOut 渠道商户余额。
type BalanceOut struct {
	Balance       int64
	FrozenBalance int64
}

// Adapter 渠道适配器：一个三方渠道一个实现包。构造函数收到的是该商户行的
// platform JSON 原文，由实现自行反序列化为自己的配置结构——结构因渠道而异，
// 框架不解释。回调验签的 header/data 由网关原样转发，实现内部完成验签与状态映射。
type Adapter interface {
	// Name 渠道名，与 Registry 键、channels.channel_name 一致（小写规范形式）。
	Name() string
	PaymentOrder(ctx context.Context, in PaymentOrderIn) (PaymentOrderOut, error)
	PayoutOrder(ctx context.Context, in PayoutOrderIn) (PayoutOrderOut, error)
	PaymentQuery(ctx context.Context, in QueryIn) (QueryOut, error)
	PayoutQuery(ctx context.Context, in QueryIn) (QueryOut, error)
	PaymentCallback(ctx context.Context, header map[string]string, data string) (CallbackOut, error)
	PayoutCallback(ctx context.Context, header map[string]string, data string) (CallbackOut, error)
	BalanceQuery(ctx context.Context) (BalanceOut, error)
}

// NewFunc 渠道适配器构造函数。
type NewFunc func(platform json.RawMessage) (Adapter, error)
