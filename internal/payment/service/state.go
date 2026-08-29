package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/pkg/queue"
)

// TaskNotify 商户异步通知任务的类型名，Worker 侧以同名注册处理函数。
const TaskNotify = "payment:notify"

// 渠道结果事件类型，取值与 channelclient.CallbackOut.CallbackType 一致
// （对应 channel adapter 的 CallbackSuccess/CallbackFailure）。
const (
	channelResultSuccess int32 = 1
	channelResultFailure int32 = 2
)

// ChannelResult 渠道侧对一笔订单的确认结果，回调验签与补单回推共用同一入口。
// OrderNo 为空时按 (InstanceID, OutOrderNo) 反查订单；Amount 单位为分。
type ChannelResult struct {
	InstanceID   int64
	OrderNo      string
	OutOrderNo   string
	CallbackType int32
	Amount       int64
	ReferenceNo  string
}

// ApplyChannelResult 是订单终态的唯一写入口：按 §5 转移表在行锁事务内收敛订单状态，
// 回调/补单回推/查询共用。返回值 converged 为 false 表示渠道结果与当前状态矛盾或金额
// 不符（已告警留痕，需人工处理，但不算错误）；重复的合法事件按幂等返回 true 且无副作用；
// 仅当底层基础设施出错（订单不存在、DB 不可达等）才返回 error，供调用方按其协议翻译。
// 进入终态且需通知商户时，在事务提交后（事务外）入队通知任务。
func (s *Payment) ApplyChannelResult(ctx context.Context, r ChannelResult) (bool, error) {
	orderNo := r.OrderNo
	if orderNo == "" {
		o, err := s.deps.Orders.FindByOut(ctx, r.InstanceID, r.OutOrderNo)
		if err != nil {
			return false, err
		}
		orderNo = o.OrderNo
	}

	var converged, notify bool
	var prevStatus int32
	if err := s.deps.Orders.Transition(ctx, orderNo, func(o *model.PaymentOrder) (*model.PaymentOrder, error) {
		prevStatus = o.Status
		next, conv, n := decideChannelResult(o, r)
		converged, notify = conv, n
		return next, nil
	}); err != nil {
		return false, err
	}

	if !converged {
		s.logger.Warn("渠道结果与订单状态不可收敛，标无效留人工",
			"order", orderNo, "status", prevStatus, "instance", r.InstanceID,
			"callback_type", r.CallbackType, "amount", r.Amount)
		return false, nil
	}
	if notify {
		s.enqueueNotify(ctx, orderNo)
	}
	return true, nil
}

// decideChannelResult 按 §5 转移表决策订单下一状态（纯函数，行锁内调用）。
// 返回：next 非 nil 表示需落库的订单（就地更新的同一指针），nil 表示不落库；
// converged 为 false 表示矛盾/金额不符需标无效；notify 为 true 表示进入终态且需触发通知。
func decideChannelResult(o *model.PaymentOrder, r ChannelResult) (next *model.PaymentOrder, converged, notify bool) {
	// 实例一致性（纵深防御）：订单已归属某渠道实例后，只接受来自该实例的结果——
	// 防被攻陷/作恶的渠道用自身凭证验签通过后，以回调体里的 order_no 推进他人实例/商户
	// 的订单（跨实例串单）。created 残留单派单未落库、ChannelInstanceID 为 0 尚未归属，
	// 由 != 0 前置放行其合法推进。覆盖成功/失败两类事件，故置于事件分派之前。
	if o.ChannelInstanceID != 0 && o.ChannelInstanceID != r.InstanceID {
		return nil, false, false
	}

	switch r.CallbackType {
	case channelResultSuccess:
		// 金额严格相等才入终态，不设容差（§5 决策 3）；不符即标无效留人工。
		if r.Amount != o.Amount {
			return nil, false, false
		}
		switch o.Status {
		case model.OrderStatusCreated, model.OrderStatusSent, model.OrderStatusFailed:
			// created 是派单中途宕机的残留，sent 是常规成功，failed 是先失败后成功——三者均收敛到 success。
			o.ReferenceNo = r.ReferenceNo
			return o, true, toTerminal(o, model.OrderStatusSuccess)
		case model.OrderStatusSuccess:
			return nil, true, false // 重复成功回调，幂等忽略
		}
	case channelResultFailure:
		switch o.Status {
		case model.OrderStatusCreated, model.OrderStatusSent:
			return o, true, toTerminal(o, model.OrderStatusFailed)
		case model.OrderStatusFailed:
			return nil, true, false // 重复失败回调，幂等忽略
		case model.OrderStatusSuccess:
			return nil, false, false // success 终态不可回退（§5 决策 2），标无效留人工
		}
	}
	return nil, false, false // 未列出的事件×状态组合，标无效留人工
}

// toTerminal 把订单推进到终态：回填完成时间，并按 notify_url 有无设置通知状态——
// 有则置待通知并返回 true（需入队），空则置跳过、无需通知。
func toTerminal(o *model.PaymentOrder, status int32) bool {
	o.Status = status
	now := time.Now()
	o.CompletedAt = &now
	if o.NotifyURL == "" {
		o.NotifyStatus = model.NotifyStatusSkipped
		return false
	}
	o.NotifyStatus = model.NotifyStatusPending
	return true
}

// enqueueNotify 入队一笔商户通知任务：payload 只带订单号（内容发送时现查，防旧数据），
// 最大重试 15 次。入队失败只 Warn 不上抛——订单行已置待通知，由 notify-sweep job 兜底重投。
func (s *Payment) enqueueNotify(ctx context.Context, orderNo string) {
	payload, err := json.Marshal(struct {
		OrderNo string `json:"order_no"`
	}{OrderNo: orderNo})
	if err != nil {
		s.logger.Warn("序列化通知任务失败，交 notify-sweep 兜底", "order", orderNo, "err", err)
		return
	}
	if err := s.deps.Queue.Enqueue(ctx, TaskNotify, payload, queue.MaxRetry(15)); err != nil {
		s.logger.Warn("通知任务入队失败，交 notify-sweep 兜底", "order", orderNo, "err", err)
	}
}
