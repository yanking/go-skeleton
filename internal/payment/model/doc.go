// Package model 定义 payment 的数据表模型（GORM），一表一结构体；不出服务边界，
// 对外形态由 handler 转换为契约类型。
// 金额单位一律为分。
package model
