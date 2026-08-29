package model

import "time"

// OrderNotification 订单通知记录：向商户推送支付结果时的重试历史。
// Attempt 记录第几次重试；ResponseCode、ResponseBody 记录商户响应。
type OrderNotification struct {
	ID           int64     `gorm:"primaryKey"`
	OrderNo      string    `gorm:"column:order_no"`
	Attempt      int32     `gorm:"column:attempt"`
	ResponseCode int32     `gorm:"column:response_code"`
	ResponseBody string    `gorm:"column:response_body"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

// TableName 固定表名，避免 GORM 复数推断歧义。
func (OrderNotification) TableName() string { return "order_notifications" }
