package model

import "time"

// ChannelInstance 支付渠道实例：对接某个三方支付的商户级配置（限额、回调地址等）。
// CallbackHeaders 为 JSON 数组的原文；CallbackDataSource 为 1(Body) 或 2(UrlQuery)；
// LimitPaymentMin/LimitPaymentMax 为本渠道单笔限额（分）。
type ChannelInstance struct {
	ID                  int64     `gorm:"primaryKey"`
	ChannelName         string    `gorm:"uniqueIndex:uniq_instance_route;column:channel_name"`
	MerchantNo          string    `gorm:"uniqueIndex:uniq_instance_route;column:merchant_no"`
	Currency            string    `gorm:"uniqueIndex:uniq_instance_route"`
	Enabled             bool      `gorm:"column:enabled"`
	LimitPaymentMin     int64     `gorm:"column:limit_payment_min"`
	LimitPaymentMax     int64     `gorm:"column:limit_payment_max"`
	CallbackHeaders     string    `gorm:"column:callback_headers"`     // JSON []string
	CallbackDataSource  int32     `gorm:"column:callback_data_source"` // 1 Body 2 UrlQuery
	CallbackReturn      string    `gorm:"column:callback_return"`
	CallbackIPWhitelist string    `gorm:"column:callback_ip_whitelist"`
	SyncedAt            time.Time `gorm:"column:synced_at"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	UpdatedAt           time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (ChannelInstance) TableName() string { return "channel_instances" }
