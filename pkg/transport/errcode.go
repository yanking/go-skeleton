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
// 非 errcode 错误原样放行，交由 gRPC 默认语义。
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

// toStatusErr 做实际翻译：errcode → status（业务码进 details），其余原样返回。
func toStatusErr(err error) error {
	var ec errcode.Code
	if !errors.As(err, &ec) {
		return err
	}
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

// gatewayErrorHandler 把 gRPC status 错误渲染为仓库统一 JSON 形态：
// {"code":<业务码>,"message":<用户可读消息>}，HTTP 状态用 grpc-gateway 的标准映射。
// 业务码从 status details 的 ErrorInfo.Reason 提取（errCodeToStatus 放进去的）；
// 取不到按通用内部错误处理，原始细节不外泄。
func gatewayErrorHandler(_ context.Context, _ *runtime.ServeMux, _ runtime.Marshaler,
	w http.ResponseWriter, _ *http.Request, err error,
) {
	st := status.Convert(err)
	code := errcode.ErrInternal.Code
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			if c, convErr := strconv.Atoi(info.Reason); convErr == nil {
				code = c
			}
			break
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(runtime.HTTPStatusFromCode(st.Code()))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    code,
		"message": st.Message(),
	})
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
	return runtime.DefaultHeaderMatcher(key)
}
