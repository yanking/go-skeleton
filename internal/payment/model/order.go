package model

import "time"

// 支付订单状态：从创建→发送至渠道→成功/失败，对应业务生命周期。
const (
	OrderStatusCreated = 1 // 已创建
	OrderStatusSent    = 2 // 已发送至渠道
	OrderStatusSuccess = 3 // 成功
	OrderStatusFailed  = 4 // 失败
)

// 通知状态：独立追踪向商户推送支付结果的异步通知是否成功。
const (
	NotifyStatusNone    = 0 // 未通知
	NotifyStatusPending = 1 // 待通知
	NotifyStatusDone    = 3 // 已通知
	NotifyStatusSkipped = 4 // 跳过通知
)

// MerchantChannel 商户-渠道绑定：商户选择使用哪些支付渠道及其优先级。
type MerchantChannel struct {
	ID                int64     `gorm:"primaryKey"`
	MerchantID        int64     `gorm:"uniqueIndex:uniq_merchant_channel;column:merchant_id"`
	ChannelInstanceID int64     `gorm:"uniqueIndex:uniq_merchant_channel;column:channel_instance_id"`
	Priority          int32     `gorm:"column:priority"`
	Enabled           bool      `gorm:"column:enabled"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (MerchantChannel) TableName() string { return "merchant_channels" }

// PaymentOrder 支付订单：核心业务单据，生命周期从创建→发送至渠道→成功/失败，并触发商户异步通知。
// Amount、Fee 单位为分；CompletedAt、NotifiedAt 为可空列；
// Status 为订单状态，NotifyStatus 为通知状态（独立追踪异步回调的通知是否成功）。
type PaymentOrder struct {
	ID                int64      `gorm:"primaryKey"`
	OrderNo           string     `gorm:"uniqueIndex:uniq_order_no;column:order_no"`
	MerchantID        int64      `gorm:"uniqueIndex:uniq_merchant_order;column:merchant_id"`
	MchOrderNo        string     `gorm:"uniqueIndex:uniq_merchant_order;column:mch_order_no"`
	Amount            int64      `gorm:"column:amount"` // 分
	Fee               int64      `gorm:"column:fee"`    // 分
	Currency          string     `gorm:"column:currency"`
	Status            int32      `gorm:"column:status"`
	ChannelInstanceID int64      `gorm:"column:channel_instance_id"`
	OutOrderNo        string     `gorm:"column:out_order_no"` // 渠道返回的订单号
	ReferenceNo       string     `gorm:"column:reference_no"` // 渠道返回的参考号
	PayURL            string     `gorm:"column:pay_url"`      // 支付链接
	NotifyURL         string     `gorm:"column:notify_url"`   // 通知 URL
	Response          string     `gorm:"column:response"`     // 渠道响应原文
	CompletedAt       *time.Time `gorm:"column:completed_at"` // 可空
	NotifyStatus      int32      `gorm:"column:notify_status"`
	NotifiedAt        *time.Time `gorm:"column:notified_at"` // 可空
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (PaymentOrder) TableName() string { return "payment_orders" }

// OutStatus 将内部订单状态映射为对外输出状态：
// created/sent → 1（待支付）、success → 2（已支付）、failed → 3（支付失败）。
func OutStatus(s int32) int32 {
	switch s {
	case OrderStatusCreated, OrderStatusSent:
		return 1
	case OrderStatusSuccess:
		return 2
	case OrderStatusFailed:
		return 3
	default:
		return 0 // 未知状态
	}
}
