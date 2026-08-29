package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Order 支付订单仓储。
type Order struct {
	db *gorm.DB
}

// NewOrder 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewOrder(db *gorm.DB) *Order {
	return &Order{db: db}
}

// Create 创建支付订单；order_no 或 (merchant_id, mch_order_no) 撞唯一约束时
// 翻译为 ErrDuplicate（商户订单号重复是可预期的业务分支，不当普通错误处理）。
func (r *Order) Create(ctx context.Context, order *model.PaymentOrder) error {
	if err := r.db.WithContext(ctx).Create(order).Error; err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicate
		}
		return fmt.Errorf("创建订单: %w", err)
	}
	return nil
}

// FindByOrderNo 按平台订单号查询订单；未命中返回 ErrRowNotFound。
func (r *Order) FindByOrderNo(ctx context.Context, orderNo string) (*model.PaymentOrder, error) {
	var o model.PaymentOrder
	if err := r.db.WithContext(ctx).Where("order_no = ?", orderNo).First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRowNotFound
		}
		return nil, fmt.Errorf("查询订单: %w", err)
	}
	return &o, nil
}

// FindForMerchant 按商户视角查询订单：orderNo、mchOrderNo 二选一有值即按其查询
// （orderNo 优先），命中后校验订单归属；两者皆空或归属不符均视为未命中
// （不区分"不存在"与"存在但不属于该商户"，防止跨商户探测订单号）。
// mchOrderNo 分支直接把 merchant_id 并入 WHERE（对齐 uniq_merchant_order 复合唯一键）：
// 若只按 mch_order_no 查，不同商户复用同一 mch_order_no 时 First()（按主键升序取首行）
// 可能先取到别家的行，即便本商户确有同名订单也会被误判为不存在；order_no 全局唯一，
// 分支不受此影响，查到后再校验归属即可。
func (r *Order) FindForMerchant(ctx context.Context, merchantID int64, orderNo, mchOrderNo string) (*model.PaymentOrder, error) {
	db := r.db.WithContext(ctx)
	switch {
	case orderNo != "":
		db = db.Where("order_no = ?", orderNo)
	case mchOrderNo != "":
		db = db.Where("merchant_id = ? AND mch_order_no = ?", merchantID, mchOrderNo)
	default:
		return nil, ErrRowNotFound
	}

	var o model.PaymentOrder
	if err := db.First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRowNotFound
		}
		return nil, fmt.Errorf("查询订单: %w", err)
	}
	if o.MerchantID != merchantID {
		return nil, ErrRowNotFound
	}
	return &o, nil
}

// FindByOut 按渠道实例与渠道侧订单号查询订单，用于回调场景把渠道侧标识映射回内部订单；
// 未命中返回 ErrRowNotFound。
func (r *Order) FindByOut(ctx context.Context, instanceID int64, outOrderNo string) (*model.PaymentOrder, error) {
	var o model.PaymentOrder
	if err := r.db.WithContext(ctx).
		Where("channel_instance_id = ? AND out_order_no = ?", instanceID, outOrderNo).
		First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRowNotFound
		}
		return nil, fmt.Errorf("按渠道订单号查询订单: %w", err)
	}
	return &o, nil
}

