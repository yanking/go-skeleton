// Package service 是 __svc__ 的业务层：持有用例逻辑，声明自己依赖的仓储接口
// （依赖倒置支点，repo 包实现）。底层错误在此统一翻译为业务 errcode
// （errcode.Wrap 挂 cause），细则见 AGENTS.md「错误处理约定」。
package service
