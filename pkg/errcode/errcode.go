// Package errcode 定义业务错误码与错误的「双通道」封装：
// 业务字段（错误码、用户可读消息、gRPC 状态码）面向客户端，原始错误链面向日志与排障。
//
// 用法约定：
//   - 纯业务错误（参数不合法、重名等，无底层原因）直接返回 Code 值或各服务的哨兵；
//   - 翻译底层错误时用 Wrap 把原始错误挂到业务码上——errors.As 沿链仍能取回业务字段，
//     errors.Is / Unwrap 仍能到达原始错误，两层信息互不丢失；
//   - 原始错误只进日志，绝不拼进 Message 返回给客户端。
package errcode

import (
	"fmt"

	"google.golang.org/grpc/codes"
)

// Code 业务错误。Status 用 gRPC 状态码而非 HTTP：gRPC 是服务间母协议，
// HTTP 状态由 gateway 按 gRPC 标准映射生成（AlreadyExists→409、NotFound→404…），不手工指定。
type Code struct {
	// Code 业务码，日志与客户端据此定位；分段规则见仓库错误码文档。
	Code int
	// Message 面向用户的提示，不出现内部细节（SQL、主机名、底层报错原文）。
	Message string
	// Status gRPC 状态码，决定 gRPC/HTTP 两侧的传输层状态。必填：
	// codes.Code 的零值恰好是 codes.OK，而 OK 状态没有「错误」可言。用 New 构造
	// 就不会漏；写结构体字面量时务必给上（出口层对漏填的兜底见 pkg/transport）。
	Status codes.Code
}

// New 构造业务错误码。各服务的业务码在自己的包内以包级变量声明，不放本包，
// 避免 pkg 出现业务概念。
func New(code int, message string, status codes.Code) Code {
	return Code{Code: code, Message: message, Status: status}
}

// Error 实现 error。纯业务错误没有更多信息可给，格式固定且不含底层细节。
func (e Code) Error() string {
	return fmt.Sprintf("code=%d message=%s", e.Code, e.Message)
}

// Wrap 返回携带原始错误的业务错误：翻译底层错误时使用。
// 之后 errors.As(err, &Code{}) 取业务字段、errors.Is / errors.Unwrap 走原始链，两个方向都通。
func Wrap(cause error, ec Code) error {
	return &wrapped{Code: ec, cause: cause}
}

// wrapped 是 Wrap 的返回类型，unexported：调用方只应通过 errors.As / Unwrap 观察，
// 不依赖具体类型，以便将来调整实现。
type wrapped struct {
	Code
	cause error
}

// Error 把原始错误拼进输出——此字符串面向日志与开发排障，不面向客户端；
// 客户端只看业务字段（Message），由出口层（拦截器/gateway）负责截取。
func (w *wrapped) Error() string {
	// 内嵌字段名与其 Code int 字段同名，提出来避免 w.Code.Code 歧义。
	ec := w.Code
	return fmt.Sprintf("code=%d message=%s cause=%v", ec.Code, ec.Message, w.cause)
}

// Unwrap 打通原始错误链：errors.Is(err, 哨兵) 与日志里的 cause 都依赖它。
func (w *wrapped) Unwrap() error { return w.cause }

// As 使 errors.As(err, &Code{}) 能提取业务字段。
// 内嵌不会自动获得该能力——errors.As 只沿 Unwrap 链比较可赋值类型，
// wrapped 并非 Code,故必须显式实现 As(errors 包会优先调用它)。
func (w *wrapped) As(target any) bool {
	if ec, ok := target.(*Code); ok {
		*ec = w.Code
		return true
	}
	return false
}

// 通用错误码（10000–19999）；各服务业务码在自己的包内按分段规则另行定义。
var (
	ErrInvalidParameter = New(10001, "参数错误", codes.InvalidArgument)
	ErrNotFound         = New(10002, "资源不存在", codes.NotFound)
	ErrInternal         = New(10003, "内部错误", codes.Internal)
	ErrUnauthenticated  = New(10004, "未认证", codes.Unauthenticated)
)
