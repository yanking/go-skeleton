package repo

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"gorm.io/gorm"
)

// Binding 商户-渠道绑定仓储。
type Binding struct {
	db *gorm.DB
}

// NewBinding 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewBinding(db *gorm.DB) *Binding {
	return &Binding{db: db}
}

// ListCandidates 列出商户在指定币种下可用的候选渠道实例：要求绑定关系与渠道实例
// 均处于启用状态；currency 非空时额外按币种相等过滤，空串视为「全币种」不加此条件
// （对齐 ListAvailableChannelsRequest.currency 的「空即返回全部币种」契约）；
// 按绑定优先级升序排列（调用方按序尝试）。
// Select 显式限定列，避免 JOIN 双方同名列（enabled/id/created_at 等）在扫描时互相覆盖。
func (r *Binding) ListCandidates(ctx context.Context, merchantID int64, currency string) ([]model.ChannelInstance, error) {
	db := r.db.WithContext(ctx).
		Select("channel_instances.*").
		Joins("JOIN merchant_channels ON merchant_channels.channel_instance_id = channel_instances.id").
		Where("merchant_channels.merchant_id = ? AND merchant_channels.enabled = ? AND channel_instances.enabled = ?",
			merchantID, true, true)
	if currency != "" {
		db = db.Where("channel_instances.currency = ?", currency)
	}

	var rows []model.ChannelInstance
	if err := db.Order("merchant_channels.priority ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询候选渠道: %w", err)
	}
	return rows, nil
}
