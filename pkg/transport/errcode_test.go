package transport

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yanking/go-skeleton/pkg/errcode"
)

// TestModuleBase 覆盖仓库名推导：主版本后缀不是仓库名，取不到 module path 时有兜底。
func TestModuleBase(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"普通 module path", "github.com/acme/acme-pay", "acme-pay"},
		{"主版本后缀不算仓库名", "github.com/acme/acme-pay/v2", "acme-pay"},
		{"多位主版本号", "github.com/acme/acme-pay/v10", "acme-pay"},
		{"v 开头但不是版本号", "github.com/acme/vault", "vault"},
		{"v 加非纯数字不是版本号", "github.com/acme/pay/v2x", "v2x"},
		{"无斜杠的裸 module path", "acme-pay", "acme-pay"},
		{"取不到 module path", "", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := moduleBase(c.path); got != c.want {
				t.Errorf("moduleBase(%q) = %q，期望 %q", c.path, got, c.want)
			}
		})
	}
}

// TestErrDomain 断言归属域接到了真实的 main module path。
// 只断言形状不断言具体值——派生项目改了 module path 后本用例仍须通过。
func TestErrDomain(t *testing.T) {
	got := errDomain()
	if got == "" || got == "unknown" {
		t.Fatalf("errDomain() = %q，未从编译产物取到 main module path", got)
	}
	if strings.Contains(got, "/") {
		t.Errorf("errDomain() = %q，应是仓库名而非完整 module path", got)
	}
}

