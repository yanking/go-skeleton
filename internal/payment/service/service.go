// Package service 是 payment 的业务层：持有用例逻辑，声明自己依赖的仓储接口
// （依赖倒置支点，repo 包实现）。底层错误在此统一翻译为业务 errcode
// （errcode.Wrap 挂 cause），细则见 AGENTS.md「错误处理约定」。
package service

import (
	"context"
	"log/slog"
	"time"

	channelclient "github.com/yanking/go-skeleton/internal/payment/channel_client"
	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/pkg/queue"
)

// MerchantRepo 商户配置仓储接口，repo.Merchant 实现。
type MerchantRepo interface {
	// FindByAppID 按 app_id 查询商户配置；未命中返回 repo.ErrRowNotFound。
	FindByAppID(ctx context.Context, appID string) (*model.Merchant, error)
	// FindByID 按主键查询商户配置；未命中返回 repo.ErrRowNotFound。
	FindByID(ctx context.Context, id int64) (*model.Merchant, error)
}

// InstanceRepo 渠道实例仓储接口，repo.Instance 实现。
type InstanceRepo interface {
	// FindByID 按主键查询渠道实例；未命中返回 repo.ErrRowNotFound。
	FindByID(ctx context.Context, id int64) (*model.ChannelInstance, error)
	// ReplaceAll 用 channel 服务的最新快照全量覆盖本地渠道实例表。
	ReplaceAll(ctx context.Context, rows []model.ChannelInstance) error
	// FindByRoute 按 (channel_name, merchant_no, currency) 路由三元组查询渠道实例；
	// 未命中返回 repo.ErrRowNotFound。
	FindByRoute(ctx context.Context, channelName, merchantNo, currency string) (*model.ChannelInstance, error)
}

// BindingRepo 商户-渠道绑定仓储接口，repo.Binding 实现。
type BindingRepo interface {
	// ListCandidates 列出商户在指定币种下已绑定、可用的渠道实例，按优先级排序。
	ListCandidates(ctx context.Context, merchantID int64, currency string) ([]model.ChannelInstance, error)
}

// OrderRepo 支付订单仓储接口，repo.Order 实现。
type OrderRepo interface {
	// Create 落库一笔新订单；商户订单号重复返回 repo.ErrDuplicate。
	Create(ctx context.Context, order *model.PaymentOrder) error
	// FindByOrderNo 按平台订单号查询订单；未命中返回 repo.ErrRowNotFound。
	FindByOrderNo(ctx context.Context, orderNo string) (*model.PaymentOrder, error)
	// FindForMerchant 按平台订单号或商户订单号查询、并校验归属；未命中或不属于该商户
	// 一律返回 repo.ErrRowNotFound（防跨商户探测）。
	FindForMerchant(ctx context.Context, merchantID int64, orderNo, mchOrderNo string) (*model.PaymentOrder, error)
	// FindByOut 按渠道实例与渠道侧订单号查询订单；未命中返回 repo.ErrRowNotFound。
	FindByOut(ctx context.Context, instanceID int64, outOrderNo string) (*model.PaymentOrder, error)
	// MarkSent 把订单标记为已发送至渠道；bool 为 false 表示状态已被并发流程推进，非错误。
	MarkSent(ctx context.Context, orderNo string, instanceID int64, outOrderNo, payURL, response string) (bool, error)
	// MarkFailedDispatch 把订单标记为下单失败；bool 为 false 表示状态已被并发流程推进，非错误。
	MarkFailedDispatch(ctx context.Context, orderNo string) (bool, error)
	// Transition 行锁读取订单后交给 fn 决策：fn 返回 (nil, nil) 表示不落库，
	// 返回非 nil 订单则保存，fn 的 error 原样上抛（调用方已组装好的业务错误，不重新包装）。
	Transition(ctx context.Context, orderNo string, fn func(o *model.PaymentOrder) (*model.PaymentOrder, error)) error
	// ListUnfinished 列出某渠道实例下、指定时间之后创建的未完结订单（已创建/已发送）。
	ListUnfinished(ctx context.Context, instanceID int64, since time.Time) ([]model.PaymentOrder, error)
	// ListStaleCreated 列出 before 之前创建、仍停留在「已创建」的滞留订单，供兜底扫描收敛。
	ListStaleCreated(ctx context.Context, before time.Time) ([]model.PaymentOrder, error)
	// ListNotifyStuck 列出通知卡住的订单号：完成于 neverTriedBefore 之前却无任何通知记录，
	// 或最近一次通知尝试早于 lastTriedBefore。
	ListNotifyStuck(ctx context.Context, neverTriedBefore, lastTriedBefore time.Time) ([]string, error)
}

