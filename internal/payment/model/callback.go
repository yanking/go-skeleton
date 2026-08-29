package model

import "time"

// 回调来源：区分三方支付的通知渠道（主动推送 HTTP 或对账拉单）。
const (
	CallbackSourceHTTP      = 1 // HTTP 回调
	CallbackSourceReconcile = 2 // 对账回调
)

// 回调处理状态：追踪回调验证与解析的进度。
const (
	CallbackStatusReceived = 1 // 已接收
	CallbackStatusVerified = 2 // 已验证
	CallbackStatusInvalid  = 3 // 验签失败
)

// Callback 渠道回调记录：三方支付下行通知，包括 HTTP 回调与对账数据。
// Headers、Query、Body 分别存储回调的请求头、查询参数、请求体；
// Source 区分来源（HTTP/对账）；Status 记录处理结果（已接收/已验证/验签失败）。
type Callback struct {
	ID                int64     `gorm:"primaryKey"`
	ChannelInstanceID int64     `gorm:"column:channel_instance_id"`
	Source            int32     `gorm:"column:source"`  // 1 HTTP 2 对账
	Headers           string    `gorm:"column:headers"` // JSON map
	Query             string    `gorm:"column:query"`
	Body              string    `gorm:"column:body"`
	IP                string    `gorm:"column:ip"`
	Status            int32     `gorm:"column:status"` // 1 已接收 2 已验证 3 验签失败
	OrderNo           string    `gorm:"column:order_no"`
	Note              string    `gorm:"column:note"` // 处理备注
	CreatedAt         time.Time `gorm:"column:created_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (Callback) TableName() string { return "callbacks" }
