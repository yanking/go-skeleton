package errcode

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
)

func TestWrapExtractsBizFields(t *testing.T) {
	cause := fmt.Errorf("dial tcp 127.0.0.1:3306: connection refused")
	err := Wrap(cause, ErrInternal)

	// 业务通道:errors.As 沿链取回业务字段。
	var ec Code
	if !errors.As(err, &ec) {
		t.Fatal("errors.As 未能从 Wrap 结果中提取 Code")
	}
	if ec.Code != ErrInternal.Code || ec.Message != ErrInternal.Message || ec.Status != ErrInternal.Status {
		t.Fatalf("提取到的业务字段不符: got %+v", ec)
	}
}

func TestWrapPreservesCauseChain(t *testing.T) {
	sentinel := errors.New("record not found")
	err := Wrap(fmt.Errorf("query user: %w", sentinel), ErrNotFound)

	// 原始通道:errors.Is 穿过 Wrap 与中间层 %w,直达哨兵。
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is 未能穿透 Wrap 到达原始哨兵错误")
	}
	if errors.Unwrap(err) == nil {
		t.Fatal("Unwrap 返回 nil, 原始错误链断裂")
	}
}

func TestWrapInDeepChain(t *testing.T) {
	sentinel := errors.New("record not found")
	// 模拟真实分层:repository 哨兵 → service Wrap 为业务码 → 上层再 %w 加上下文。
	err := fmt.Errorf("GetUser: %w", Wrap(fmt.Errorf("query: %w", sentinel), ErrNotFound))

	var ec Code
	if !errors.As(err, &ec) || ec.Code != ErrNotFound.Code {
		t.Fatalf("多层包装后 errors.As 失效: %+v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatal("多层包装后 errors.Is 失效")
	}
}

func TestErrorStrings(t *testing.T) {
	// 纯业务错误不含 cause;Wrap 结果含 cause,供日志排查。
	if got := ErrNotFound.Error(); got != "code=10002 message=资源不存在" {
		t.Fatalf("Code.Error() = %q", got)
	}
	cause := errors.New("boom")
	got := Wrap(cause, ErrInternal).Error()
	want := "code=10003 message=内部错误 cause=boom"
	if got != want {
		t.Fatalf("wrapped.Error() = %q, want %q", got, want)
	}
}

func TestPlainErrCodeIsError(t *testing.T) {
	// 哨兵直接返回时 errors.As 也要能命中(自身即目标类型)。
	var ec Code
	if !errors.As(ErrInvalidParameter, &ec) || ec.Status != codes.InvalidArgument {
		t.Fatal("纯 Code 值未能被 errors.As 提取")
	}
}
