package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yanking/go-skeleton/pkg/errcode"
)

// errDomain 返回业务码在 status details 里的归属域，即仓库名——错误码分段是仓库级的
// （见 AGENTS.md 错误码分段表），归属域随之取仓库而非单个服务。
// 值从编译产物的 main module path 推导而非写死：以本仓为模板派生的项目改了 module path
// 就自动跟随，不必记得回来改这一行。
var errDomain = sync.OnceValue(func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return moduleBase("")
	}
	return moduleBase(bi.Main.Path)
})

// moduleBase 从 module path 取仓库名：末段即仓库名，但主版本后缀（/v2、/v10）不是，
// 遇到它再往前取一段。module path 取不到时给固定兜底值，不让归属域出现空串。
func moduleBase(modulePath string) string {
	if modulePath == "" {
		return "unknown"
	}
	base := path.Base(modulePath)
	if dir := path.Dir(modulePath); dir != "." && isMajorVersion(base) {
		return path.Base(dir)
	}
	return base
}

// isMajorVersion 判断路径末段是否为 module 主版本后缀：v 后面全是数字才算，
// vault、v2x 这类是仓库名。
func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// errCodeToStatus 出口层错误翻译（Unary）：errors.As 提取 errcode 业务字段——
// Status/Message 面向客户端，业务码放 details 的 ErrorInfo.Reason；
// 已是 status 的错误放行（故障类码只换消息）；ctx 超时/取消不论裸 ctx 还是 status 形态都兜成通用码 10005/10006；
// 其余错误兜成通用内部错误，原始文本不出客户端。
func errCodeToStatus(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err != nil {
		return nil, toStatusErr(err)
	}
	return resp, nil
}

// errCodeToStatusStream 同 errCodeToStatus 的 Stream 形态。
func errCodeToStatusStream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return toStatusErr(handler(srv, ss))
}

// toStatusErr 做实际翻译，按错误来源分三路：
//   - errcode（含 Wrap 结果）→ status，业务码进 details；
//   - 已是 status 的错误（handler 有意构造的、或下游 gRPC 调用原样带回的）→ 取链上第一个
//     GRPCStatus 实现（errors.As 语义，即最外层）放行，外层 %w 加的上下文文本只进日志；
//     DeadlineExceeded/Canceled 例外，同 ctx 形态兜成通用码 10005/10006；
//     其余经 scrubStatus 过白名单：只有本包造出的 status 原样放行，别处来的只留码；
//   - 其余一律兜成通用内部错误：原始文本可能带 SQL、主机名，不能出客户端（宪法第 2 条），
//     出口层不能指望每一层都记得翻译。ctx 超时/取消保留其语义码（各有通用码 10005/10006），
//     客户端据此决定是否重试。
func toStatusErr(err error) error {
	if err == nil {
		return nil
	}
	var ec errcode.Code
	if errors.As(err, &ec) {
		return statusFromCode(ec)
	}
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		// GRPCStatus 返回 nil 或 OK 的实现，其 Err() 会给 nil——错误变成功，照样兜成内部错误。
		if st := gs.GRPCStatus(); st != nil && st.Code() != codes.OK {
			// 下游 gRPC 调用带回的超时/取消是 status 形态，与裸 ctx 错误是同一故障，
			// 走同一通用码——否则同一故障两种契约，客户端按 code 做重试决策会把超时当内部错误。
			switch st.Code() {
			case codes.DeadlineExceeded:
				return statusFromCode(errcode.ErrDeadlineExceeded)
			case codes.Canceled:
				return statusFromCode(errcode.ErrCanceled)
			}
			return scrubStatus(st).Err()
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return statusFromCode(errcode.ErrDeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return statusFromCode(errcode.ErrCanceled)
	}
	return statusFromCode(errcode.ErrInternal)
}

// scrubStatus 决定一个 status 能不能原样出客户端，按白名单判：只有本包
// statusFromCode 造出的 status（details 里带本仓 Domain 的 ErrorInfo）才是
// 「有意给客户端看的消息」，原样放行；别处来的只保留状态码，消息换成通用内部
// 错误消息、details 一并丢掉。
//
// 原先按「故障类码」黑名单换消息，漏掉了其余所有码——handler 漏翻译、或下游返回
// FailedPrecondition / ResourceExhausted 一类码时，消息原文直通外部客户端，实测
// 能带出 dial tcp 的内网地址、库连接串里的凭证、内部的报文大小上限（宪法第 2 条）。
// 黑名单要穷举「哪些不安全」，白名单只需说明「哪些安全」，后者才兜得住没想到的码。
//
// 保留状态码是刻意的：码携带重试语义（客户端据此决定退避还是放弃），而泄漏发生在
// 消息与 details 里。丢掉 details 同样必要——下游的 ErrorInfo 带着它自己分段的
// 业务码，透出去就是冒充本仓的码，渲染出的 code 会落在别人的分段上。
func scrubStatus(st *status.Status) *status.Status {
	if ownStatus(st) {
		return st
	}
	return status.New(st.Code(), errcode.ErrInternal.Message)
}

// ownStatus 判断 status 是否由本包的 statusFromCode 造出：details 里有一条
// ErrorInfo 且 Domain 是本仓的归属域。
func ownStatus(st *status.Status) bool {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == errDomain() {
			return true
		}
	}
	return false
}

