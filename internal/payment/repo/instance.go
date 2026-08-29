package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Instance 渠道实例仓储。
type Instance struct {
	db *gorm.DB
}

// NewInstance 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewInstance(db *gorm.DB) *Instance {
	return &Instance{db: db}
}

// instanceUpsertColumns 是 ReplaceAll upsert 时随冲突更新的全部非键列
// （channel_name/merchant_no/currency 是冲突目标列，id/created_at 保留首次写入值）。
var instanceUpsertColumns = []string{
	"enabled", "limit_payment_min", "limit_payment_max",
	"callback_headers", "callback_data_source", "callback_return",
	"callback_ip_whitelist", "synced_at", "updated_at",
}

// ReplaceAll 用最新快照整体替换库内渠道实例：按 (channel_name, merchant_no, currency)
// 三元组 upsert（更新全列并强制 enabled=true、synced_at=当前时间——快照来源即视为
// 当前有效配置，不采信调用方传入的这两个字段）；随后把库中不在本批三元组范围内的行
// 统一置 enabled=false——上游下线的渠道无需显式通知，靠“未出现在本批”隐式表达。
func (r *Instance) ReplaceAll(ctx context.Context, rows []model.ChannelInstance) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if len(rows) > 0 {
			for i := range rows {
				rows[i].Enabled = true
				rows[i].SyncedAt = now
				rows[i].UpdatedAt = now
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "channel_name"}, {Name: "merchant_no"}, {Name: "currency"}},
				DoUpdates: clause.AssignmentColumns(instanceUpsertColumns),
			}).Create(&rows).Error; err != nil {
				return fmt.Errorf("同步渠道实例: %w", err)
			}
		}

		disable := tx.Model(&model.ChannelInstance{}).Where("1 = 1")
		if len(rows) > 0 {
			placeholders := make([]string, len(rows))
			args := make([]any, 0, len(rows)*3)
			for i, row := range rows {
				placeholders[i] = "(?, ?, ?)"
				args = append(args, row.ChannelName, row.MerchantNo, row.Currency)
			}
			disable = tx.Model(&model.ChannelInstance{}).
				Where(fmt.Sprintf("(channel_name, merchant_no, currency) NOT IN (%s)", strings.Join(placeholders, ", ")), args...)
		}
		if err := disable.Update("enabled", false).Error; err != nil {
			return fmt.Errorf("停用未同步的渠道实例: %w", err)
		}
		return nil
	})
}

// FindByID 按主键查询渠道实例；未命中返回 ErrRowNotFound。
func (r *Instance) FindByID(ctx context.Context, id int64) (*model.ChannelInstance, error) {
	var inst model.ChannelInstance
	if err := r.db.WithContext(ctx).First(&inst, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRowNotFound
		}
		return nil, fmt.Errorf("查询渠道实例: %w", err)
	}
	return &inst, nil
}

// FindByRoute 按 (channel_name, merchant_no, currency) 路由三元组查询渠道实例；
// 未命中返回 ErrRowNotFound。
func (r *Instance) FindByRoute(ctx context.Context, channelName, merchantNo, currency string) (*model.ChannelInstance, error) {
	var inst model.ChannelInstance
	if err := r.db.WithContext(ctx).
		Where("channel_name = ? AND merchant_no = ? AND currency = ?", channelName, merchantNo, currency).
		First(&inst).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRowNotFound
		}
		return nil, fmt.Errorf("按路由查询渠道实例: %w", err)
	}
	return &inst, nil
}

// ListEnabled 列出全部已启用的渠道实例。
func (r *Instance) ListEnabled(ctx context.Context) ([]model.ChannelInstance, error) {
	var rows []model.ChannelInstance
	if err := r.db.WithContext(ctx).Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询启用中的渠道实例: %w", err)
	}
	return rows, nil
}