// TestToStatusErr 覆盖出口翻译的三路：errcode 业务码进 details 的 ErrorInfo，已是 status 的放行
// （故障类码只换消息），其余兜成通用码（ctx 超时/取消为 10005/10006，其他为内部错误 10003）。
func TestToStatusErr(t *testing.T) {
	ec := errcode.New(40001, "渠道实例不存在", codes.NotFound)

	t.Run("业务错误码翻译为带 details 的 status", func(t *testing.T) {
		st, ok := status.FromError(toStatusErr(errcode.Wrap(errors.New("底层原因"), ec)))
		if !ok {
			t.Fatal("翻译结果不是 status 错误")
		}
		if st.Code() != codes.NotFound {
			t.Errorf("status code = %v，期望 %v", st.Code(), codes.NotFound)
		}
		if st.Message() != "渠道实例不存在" {
			t.Errorf("message = %q，期望业务消息而非底层原因", st.Message())
		}
		if strings.Contains(st.Message(), "底层原因") {
			t.Error("底层错误泄漏进了给客户端的消息（违反宪法第 2 条）")
		}
		details := st.Details()
		if len(details) != 1 {
			t.Fatalf("details 条数 = %d，期望 1", len(details))
		}
		info, ok := details[0].(*errdetails.ErrorInfo)
		if !ok {
			t.Fatalf("details[0] 类型 = %T，期望 *errdetails.ErrorInfo", details[0])
		}
		if info.GetReason() != "40001" {
			t.Errorf("Reason = %q，期望业务码 \"40001\"", info.GetReason())
		}
		if info.GetDomain() != errDomain() {
			t.Errorf("Domain = %q，期望取自 module path 的 %q", info.GetDomain(), errDomain())
		}
	})

	t.Run("Status 漏填也绝不吞错", func(t *testing.T) {
		// codes.Code 的零值就是 codes.OK，而 OK 状态的 Err() 返回 nil：
		// 结构体字面量漏填 Status 时照直翻译会把业务错误变成「成功」（违反宪法第 1 条）。
		zero := errcode.Code{Code: 20001, Message: "订单不存在"}
		for _, in := range []error{zero, errcode.Wrap(errors.New("底层原因"), zero)} {
			got := toStatusErr(in)
			if got == nil {
				t.Fatalf("业务错误被吞成 nil：%v", in)
			}
			st, ok := status.FromError(got)
			if !ok {
				t.Fatalf("翻译结果不是 status 错误：%v", got)
			}
			if st.Code() == codes.OK {
				t.Errorf("status code = OK，客户端会当成成功")
			}
			details := st.Details()
			if len(details) != 1 {
				t.Fatalf("业务码未进 details，条数 = %d", len(details))
			}
			if info := details[0].(*errdetails.ErrorInfo); info.GetReason() != "20001" {
				t.Errorf("Reason = %q，业务码应仍然可取", info.GetReason())
			}
		}
	})

	t.Run("非业务错误兜成内部错误，原始文本不出客户端", func(t *testing.T) {
		raw := errors.New("dial tcp 10.0.0.5:5432: connection refused")
		st, ok := status.FromError(toStatusErr(fmt.Errorf("查询用户: %w", raw)))
		if !ok {
			t.Fatal("翻译结果不是 status 错误")
		}
		if st.Code() != codes.Internal {
			t.Errorf("status code = %v，期望 %v", st.Code(), codes.Internal)
		}
		if st.Message() != errcode.ErrInternal.Message {
			t.Errorf("message = %q，期望通用内部错误消息 %q", st.Message(), errcode.ErrInternal.Message)
		}
		if strings.Contains(st.Message(), "10.0.0.5") {
			t.Error("原始错误文本泄漏进了给客户端的消息（违反宪法第 2 条）")
		}
		details := st.Details()
		if len(details) != 1 {
			t.Fatalf("details 条数 = %d，期望 1", len(details))
		}
		if info := details[0].(*errdetails.ErrorInfo); info.GetReason() != strconv.Itoa(errcode.ErrInternal.Code) {
			t.Errorf("Reason = %q，期望通用内部错误码 %d", info.GetReason(), errcode.ErrInternal.Code)
		}
	})

	t.Run("客户端类 status 原样放行，外层包装文本不出客户端", func(t *testing.T) {
		inner := status.Error(codes.PermissionDenied, "无权访问")
		st, ok := status.FromError(toStatusErr(fmt.Errorf("校验 10.0.0.5: %w", inner)))
		if !ok {
			t.Fatal("翻译结果不是 status 错误")
		}
		if st.Code() != codes.PermissionDenied || st.Message() != "无权访问" {
			t.Errorf("status = %v %q，期望 PermissionDenied 无权访问", st.Code(), st.Message())
		}
	})

	t.Run("故障类 status 只换消息，码与 details 保留", func(t *testing.T) {
		// Unavailable 是下游不可达时 gRPC 客户端库生成的码，消息带拨号详情；Unknown 是下游漏翻译时
		// gRPC 默认给的码，消息就是原始错误文本——四个故障类码的消息都不能带出去。
		for _, code := range []codes.Code{codes.Unknown, codes.Internal, codes.Unavailable, codes.DataLoss} {
			raw := status.New(code, `connection error: desc = "transport: Error while dialing: dial tcp 10.0.0.5:50051: connect: connection refused"`)
			withInfo, err := raw.WithDetails(&errdetails.ErrorInfo{Reason: "7", Domain: "downstream"})
			if err != nil {
				t.Fatal(err)
			}
			st := status.Convert(toStatusErr(fmt.Errorf("查询用户: %w", withInfo.Err())))
			if st.Code() != code {
				t.Errorf("%v: status code = %v，期望保留", code, st.Code())
			}
			if st.Message() != errcode.ErrInternal.Message || strings.Contains(st.Message(), "10.0.0.5") {
				t.Errorf("%v: message = %q，期望换成通用内部错误消息", code, st.Message())
			}
			details := st.Details()
			if len(details) != 1 {
				t.Errorf("%v: details 条数 = %d，期望保留 1 条", code, len(details))
				continue
			}
			if info, ok := details[0].(*errdetails.ErrorInfo); !ok || info.GetReason() != "7" {
				t.Errorf("%v: details[0] = %v，期望保留下游原 ErrorInfo（Reason \"7\"）", code, details[0])
			}
		}
	})

	t.Run("GRPCStatus 给 OK 的怪异实现也绝不吞错", func(t *testing.T) {
		got := toStatusErr(okStatusErr{})
		if got == nil {
			t.Fatal("错误被吞成 nil")
		}
		if st := status.Convert(got); st.Code() != codes.Internal {
			t.Errorf("status code = %v，期望兜成 %v", st.Code(), codes.Internal)
		}
	})

	t.Run("errcode 与 status 同在链上时 errcode 优先", func(t *testing.T) {
		err := errcode.Wrap(status.Error(codes.PermissionDenied, "无权访问"), ec)
		if st := status.Convert(toStatusErr(err)); st.Code() != codes.NotFound || st.Message() != ec.Message {
			t.Errorf("status = %v %q，期望按 errcode 翻译为 NotFound %q", st.Code(), st.Message(), ec.Message)
		}
	})

	t.Run("ctx 超时与取消保留语义码，包装文本不出客户端", func(t *testing.T) {
		cases := []struct {
			in   error
			want codes.Code
			ec   errcode.Code
		}{
			{fmt.Errorf("查询 10.0.0.5: %w", context.DeadlineExceeded), codes.DeadlineExceeded, errcode.ErrDeadlineExceeded},
			{fmt.Errorf("查询 10.0.0.5: %w", context.Canceled), codes.Canceled, errcode.ErrCanceled},
			// 下游 gRPC 调用带回的 status 形态与裸 ctx 错误是同一故障，也走同一通用码。
			{fmt.Errorf("调下游 10.0.0.5: %w", status.Error(codes.DeadlineExceeded, "context deadline exceeded")), codes.DeadlineExceeded, errcode.ErrDeadlineExceeded},
			{status.Error(codes.Canceled, "context canceled"), codes.Canceled, errcode.ErrCanceled},
		}
		for _, c := range cases {
			st := status.Convert(toStatusErr(c.in))
			if st.Code() != c.want {
				t.Errorf("toStatusErr(%v) code = %v，期望 %v", c.in, st.Code(), c.want)
			}
			if st.Message() != c.ec.Message {
				t.Errorf("toStatusErr(%v) message = %q，期望通用码消息 %q", c.in, st.Message(), c.ec.Message)
			}
			if strings.Contains(st.Message(), "10.0.0.5") {
				t.Errorf("ctx 错误的包装文本泄漏进了客户端消息: %q", st.Message())
			}
			details := st.Details()
			if len(details) != 1 {
				t.Errorf("toStatusErr(%v) details 条数 = %d，期望 1", c.in, len(details))
				continue
			}
			if info, ok := details[0].(*errdetails.ErrorInfo); !ok || info.GetReason() != strconv.Itoa(c.ec.Code) {
				t.Errorf("toStatusErr(%v) details[0] = %v，期望 Reason 为通用码 %d", c.in, details[0], c.ec.Code)
			}
		}
	})

	t.Run("nil 原样返回", func(t *testing.T) {
		if got := toStatusErr(nil); got != nil {
			t.Errorf("toStatusErr(nil) = %v，期望 nil", got)
		}
	})
}

// okStatusErr 模拟 GRPCStatus 返回 OK 的错误实现：其 Err() 会给 nil，出口层不能让它变成功。
type okStatusErr struct{}

func (okStatusErr) Error() string              { return "怪异实现" }
func (okStatusErr) GRPCStatus() *status.Status { return status.New(codes.OK, "") }