// statusFromCode 把业务错误码翻成 status：Status/Message 面向客户端，业务码放 details 的 ErrorInfo.Reason。
func statusFromCode(ec errcode.Code) error {
	// Status 漏填时 codes.Code 的零值就是 codes.OK，而 OK 状态的 Err() 返回 nil——
	// 照直翻译会把业务错误吞成「成功」，是宪法第 1 条禁止的吞错，且客户端毫无察觉。
	// 兜成 Unknown：业务码照样进 details，调用方至少知道这是个错误。
	st := status.New(nonOKStatus(ec.Status), ec.Message)
	withDetails, derr := st.WithDetails(&errdetails.ErrorInfo{
		Reason: strconv.Itoa(ec.Code),
		Domain: errDomain(),
	})
	if derr != nil {
		// details 装不进去（几乎不可能）时退回无 details 的 status，业务码进日志。
		return st.Err()
	}
	return withDetails.Err()
}

// nonOKStatus 保证用于构造 status 的码不是 OK。业务错误码的 Status 是必填项，
// 但它是 codes.Code 类型、零值恰好是 OK，结构体字面量漏填就会撞上——出口层
// 不能因为一个漏填的字段把错误变没。
func nonOKStatus(c codes.Code) codes.Code {
	if c == codes.OK {
		return codes.Unknown
	}
	return c
}

// gatewayErrorHandler 把错误渲染为仓库统一 JSON 形态：{"code":<业务码>,"message":<用户可读消息>}，
// HTTP 状态用 grpc-gateway 的标准映射。两种 HTTP 出口共用它：环回形态的错误已经过
// gRPC 拦截器翻译，直连形态的原始错误由 renderError 里的 toStatusErr 兜底——对已是
// status 的错误再翻一次是幂等的（放行码原样、故障码消息已是通用文案），环回路径不受影响。
func gatewayErrorHandler(_ context.Context, _ *runtime.ServeMux, _ runtime.Marshaler,
	w http.ResponseWriter, _ *http.Request, err error,
) {
	renderError(w, err)
}

// renderError 按出口约定翻译错误（toStatusErr）并渲染为统一 JSON 形态。除 mux 的
// 错误处理器外，直连形态的鉴权拒绝也走这里——它拿不到 mux 的错误处理流程，同一份
// 渲染两处共用，客户端契约不变。业务码从 status details 的 ErrorInfo.Reason 提取
// （statusFromCode 放进去的），取不到按通用内部错误处理，原始细节不外泄。
// 翻译出的 gRPC 码顺手记进响应写回原（accessWriter，若有）供直连形态的访问日志
// 取用，免去再翻一次；w 不带该能力时（环回形态）静默跳过。
func renderError(w http.ResponseWriter, err error) {
	st := status.Convert(toStatusErr(err))
	renderStatus(w, runtime.HTTPStatusFromCode(st.Code()), st, err)
}

