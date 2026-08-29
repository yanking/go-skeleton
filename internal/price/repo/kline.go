package repo

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/yanking/go-skeleton/internal/price/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Kline K 线仓储。
type Kline struct {
	db *gorm.DB
}

// NewKline 构造仓储；db 句柄由装配层（initial）从 pgsql 组件解嵌传入。
func NewKline(db *gorm.DB) *Kline {
	return &Kline{db: db}
}

// klineUpsertColumns 是 Upsert 冲突时覆盖的全部值列（主键五列是冲突目标，不在此列）。
var klineUpsertColumns = []string{
	"open", "high", "low", "close", "volume", "quote_volume", "source", "updated_at",
}

// Upsert 写入一批 K 线；冲突目标是主键五列 (exchange, market, native_symbol,
// interval, open_time)，冲突时覆盖全部值列。同一根 K 线会从实时流（ws 收线）与
// 补洞任务（REST 回填）两条路径重复到达，此为常态而非异常，冲突目标必须精确
// 对齐主键，否则要么产生重复行、要么覆盖到错误的行。
func (r *Kline) Upsert(ctx context.Context, rows []model.Kline) error {
	if len(rows) == 0 {
		return nil
	}
	if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "exchange"}, {Name: "market"}, {Name: "native_symbol"},
			{Name: "interval"}, {Name: "open_time"},
		},
		DoUpdates: clause.AssignmentColumns(klineUpsertColumns),
	}).Create(&rows).Error; err != nil {
		return fmt.Errorf("写入 K 线: %w", err)
	}
	return nil
}

// MaxOpenTime 查询指定标的与周期已落库的最大开盘时间，供补洞任务判断续接位置。
// nativeSymbol 是交易所原生 symbol（对应本表与 model.Instrument 的 NativeSymbol
// 列），不是 model.Instrument.Symbol（规范化展示符号）——两者是语义不同的两个
// 字段，传错会查不到已有行、被误判为「新标的从未拉过历史」，触发不必要的全量回溯。
// 表为空（该标的从未落过任何一根 K 线）时 max(open_time) 在 SQL 层返回 NULL，
// 用 sql.NullInt64 承接、以返回值 bool 表达「是否有值」——调用方据此区分
// 「接着上次补」（true）与「按最大回溯窗口起步」（false），判错会导致新标的
// 拉取过多历史或干脆不补。
func (r *Kline) MaxOpenTime(ctx context.Context, exchange, market, nativeSymbol, interval string) (int64, bool, error) {
	var maxOpenTime sql.NullInt64
	if err := r.db.WithContext(ctx).Model(&model.Kline{}).
		Where("exchange = ? AND market = ? AND native_symbol = ? AND interval = ?", exchange, market, nativeSymbol, interval).
		Select("max(open_time)").
		Scan(&maxOpenTime).Error; err != nil {
		return 0, false, fmt.Errorf("查询最大开盘时间: %w", err)
	}
	if !maxOpenTime.Valid {
		return 0, false, nil
	}
	return maxOpenTime.Int64, true, nil
}
