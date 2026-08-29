package repo

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"gorm.io/gorm"
)

// Notification 订单通知记录仓储。
type Notification struct {
	db *gorm.DB
}

// NewNotification 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewNotification(db *gorm.DB) *Notification {
	return &Notification{db: db}
}

// Create 落地一次向商户推送结果的通知尝试记录。
func (r *Notification) Create(ctx context.Context, n *model.OrderNotification) error {
	if err := r.db.WithContext(ctx).Create(n).Error; err != nil {
		return fmt.Errorf("创建通知记录: %w", err)
	}
	return nil
}

// CountByOrder 统计某订单已尝试通知的次数，供重试策略判定是否已达上限。
func (r *Notification) CountByOrder(ctx context.Context, orderNo string) (int64, error) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&model.OrderNotification{}).
		Where("order_no = ?", orderNo).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("统计通知次数: %w", err)
	}
	return count, nil
}
