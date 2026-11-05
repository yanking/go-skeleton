// Package handler 是__scope__的协议出口：薄壳——调 service、做出口转换，业务逻辑不在此写。
//
// 依赖 service 的方式是在本包里声明需要的窄用例接口（go-style 硬规则 6「接口在使用方声明」），
// 不依赖 service 的具体类型；实现由 cmd/<svc>/initial 注入。handler 因此能脱开 service 单测。
package handler
