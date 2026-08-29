// Package repo 是 channel 服务的数据访问层：实现 service 声明的仓储接口，GORM 不出本层。
package repo

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/channel/model"
	"gorm.io/gorm"
)

// Channel 渠道商户配置仓储。
type Channel struct {
	db *gorm.DB
}

// New 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func New(db *gorm.DB) *Channel {
	return &Channel{db: db}
}

// LoadAll 全量加载渠道商户配置行。渠道量级在百行以内，整表读取代价可忽略，
// 换来路由表的简单重建；行级查询无必要。
func (r *Channel) LoadAll(ctx context.Context) ([]model.Channel, error) {
	var rows []model.Channel
	if err := r.db.WithContext(ctx).
		Order("channel_name, merchant_no, currency").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("加载渠道配置: %w", err)
	}
	return rows, nil
}