// renderStatus 按给定 HTTP 状态码渲染统一错误体。httpStatus 单独给，是为了让路由
// 错误保住 gateway 判定的 HTTP 语义（404 就是 404、405 就是 405），而不被 gRPC 码
// 的标准映射改写成别的。
func renderStatus(w http.ResponseWriter, httpStatus int, st *status.Status, cause error) {
	if acc, ok := w.(interface{ recordAccess(codes.Code, error) }); ok {
		acc.recordAccess(st.Code(), cause)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    businessCode(st),
		"message": st.Message(),
	})
}

// businessCode 从 status details 提取本仓的业务码：只认 Domain 是本仓归属域的
// ErrorInfo（statusFromCode 放进去的那条），取不到按通用内部错误处理。
//
// 必须认 Domain：不认的话别处来的 ErrorInfo 会被当成本服务的码渲染出去，
// 客户端看到的 code 落在别人的分段上。
//
// 隔离范围要说清：归属域取的是仓库名（见 errDomain），所以它隔离的是**跨仓**
// 调用。同一个 monorepo 内 A 调 B、A 又漏翻译时，B 的消息与 B 分段的码照样会
// 渲染出去——错误码分段是按服务划的，而归属域是按仓库的，这中间有个落差。
// 兜住它靠的仍是「每层只翻译相邻下层的错误」这条纪律，不是这里。
func businessCode(st *status.Status) int {
	for _, d := range st.Details() {
		info, ok := d.(*errdetails.ErrorInfo)
		if !ok || info.GetDomain() != errDomain() {
			continue
		}
		if c, err := strconv.Atoi(info.GetReason()); err == nil {
			return c
		}
	}
	return errcode.ErrInternal.Code
}

// routingErrorHandler 把 gateway 的路由错误翻成本仓的通用业务码：路径不存在 →
// 10002 资源不存在，方法不允许与请求解析失败 → 10001 参数错误。
//
// 不接这个钩子的话它们全落进 10003 内部错误，客户端按 code 分支时分不开
// 「路径写错了」和「服务端崩了」；而 AGENTS.md 声明的 10001/10002 在这些路径上
// 原先根本到不了。HTTP 状态码用 gateway 判定的那个（404/405/400），不走 gRPC 码
// 的标准映射——那会把 405 变成 501。
func routingErrorHandler(_ context.Context, _ *runtime.ServeMux, _ runtime.Marshaler,
	w http.ResponseWriter, _ *http.Request, httpStatus int,
) {
	ec := errcode.ErrInternal
	switch httpStatus {
	case http.StatusNotFound:
		ec = errcode.ErrNotFound
	case http.StatusMethodNotAllowed, http.StatusBadRequest:
		ec = errcode.ErrInvalidParameter
	}
	renderStatus(w, httpStatus, status.Convert(statusFromCode(ec)), nil)
}

// incomingHeaderMatcher 决定哪些 HTTP 头转成 gRPC metadata：先确保 Authorization
// 到达鉴权拦截器（大小写不敏感匹配，落成小写键），其余交给 grpc-gateway 的默认规则。
//
// 默认规则的实际行为（别按「转发全部头」理解）：标准头加 grpcgateway- 前缀转发，
// 自定义头（X-Custom-Thing 等）一律丢弃，而 Grpc-Metadata-Xxx 会脱去前缀成为
// metadata 键 Xxx——**客户端因此能从 HTTP 侧注入任意 metadata 键**。
// 后果见 TokenFromMetadata 的注释：metadata 里的值一律是外部输入。
func incomingHeaderMatcher(key string) (string, bool) {
	if strings.EqualFold(key, "Authorization") {
		return "authorization", true
	}
	// 凭证只留一条通道。默认规则会把 Grpc-Metadata-Authorization 脱掉前缀变成
	// metadata 的 authorization 键，于是客户端能从第二条路另送一份凭证：边缘网关
	// 只清洗（或验签后改写）真 Authorization 头时，这份它没看过；两份并存时鉴权
	// 校验的是哪一份还取决于 gateway 遍历 header map 的顺序，实测同一请求 30 次里
	// 有 3 次用了伪造的那份——合法请求偶发 401、伪造请求偶发放行，而且不可复现。
	if strings.EqualFold(key, "Grpc-Metadata-Authorization") {
		return "", false
	}
	return runtime.DefaultHeaderMatcher(key)
}
