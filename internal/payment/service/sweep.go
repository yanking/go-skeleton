package service

import (
	"context"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

// 兜底扫描的时间阈值：三者都取「正常路径早该完成」的量级，宁可晚判也不误判。
const (
	// staleCreatedAge 订单停留在「已创建」多久算派单残留：下单请求最长也就几十秒，
	// 30 分钟仍未发出渠道，只可能是派单中途进程退出。
	staleCreatedAge = 30 * time.Minute
	// notifyNeverTriedAge 订单完成后多久仍无任何通知记录算入队丢失（入队失败只 Warn，
	// 靠本扫描补投）。
	notifyNeverTriedAge = 10 * time.Minute
	// notifyLastTriedAge 最近一次通知尝试距今多久仍停在待通知算卡住。
	//
	// 取值必须大于 asynq 两次重试之间的最大间隔，否则会把队列里仍在退避等待的任务
	// 重新入队，商户收到重复通知（SendNotify 只在 notify_status 已离开「待通知」时
	// 才幂等跳过，重试期间它一直是待通知，拦不住）。asynq v0.26 的默认退避是
	// n⁴ + 15 + rand(30)·(n+1) 秒，第 15 次重试的单次间隔最坏 51104s ≈ 14.2h
	// （15 次跑满累计约 50h，不是直觉上的几小时）。24h 留足余量。
	//
	// 代价：真正丢失的任务最长要等 24h 才被重投。这是刻意的取舍——「从未尝试过」
	// 那条判据只用 10 分钟就能兜住任务丢失的主要场景，而重复给商户发终态通知
	// 的代价远高于晚补一次。改动此值前先重算上面的公式。
	notifyLastTriedAge = 24 * time.Hour
)

// SweepStaleCreated 把滞留在「已创建」的订单收敛为失败：派单过程中进程退出会留下
// 既没发到渠道、也没落失败态的残单，它们永远等不到回调。由定时 job 周期调用。
//
// 单笔失败只记日志并继续处理下一笔——兜底通道不该因一笔坏数据整批停摆，下一轮自会重试；
// 取不到列表则是基础设施故障，上抛给调用方。
func (s *Payment) SweepStaleCreated(ctx context.Context) error {
	orders, err := s.deps.Orders.ListStaleCreated(ctx, time.Now().Add(-staleCreatedAge))
	if err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}

	for _, stale := range orders {
		orderNo := stale.OrderNo
		var notify bool
		err := s.deps.Orders.Transition(ctx, orderNo, func(o *model.PaymentOrder) (*model.PaymentOrder, error) {
			// 行锁内复查：列表读出到取到锁之间，订单可能已被回调推进离开「已创建」。
			if o.Status != model.OrderStatusCreated {
				return nil, nil
			}
			notify = toTerminal(o, model.OrderStatusFailed)
			return o, nil
		})
		if err != nil {
			s.logger.Warn("滞留待发订单置失败失败，下轮重试", "order", orderNo, "err", err)
			continue
		}
		if notify {
			s.enqueueNotify(ctx, orderNo)
		}
	}
	return nil
}

// SweepStuckNotify 重投卡住的商户通知：入队失败（只 Warn 不上抛）或 worker 侧任务丢失
// 都会让订单停在「待通知」，本扫描按两条陈旧判据捞回重投。由定时 job 周期调用。
//
// 重投是幂等的：SendNotify 取单时会复查通知状态，已完成的直接跳过。
func (s *Payment) SweepStuckNotify(ctx context.Context) error {
	now := time.Now()
	orderNos, err := s.deps.Orders.ListNotifyStuck(ctx, now.Add(-notifyNeverTriedAge), now.Add(-notifyLastTriedAge))
	if err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}

	for _, orderNo := range orderNos {
		s.enqueueNotify(ctx, orderNo)
	}
	return nil
}
