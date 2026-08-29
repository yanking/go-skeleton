package repo

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"gorm.io/gorm"
)

// Callback 渠道回调记录仓储。
type Callback struct {
	db *gorm.DB
}

// NewCallback 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewCallback(db *gorm.DB) *Callback {
	return &Callback{db: db}
}

// Create 落地一条回调原始记录；GORM 写入后自动回填自增主键到 cb.ID。
func (r *Callback) Create(ctx context.Context, cb *model.Callback) error {
	if err := r.db.WithContext(ctx).Create(cb).Error; err != nil {
		return fmt.Errorf("创建回调记录: %w", err)
	}
	return nil
}

// Mark 回填回调处理结果：验签/解析后的处理状态、关联到的订单号与处理备注。
func (r *Callback) Mark(ctx context.Context, id int64, status int32, orderNo, note string) error {
	if err := r.db.WithContext(ctx).Model(&model.Callback{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":   status,
			"order_no": orderNo,
			"note":     note,
		}).Error; err != nil {
		return fmt.Errorf("更新回调处理结果: %w", err)
	}
	return nil
}
