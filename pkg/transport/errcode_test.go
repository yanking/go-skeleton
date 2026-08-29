package transport

import (
	"errors"
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

// TestToStatusErr 覆盖出口翻译：业务码进 details 的 ErrorInfo，非 errcode 错误原样放行。
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

	t.Run("非业务错误原样放行", func(t *testing.T) {
		plain := errors.New("非 errcode 错误")
		if got := toStatusErr(plain); !errors.Is(got, plain) {
			t.Errorf("toStatusErr 改写了非业务错误：%v", got)
		}
	})

	t.Run("nil 原样返回", func(t *testing.T) {
		if got := toStatusErr(nil); got != nil {
			t.Errorf("toStatusErr(nil) = %v，期望 nil", got)
		}
	})
}