// MarkSent 把已创建订单标记为已发送至渠道，回填渠道侧订单号、支付链接与响应原文；
// 条件更新（仅 status=已创建 的行才生效）防止并发路径下重复下发覆盖已变更的状态，
// 返回值表示本次调用是否实际生效。
func (r *Order) MarkSent(ctx context.Context, orderNo string, instanceID int64, outOrderNo, payURL, response string) (bool, error) {
	res := r.db.WithContext(ctx).Model(&model.PaymentOrder{}).
		Where("order_no = ? AND status = ?", orderNo, model.OrderStatusCreated).
		Updates(map[string]any{
			"status":              model.OrderStatusSent,
			"channel_instance_id": instanceID,
			"out_order_no":        outOrderNo,
			"pay_url":             payURL,
			"response":            response,
		})
	if res.Error != nil {
		return false, fmt.Errorf("标记订单已发送: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// MarkFailedDispatch 把下发渠道失败（如渠道请求异常、无可用渠道）的订单直接判失败，
// 同时把通知状态置为跳过——从未发送成功的订单没有"通知商户支付结果"的必要。
// 条件更新同 MarkSent，返回值表示本次调用是否实际生效。
func (r *Order) MarkFailedDispatch(ctx context.Context, orderNo string) (bool, error) {
	res := r.db.WithContext(ctx).Model(&model.PaymentOrder{}).
		Where("order_no = ? AND status = ?", orderNo, model.OrderStatusCreated).
		Updates(map[string]any{
			"status":        model.OrderStatusFailed,
			"notify_status": model.NotifyStatusSkipped,
			"completed_at":  time.Now(),
		})
	if res.Error != nil {
		return false, fmt.Errorf("标记订单发送失败: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// Transition 在事务内以行锁读取订单并交给 fn 决定下一状态：fn 返回 (nil, nil)
// 表示无需变更（直接提交，不写库）；返回非 nil 订单则整行 Save；fn 返回 error
// 时整个事务回滚，错误原样上抛给调用方（fn 的错误已是完整的业务错误，本层不重新包装）。
// 行锁保证同一订单的并发状态流转串行化，是回调重复投递、补单轮询等场景的并发安全底座。
func (r *Order) Transition(ctx context.Context, orderNo string, fn func(o *model.PaymentOrder) (*model.PaymentOrder, error)) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o model.PaymentOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_no = ?", orderNo).First(&o).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRowNotFound
			}
			return fmt.Errorf("锁定订单: %w", err)
		}

		next, err := fn(&o)
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		if err := tx.Save(next).Error; err != nil {
			return fmt.Errorf("保存订单状态: %w", err)
		}
		return nil
	})
}

// ListUnfinished 列出某渠道实例下、指定时间之后创建的未完结订单（已创建/已发送），
// 供补单对账 job 逐笔向渠道核实真实状态。命中 idx_order_reconcile 索引。
func (r *Order) ListUnfinished(ctx context.Context, instanceID int64, since time.Time) ([]model.PaymentOrder, error) {
	var rows []model.PaymentOrder
	if err := r.db.WithContext(ctx).
		Where("channel_instance_id = ? AND status IN ? AND created_at >= ?",
			instanceID, []int32{model.OrderStatusCreated, model.OrderStatusSent}, since).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询未完结订单: %w", err)
	}
	return rows, nil
}

// ListStaleCreated 列出创建后长期停留在"已创建"（从未成功下发渠道）的订单，
// 供清理 job 判定超时失败。
func (r *Order) ListStaleCreated(ctx context.Context, before time.Time) ([]model.PaymentOrder, error) {
	var rows []model.PaymentOrder
	if err := r.db.WithContext(ctx).
		Where("status = ? AND created_at < ?", model.OrderStatusCreated, before).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询滞留待发订单: %w", err)
	}
	return rows, nil
}

// ListNotifyStuck 列出待通知商户、但通知一直未推进的订单号：要么从未尝试过通知且
// 订单早已完成（completed_at 早于 neverTriedBefore），要么已尝试过但最近一次通知
// 时间也过于陈旧（早于 lastTriedBefore）。两段查询各自成立即算命中，用 UNION 去重合并。
func (r *Order) ListNotifyStuck(ctx context.Context, neverTriedBefore, lastTriedBefore time.Time) ([]string, error) {
	const query = `
		SELECT o.order_no FROM payment_orders o
		WHERE o.notify_status = ?
		  AND o.completed_at < ?
		  AND NOT EXISTS (SELECT 1 FROM order_notifications n WHERE n.order_no = o.order_no)
		UNION
		SELECT o.order_no FROM payment_orders o
		JOIN (
			SELECT order_no, MAX(created_at) AS last_at FROM order_notifications GROUP BY order_no
		) n ON n.order_no = o.order_no
		WHERE o.notify_status = ? AND n.last_at < ?
	`
	var orderNos []string
	if err := r.db.WithContext(ctx).
		Raw(query, model.NotifyStatusPending, neverTriedBefore, model.NotifyStatusPending, lastTriedBefore).
		Scan(&orderNos).Error; err != nil {
		return nil, fmt.Errorf("查询通知滞留订单: %w", err)
	}
	return orderNos, nil
}
