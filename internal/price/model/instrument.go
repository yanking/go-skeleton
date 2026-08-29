package model

import "time"

// 标的状态：控制该标的是否仍在参与订阅重载。
const (
	InstrumentStatusTrading  = 1 // 交易中
	InstrumentStatusDelisted = 2 // 已下架
)

// Instrument 标的：交易所原生符号与规范化符号的映射表，是订阅重载与查询的基准数据。
// NativeSymbol 为交易所原生符号（如 Binance 的 BTCUSDT）；Symbol 为规范化后的展示符号（如 BTC-USDT）。
type Instrument struct {
	ID           int64     `gorm:"primaryKey"`
	Exchange     string    `gorm:"uniqueIndex:uniq_instrument;column:exchange"`
	Market       string    `gorm:"uniqueIndex:uniq_instrument;column:market"`
	NativeSymbol string    `gorm:"uniqueIndex:uniq_instrument;column:native_symbol"`
	Symbol       string    `gorm:"column:symbol"`
	Base         string    `gorm:"column:base"`
	Quote        string    `gorm:"column:quote"`
	Status       int32     `gorm:"column:status"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (Instrument) TableName() string { return "price_instruments" }
