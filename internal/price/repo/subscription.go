package repo

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/price/model"
	"gorm.io/gorm"
)

// Subscription 订阅声明仓储。
type Subscription struct {
	db *gorm.DB
}

// NewSubscription 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewSubscription(db *gorm.DB) *Subscription {
	return &Subscription{db: db}
}

// ListEnabled 列出全部已启用的订阅，供重载器周期读取生成实际连接；
// 已停用的订阅不返回，避免重载器把它们当活跃订阅重新建连。
func (r *Subscription) ListEnabled(ctx context.Context) ([]model.Subscription, error) {
	var rows []model.Subscription
	if err := r.db.WithContext(ctx).Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询启用中的订阅: %w", err)
	}
	return rows, nil
}
