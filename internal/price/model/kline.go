package model

import "time"

// K 线来源：区分数据是实时流推送落库，还是补洞任务向 REST 回填得到。
const (
	KlineSourceStream   = 1 // 实时流
	KlineSourceBackfill = 2 // 补洞回填
)

// Kline K 线：以（交易所、市场、原生符号、周期、开盘时间）为主键，实时流与补洞任务共用同一张表、以 Source 区分来源。
// Open/High/Low/Close/Volume/QuoteVolume 用 string 承载数据库 NUMERIC 列——交易所下发的是十进制字符串，
// 转 float64 会丢精度，故不做数值化，原样透传到落库与出口。
type Kline struct {
	Exchange     string    `gorm:"primaryKey;column:exchange"`
	Market       string    `gorm:"primaryKey;column:market"`
	NativeSymbol string    `gorm:"primaryKey;column:native_symbol"`
	Interval     string    `gorm:"primaryKey;column:interval"`
	OpenTime     int64     `gorm:"primaryKey;column:open_time"`
	Open         string    `gorm:"column:open"`
	High         string    `gorm:"column:high"`
	Low          string    `gorm:"column:low"`
	Close        string    `gorm:"column:close"`
	Volume       string    `gorm:"column:volume"`
	QuoteVolume  string    `gorm:"column:quote_volume"`
	Source       int32     `gorm:"column:source"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (Kline) TableName() string { return "price_klines" }
