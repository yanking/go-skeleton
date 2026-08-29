package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/repo"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

func TestQueryOrder_BothKeysEmpty(t *testing.T) {
	svc := New(Config{}, Deps{}, testLogger())

	_, err := svc.QueryOrder(context.Background(), testMerchant(), "", "")

	if ec := errCode(t, err); ec.Code != errcode.ErrInvalidParameter.Code {
		t.Fatalf("code = %d, want %d", ec.Code, errcode.ErrInvalidParameter.Code)
	}
}

func TestQueryOrder_NotFoundOrOtherMerchant(t *testing.T) {
	orders := &mockOrderRepo{
		findForMerchant: func(context.Context, int64, string, string) (*model.PaymentOrder, error) {
			return nil, repo.ErrRowNotFound // FindForMerchant 对「不存在」与「不属于该商户」统一返回该哨兵
		},
	}
	svc := New(Config{}, Deps{Orders: orders}, testLogger())

	_, err := svc.QueryOrder(context.Background(), testMerchant(), "P1", "")

	if ec := errCode(t, err); ec.Code != errcode.ErrNotFound.Code {
		t.Fatalf("code = %d, want %d", ec.Code, errcode.ErrNotFound.Code)
	}
}

func TestQueryOrder_InfraErrorWraps(t *testing.T) {
	orders := &mockOrderRepo{
		findForMerchant: func(context.Context, int64, string, string) (*model.PaymentOrder, error) {
			return nil, errors.New("数据库不可达")
		},
	}
	svc := New(Config{}, Deps{Orders: orders}, testLogger())

	_, err := svc.QueryOrder(context.Background(), testMerchant(), "P1", "")

	if ec := errCode(t, err); ec.Code != errcode.ErrInternal.Code {
		t.Fatalf("code = %d, want %d", ec.Code, errcode.ErrInternal.Code)
	}
}

func TestQueryOrder_Success(t *testing.T) {
	completed := time.UnixMilli(1_700_000_000_000)
	order := &model.PaymentOrder{
		OrderNo: "P1", MchOrderNo: "mch-1", Status: model.OrderStatusSuccess,
		Amount: 1000, Fee: 30, ReferenceNo: "REF1", CompletedAt: &completed,
	}
	orders := &mockOrderRepo{
		findForMerchant: func(_ context.Context, merchantID int64, orderNo, mchOrderNo string) (*model.PaymentOrder, error) {
			if merchantID != 1 || orderNo != "P1" || mchOrderNo != "" {
				t.Fatalf("FindForMerchant(%d, %q, %q), want (1, \"P1\", \"\")", merchantID, orderNo, mchOrderNo)
			}
			return order, nil
		},
	}
	svc := New(Config{}, Deps{Orders: orders}, testLogger())

	view, err := svc.QueryOrder(context.Background(), testMerchant(), "P1", "")
	if err != nil {
		t.Fatalf("QueryOrder() error = %v, want nil", err)
	}

	want := OrderView{
		OrderNo: "P1", MchOrderNo: "mch-1", Status: model.OutStatus(model.OrderStatusSuccess),
		Amount: 1000, Fee: 30, ReferenceNo: "REF1", CompletedAt: completed.UnixMilli(),
	}
	if view != want {
		t.Fatalf("QueryOrder() = %+v, want %+v", view, want)
	}
}

func TestQueryOrder_NilCompletedAtIsZero(t *testing.T) {
	order := &model.PaymentOrder{OrderNo: "P1", MchOrderNo: "mch-1", Status: model.OrderStatusCreated}
	orders := &mockOrderRepo{
		findForMerchant: func(context.Context, int64, string, string) (*model.PaymentOrder, error) { return order, nil },
	}
	svc := New(Config{}, Deps{Orders: orders}, testLogger())

	view, err := svc.QueryOrder(context.Background(), testMerchant(), "", "mch-1")
	if err != nil {
		t.Fatalf("QueryOrder() error = %v, want nil", err)
	}
	if view.CompletedAt != 0 {
		t.Fatalf("CompletedAt = %d, want 0（未完成订单不应有完成时间）", view.CompletedAt)
	}
}

func TestAvailableChannels_AggregatesLimitsUnion(t *testing.T) {
	bindings := &mockBindingRepo{
		listCandidates: func(_ context.Context, merchantID int64, currency string) ([]model.ChannelInstance, error) {
			if merchantID != 1 {
				t.Fatalf("merchantID = %d, want 1", merchantID)
			}
			if currency != "" {
				t.Fatalf("currency = %q, want 空（全币种）", currency)
			}
			return []model.ChannelInstance{
				{ChannelName: "a", Currency: "INR", LimitPaymentMin: 100, LimitPaymentMax: 5000},
				{ChannelName: "a", Currency: "INR", LimitPaymentMin: 50, LimitPaymentMax: 8000}, // 同组，取并集
				{ChannelName: "b", Currency: "USD", LimitPaymentMin: 10, LimitPaymentMax: 100},
			}, nil
		},
	}
	svc := New(Config{}, Deps{Bindings: bindings}, testLogger())

	views, err := svc.AvailableChannels(context.Background(), testMerchant(), "")
	if err != nil {
		t.Fatalf("AvailableChannels() error = %v, want nil", err)
	}

	want := []ChannelView{
		{ChannelName: "a", Currency: "INR", LimitMin: 50, LimitMax: 8000},
		{ChannelName: "b", Currency: "USD", LimitMin: 10, LimitMax: 100},
	}
	if len(views) != len(want) {
		t.Fatalf("views = %+v, want %+v", views, want)
	}
	for i := range want {
		if views[i] != want[i] {
			t.Fatalf("views[%d] = %+v, want %+v", i, views[i], want[i])
		}
	}
}

func TestAvailableChannels_InfraErrorWraps(t *testing.T) {
	bindings := &mockBindingRepo{
		listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
			return nil, errors.New("数据库不可达")
		},
	}
	svc := New(Config{}, Deps{Bindings: bindings}, testLogger())

	_, err := svc.AvailableChannels(context.Background(), testMerchant(), "INR")

	if ec := errCode(t, err); ec.Code != errcode.ErrInternal.Code {
		t.Fatalf("code = %d, want %d", ec.Code, errcode.ErrInternal.Code)
	}
}

func TestAvailableChannels_EmptyCandidates(t *testing.T) {
	bindings := &mockBindingRepo{
		listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) { return nil, nil },
	}
	svc := New(Config{}, Deps{Bindings: bindings}, testLogger())

	views, err := svc.AvailableChannels(context.Background(), testMerchant(), "INR")
	if err != nil {
		t.Fatalf("AvailableChannels() error = %v, want nil", err)
	}
	if len(views) != 0 {
		t.Fatalf("views = %+v, want empty", views)
	}
}
