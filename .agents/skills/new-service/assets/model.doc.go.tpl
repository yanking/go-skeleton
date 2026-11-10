// Package model 定义__scope__的数据表模型（GORM），一表一结构体；不出所属模块（平铺的服务
// 即整个服务）——rpc/both 变体由 handler 转换为对外契约类型，none 变体没有 handler 层，
// 对外形态由具体实现自行决定。
package model