// CallbackRepo 渠道回调记录仓储接口，repo.Callback 实现。
type CallbackRepo interface {
	// Create 落库一条渠道回调原始记录。
	Create(ctx context.Context, cb *model.Callback) error
	// Mark 更新回调记录的处理状态与关联订单号。
	Mark(ctx context.Context, id int64, status int32, orderNo, note string) error
}

// NotificationRepo 商户通知记录仓储接口，repo.Notification 实现。
type NotificationRepo interface {
	// Create 落库一条商户通知发送记录。
	Create(ctx context.Context, n *model.OrderNotification) error
	// CountByOrder 统计某订单已发送过的通知次数，用于计算下一次的 attempt。
	CountByOrder(ctx context.Context, orderNo string) (int64, error)
}

// ChannelClient channel 服务客户端接口，channelclient.Client 实现。
type ChannelClient interface {
	// CreateOrder 代收下单，生成支付链接。
	CreateOrder(ctx context.Context, r channelclient.Route, in channelclient.OrderIn) (channelclient.OrderOut, error)
	// VerifyCallback 解析三方回调报文并验证签名。
	VerifyCallback(ctx context.Context, r channelclient.Route, header map[string]string, data string) (channelclient.CallbackOut, error)
	// ListInstances 拉取全量渠道商户实例元数据，供本地同步。
	ListInstances(ctx context.Context) ([]channelclient.Instance, error)
}

// Notifier 异步任务入队接口，queue.Client 实现。
type Notifier interface {
	Enqueue(ctx context.Context, typename string, payload []byte, opts ...queue.Option) error
}

// HTTPClient 出站 HTTP 客户端接口，pkg/httpc.Client 实现（PostJSON 签名逐字一致，
// 商户通知发送用它推送 JSON 报文）。
type HTTPClient interface {
	// PostJSON 发 JSON POST，返回 HTTP 状态码与响应体原文；只报网络/协议层错误，
	// 业务层失败（非 200、响应体不符）由调用方自行判定。
	PostJSON(ctx context.Context, url string, header map[string]string, body any, timeout time.Duration) (int, string, error)
}

// Config 业务层配置，装配期由 cmd/payment/initial 传入。
type Config struct {
	// CallbackBaseURL 拼装渠道回调地址的前缀（+ "/callbacks/payment/{instance_id}"）。
	CallbackBaseURL string
}

// Deps 用例依赖集合，全部为接口，装配期由 cmd/payment/initial 注入具体实现。
type Deps struct {
	Merchants     MerchantRepo
	Instances     InstanceRepo
	Bindings      BindingRepo
	Orders        OrderRepo
	Callbacks     CallbackRepo
	Notifications NotificationRepo
	Channel       ChannelClient
	Queue         Notifier
	HTTP          HTTPClient
}

// Payment payment 业务用例集，持有装配期配置、依赖与日志器。
type Payment struct {
	cfg    Config
	deps   Deps
	logger *slog.Logger
}

// New 构造用例集。
func New(cfg Config, deps Deps, logger *slog.Logger) *Payment {
	return &Payment{cfg: cfg, deps: deps, logger: logger}
}
