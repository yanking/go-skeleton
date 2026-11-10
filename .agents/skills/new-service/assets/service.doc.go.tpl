// Package service 是__scope__的业务层：持有用例逻辑，声明自己依赖的仓储接口
// （依赖倒置支点，repo 包实现）。底层错误在此统一翻译为业务 errcode
// （errcode.Wrap 挂 cause），细则见 AGENTS.md「错误处理约定」。
//
// 本包只依赖自己声明的接口：仓储如此，跨模块调用对方的 service 也如此
// （窄接口声明在本包，实现由 cmd/<svc>/initial 注入）。
package service
