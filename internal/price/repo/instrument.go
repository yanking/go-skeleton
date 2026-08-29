package repo

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/price/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Instrument 标的仓储。
type Instrument struct {
	db *gorm.DB
}

// NewInstrument 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewInstrument(db *gorm.DB) *Instrument {
	return &Instrument{db: db}
}

// instrumentUpsertColumns 是 UpsertAll 冲突时覆盖的非键列
// （exchange/market/native_symbol 是冲突目标列，id/created_at 保留首次写入值）。
var instrumentUpsertColumns = []string{"symbol", "base", "quote", "status", "updated_at"}

// UpsertAll 按唯一键 (exchange, market, native_symbol) 写入一批标的；已存在则
// 覆盖展示符号、交易对拆分与状态等可变列，首次写入的 id/created_at 保持不变。
func (r *Instrument) UpsertAll(ctx context.Context, rows []model.Instrument) error {
	if len(rows) == 0 {
		return nil
	}
	if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "exchange"}, {Name: "market"}, {Name: "native_symbol"}},
		DoUpdates: clause.AssignmentColumns(instrumentUpsertColumns),
	}).Create(&rows).Error; err != nil {
		return fmt.Errorf("同步标的: %w", err)
	}
	return nil
}

// MarkDelistedExcept 把指定交易所与市场下、原生符号不在 keep 内的标的标记为已下架；
// keep 为空则该交易所与市场下全部标的都标记为已下架（本轮同步未拿到任何标的）。
func (r *Instrument) MarkDelistedExcept(ctx context.Context, exchange, market string, keep []string) error {
	db := r.db.WithContext(ctx).Model(&model.Instrument{}).
		Where("exchange = ? AND market = ?", exchange, market)
	if len(keep) > 0 {
		db = db.Where("native_symbol NOT IN ?", keep)
	}
	if err := db.Update("status", model.InstrumentStatusDelisted).Error; err != nil {
		return fmt.Errorf("标记下架标的: %w", err)
	}
	return nil
}
