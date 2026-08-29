package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"testing"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
	"github.com/yanking/go-skeleton/internal/channel/model"
	"github.com/yanking/go-skeleton/pkg/errcode"
	"log/slog"
)

// fakeRepo 内存仓储，模拟渠道配置表。
type fakeRepo struct{ rows []model.Channel }

func (f *fakeRepo) LoadAll(context.Context) ([]model.Channel, error) { return f.rows, nil }

func testLogger() *slog.Logger { return slog.Default() }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// payapayConf payapay 适配器可接受的最小 platform 配置。
func payapayConf(t *testing.T) string {
	return mustJSON(t, map[string]any{
		"base_url":   "https://channel.example",
		"app_secret": "s3cret",
	})
}

func newTestSvc(t *testing.T) *ChannelSvc {
	t.Helper()
	repo := &fakeRepo{rows: []model.Channel{
		{
			ChannelName: "payapay", MerchantNo: "016", Currency: "INR",
			CallbackHeaders: "[]", PayoutSupports: "[1]",
			Platform: payapayConf(t),
		},
		{ChannelName: "ghost", MerchantNo: "001", Currency: "INR", CallbackHeaders: "[]", PayoutSupports: "[]"}, // 未迁移渠道
		{ChannelName: "payapay", MerchantNo: "099", Currency: "INR",
			CallbackHeaders: "[]", PayoutSupports: "[]", Platform: `{"base_url":`}, // 脏配置行
	}}
	svc, err := New(context.Background(), repo, httpc.New(httpc.Config{}), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func TestNew_SkipsUnmigratedAndBrokenRows(t *testing.T) {
	svc := newTestSvc(t)

	if got := len(svc.ListChannels(context.Background())); got != 1 {
		t.Fatalf("ListChannels want 1 available channel, got %d", got)
	}
}

func TestLookup_RouteResolution(t *testing.T) {
	svc := newTestSvc(t)
	ctx := context.Background()

	// 命中。
	ins, err := svc.lookup(ctx, adapter.Route{ChannelName: "payapay", MerchantNo: "016", Currency: "INR"})
	if err != nil || ins == nil {
		t.Fatalf("valid route should hit, err=%v", err)
	}

	// 未命中翻译为 40001。
	_, err = svc.lookup(ctx, adapter.Route{ChannelName: "payapay", MerchantNo: "000", Currency: "INR"})
	var ec errcode.Code
	if !errors.As(err, &ec) || ec.Code != 40001 {
		t.Fatalf("miss should be 40001, got %v", err)
	}

	// 三元组缺字段按参数错误。
	_, err = svc.lookup(ctx, adapter.Route{ChannelName: "payapay"})
	if !errors.Is(err, errcode.ErrInvalidParameter) {
		t.Fatalf("incomplete route should be invalid parameter, got %v", err)
	}
}

func TestTranslate_AdapterSentinels(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want int
	}{
		{"rejected", adapter.ErrChannelRejected, 40002},
		{"verify", adapter.ErrVerifyFailed, 40003},
		{"unknown_status", adapter.ErrUnknownCallbackStatus, 40004},
		{"bad_response", adapter.ErrBadResponse, 40005},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := errors.Join(tc.in) // 带一层包装再翻译
			var ec errcode.Code
			if !errors.As(translate(wrapped), &ec) || ec.Code != tc.want {
				t.Fatalf("want %d, got %v", tc.want, translate(wrapped))
			}
		})
	}
}

func TestReconcileRoutes_OnlyEnabled(t *testing.T) {
	repo := &fakeRepo{rows: []model.Channel{
		{ChannelName: "payapay", MerchantNo: "016", Currency: "INR",
			CallbackHeaders: "[]", PayoutSupports: "[]", Platform: payapayConf(t), ReconcileEnabled: true},
		{ChannelName: "neokred", MerchantNo: "001", Currency: "INR",
			CallbackHeaders: "[]", PayoutSupports: "[]", Platform: mustJSON(t, map[string]any{
				"email": "a@b.c", "password": "p",
				"dashboard_apis": map[string]string{"login": "https://x", "query": "https://x", "balance": "https://x"},
				"payment":        map[string]any{"client_secret": "s", "program_id": "p", "apis": map[string]string{"order": "https://x", "query": "https://x"}},
				"payout":         map[string]any{"client_secret": "s", "program_id": "p", "apis": map[string]string{"order": "https://x", "query": "https://x"}},
			})},
	}}
	svc, err := New(context.Background(), repo, httpc.New(httpc.Config{}), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	routes := svc.ReconcileRoutes(context.Background())
	if len(routes) != 1 || routes[0].ChannelName != "payapay" {
		t.Fatalf("want only payapay reconciling, got %+v", routes)
	}
}
