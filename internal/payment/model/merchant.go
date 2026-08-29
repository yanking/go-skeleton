package model

import "time"

// 商户状态：控制商户是否允许发起支付交易。
const (
	MerchantStatusNormal = 1 // 正常
	MerchantStatusBanned = 2 // 封禁
)

// Merchant 商户配置：应用 ID/密钥、额度、费率等。
// IPWhitelist 为 JSON 数组的原文；LimitMin/LimitMax 为单笔限额（分）；FeeRate/FeeExtra 为费率/单笔手续费（千分位）。
type Merchant struct {
	ID          int64     `gorm:"primaryKey"`
	AppID       string    `gorm:"uniqueIndex:uniq_merchant_app;column:app_id"`
	AppSecret   string    `gorm:"column:app_secret"`
	Name        string    `gorm:"column:name"`
	Status      int32     `gorm:"column:status"`
	IPWhitelist string    `gorm:"column:ip_whitelist"` // JSON []string
	LimitMin    int64     `gorm:"column:limit_min"`
	LimitMax    int64     `gorm:"column:limit_max"`
	FeeRate     int32     `gorm:"column:fee_rate"`  // 千分位
	FeeExtra    int32     `gorm:"column:fee_extra"` // 分，单笔手续费
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (Merchant) TableName() string { return "merchants" }
