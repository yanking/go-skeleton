// Package model 定义 price 的数据表模型（GORM），一表一结构体；不出服务边界
// ——rpc/both 变体由 handler 转换为对外契约类型，none 变体没有 handler 层，
// 对外形态由具体实现自行决定。
package model
