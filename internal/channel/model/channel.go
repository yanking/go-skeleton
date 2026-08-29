// Package model 定义 channel 服务的数据表模型（GORM）。
package model

import "time"

// Channel 渠道商户实例：一行即原「一商户一分支一进程」的全部配置载体。
// General 字段对外暴露给网关做路由与限额；Platform 为渠道私有配置的 JSON 原文，
// 结构因渠道而异，由 adapter 层各自反序列化，本层不解释。
// 金额单位一律为分，费率一律为千分位。
type Channel struct {
	ID                  int64  `gorm:"primaryKey"`
	ChannelName         string `gorm:"uniqueIndex:uniq_channel_route;column:channel_name"`
	MerchantNo          string `gorm:"uniqueIndex:uniq_channel_route;column:merchant_no"`
	Currency            string `gorm:"uniqueIndex:uniq_channel_route"`
	ChannelLevel        int32  `gorm:"column:channel_level"`
	CallbackHeaders     string `gorm:"column:callback_headers"`     // JSON []string
	CallbackDataSource  int32  `gorm:"column:callback_data_source"` // 1 Body 2 UrlQuery
	CallbackReturn      string `gorm:"column:callback_return"`
	CallbackIPWhitelist string `gorm:"column:callback_ip_whitelist"`
	PayoutSupports      string `gorm:"column:payout_supports"` // JSON []int32
	LimitPaymentMin     int64  `gorm:"column:limit_payment_min"`
	LimitPaymentMax     int64  `gorm:"column:limit_payment_max"`
	LimitPayoutMin      int64  `gorm:"column:limit_payout_min"`
	LimitPayoutMax      int64  `gorm:"column:limit_payout_max"`
	// PaymentCommissionRate 等四项费率与单笔手续费，千分位。
	PaymentCommissionRate  int32 `gorm:"column:payment_commission_rate"`
	PaymentCommissionExtra int32 `gorm:"column:payment_commission_extra"`
	PayoutCommissionRate   int32 `gorm:"column:payout_commission_rate"`
	PayoutCommissionExtra  int32 `gorm:"column:payout_commission_extra"`
	// Platform 渠道私有配置 JSON 原文（BaseUrl、API 路径、密钥等），JSONB 列。
	Platform string `gorm:"column:platform"`
	// ReconcileEnabled 是否启用补单轮询（渠道回调不可靠时开启，job 按 15s 拉单对账）。
	ReconcileEnabled bool      `gorm:"column:reconcile_enabled"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (Channel) TableName() string { return "channels" }
