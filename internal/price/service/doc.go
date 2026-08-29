// Package service 是 price 的业务层：持有用例逻辑，声明自己依赖的仓储接口
// （依赖倒置支点，repo 包实现）。price 没有对外的 gRPC/HTTP 出口（见 AGENTS.md
// 错误码分段表：price 不占分段），底层错误一律用 fmt.Errorf 挂 %w 逐层返回、
// 最终只进日志，不走 pkg/errcode 的双通道封装——那是给客户端可见错误用的，
// price 没有客户端。
package service
