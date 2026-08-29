package model

import "time"

// Subscription 订阅：声明式描述采集器要为哪个标的的哪种流建立订阅，由重载器周期读取本表生成实际连接。
// StreamType 取值即 exchange 包的流类型常量（kline/ticker/depth）；Interval 仅 kline 流有意义，其余流为空串。
type Subscription struct {
	ID           int64     `gorm:"primaryKey"`
	Exchange     string    `gorm:"uniqueIndex:uniq_subscription;column:exchange"`
	Market       string    `gorm:"uniqueIndex:uniq_subscription;column:market"`
	NativeSymbol string    `gorm:"uniqueIndex:uniq_subscription;column:native_symbol"`
	StreamType   string    `gorm:"uniqueIndex:uniq_subscription;column:stream_type"`
	Interval     string    `gorm:"uniqueIndex:uniq_subscription;column:interval"`
	Enabled      bool      `gorm:"column:enabled"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (Subscription) TableName() string { return "price_subscriptions" }
