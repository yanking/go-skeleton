// Package job 是 channel 服务的异步任务层。Reconcile 实现补单对账：
// 渠道不回调或回调不可靠时（neokred 形态），按商户 reconcile_enabled 开启，
// 定期拉取网关侧未完结订单、查渠道真实状态、金额一致才回推。
package job

import (
	"context"
	"log/slog"
	"time"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
	"github.com/yanking/go-skeleton/internal/channel/gateway_client"
)

// interval 轮询周期，与原 neokred 实现一致；拉单窗口期 30 分钟。
const (
	interval      = 15 * time.Second
	paymentPeriod = 30
)

// QueryService job 所需的渠道查询能力，service 实现。
type QueryService interface {
	ReconcileRoutes(ctx context.Context) []adapter.Route
	PaymentQuery(ctx context.Context, route adapter.Route, in adapter.QueryIn) (adapter.QueryOut, error)
	PayoutQuery(ctx context.Context, route adapter.Route, in adapter.QueryIn) (adapter.QueryOut, error)
}

// Gateway job 所需的网关对账能力，gatewayclient 实现。
type Gateway interface {
	UnfinishedOrders(ctx context.Context, route adapter.Route, period int32) (payments, payouts []gatewayclient.UnfinishedOrder, err error)
	OrderCallback(ctx context.Context, route adapter.Route, orderType int32,
		orderNo, outOrderNo string, amount int64, callbackType int32, referenceNo string) error
}

// 订单与回调类型常量，与 gateway 契约一致。
const (
	orderTypePayment = 1
	orderTypePayout  = 2
)

// Reconcile 补单对账组件。
type Reconcile struct {
	svc    QueryService
	gw     Gateway
	logger *slog.Logger
}

// New 构造对账组件；svc 与 gw 由装配层注入。
func New(svc QueryService, gw Gateway, logger *slog.Logger) *Reconcile {
	return &Reconcile{svc: svc, gw: gw, logger: logger}
}

// Name 组件名。
func (r *Reconcile) Name() string { return "reconcile" }

// Start 轮询直到 ctx 取消。首轮立即执行，随后按 interval 周期。
func (r *Reconcile) Start(ctx context.Context) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// 单轮出错只记日志，下一轮重试；对账是兜底通道，不该放大为服务故障。
		if err := r.round(ctx); err != nil {
			r.logger.Warn("补单对账本轮失败", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Stop 对账循环随 ctx 退出，无须额外动作。
func (r *Reconcile) Stop(context.Context) error { return nil }

// round 一轮对账：遍历开启补单的路由，逐单核对回推。
func (r *Reconcile) round(ctx context.Context) error {
	for _, route := range r.svc.ReconcileRoutes(ctx) {
		payments, payouts, err := r.gw.UnfinishedOrders(ctx, route, paymentPeriod)
		if err != nil {
			r.logger.Warn("拉取未完结订单失败", "route", route.ChannelName+"/"+route.MerchantNo, "err", err)
			continue
		}
		for _, item := range payments {
			r.reconcileOne(ctx, route, orderTypePayment, item)
		}
		for _, item := range payouts {
			r.reconcileOne(ctx, route, orderTypePayout, item)
		}
	}
	return nil
}

// reconcileOne 核对一单：渠道状态离开处理中且金额与网关记录一致才回推。
func (r *Reconcile) reconcileOne(ctx context.Context, route adapter.Route, orderType int32, item gatewayclient.UnfinishedOrder) {
	var (
		out adapter.QueryOut
		err error
	)
	query := adapter.QueryIn{OrderNo: item.OrderNo, OutOrderNo: item.OutOrderNo}
	if orderType == orderTypePayment {
		out, err = r.svc.PaymentQuery(ctx, route, query)
	} else {
		out, err = r.svc.PayoutQuery(ctx, route, query)
	}
	if err != nil {
		r.logger.Warn("对账查询失败", "order", item.OrderNo, "err", err)
		return
	}

	if out.Amount != item.Amount || out.Status == adapter.StatusProcessing {
		return
	}

	var callbackType int32
	switch out.Status {
	case adapter.StatusSuccess:
		callbackType = adapter.CallbackSuccess
	case adapter.StatusFailure:
		callbackType = adapter.CallbackFailure
	default:
		return
	}

	if err := r.gw.OrderCallback(ctx, route, orderType, item.OrderNo, out.OutOrderNo, out.Amount, callbackType, out.ReferenceNo); err != nil {
		r.logger.Warn("对账回推失败", "order", item.OrderNo, "err", err)
	}
}
