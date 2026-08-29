package service

import (
	"context"
	"errors"
	"testing"

	channelclient "github.com/yanking/go-skeleton/internal/payment/channel_client"
	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

// syncChannelClient 手写 mock：只关心 ListInstances。
type syncChannelClient struct {
	listInstances func(ctx context.Context) ([]channelclient.Instance, error)
}

func (m *syncChannelClient) CreateOrder(context.Context, channelclient.Route, channelclient.OrderIn) (channelclient.OrderOut, error) {
	panic("未预期调用 CreateOrder")
}

func (m *syncChannelClient) VerifyCallback(context.Context, channelclient.Route, map[string]string, string) (channelclient.CallbackOut, error) {
	panic("未预期调用 VerifyCallback")
}

func (m *syncChannelClient) ListInstances(ctx context.Context) ([]channelclient.Instance, error) {
	return m.listInstances(ctx)
}

// syncInstanceRepo 手写 mock：只关心 ReplaceAll，捕获收到的行供断言映射结果。
type syncInstanceRepo struct {
	replaceAllErr error
	got           []model.ChannelInstance
}

func (m *syncInstanceRepo) FindByID(context.Context, int64) (*model.ChannelInstance, error) {
	panic("未预期调用 FindByID")
}

func (m *syncInstanceRepo) ReplaceAll(_ context.Context, rows []model.ChannelInstance) error {
	m.got = rows
	return m.replaceAllErr
}

func (m *syncInstanceRepo) FindByRoute(context.Context, string, string, string) (*model.ChannelInstance, error) {
	panic("未预期调用 FindByRoute")
}

func TestSyncInstances_MapsAndReplaces(t *testing.T) {
	channel := &syncChannelClient{
		listInstances: func(context.Context) ([]channelclient.Instance, error) {
			return []channelclient.Instance{
				{
					ChannelName: "a", MerchantNo: "M1", Currency: "INR",
					LimitPaymentMin: 100, LimitPaymentMax: 10000,
					CallbackHeaders: []string{"X-Sign"}, CallbackDataSource: 1,
					CallbackReturn: "success", CallbackIPWhitelist: "1.2.3.4",
				},
				{
					ChannelName: "b", MerchantNo: "M2", Currency: "USD",
					LimitPaymentMin: 200, LimitPaymentMax: 20000,
					CallbackHeaders: nil, CallbackDataSource: 2,
					CallbackReturn: "OK", CallbackIPWhitelist: "",
				},
			}, nil
		},
	}
	instances := &syncInstanceRepo{}
	svc := New(Config{}, Deps{Channel: channel, Instances: instances}, testLogger())

	if err := svc.SyncInstances(context.Background()); err != nil {
		t.Fatalf("SyncInstances() error = %v, want nil", err)
	}

	if len(instances.got) != 2 {
		t.Fatalf("ReplaceAll 收到 %d 行, want 2", len(instances.got))
	}

	first := instances.got[0]
	if first.ChannelName != "a" || first.MerchantNo != "M1" || first.Currency != "INR" {
		t.Fatalf("第一行路由 = %+v, want a/M1/INR", first)
	}
	if first.LimitPaymentMin != 100 || first.LimitPaymentMax != 10000 {
		t.Fatalf("第一行限额 = %d/%d, want 100/10000", first.LimitPaymentMin, first.LimitPaymentMax)
	}
	if first.CallbackHeaders != `["X-Sign"]` {
		t.Fatalf(`CallbackHeaders = %q, want ["X-Sign"]`, first.CallbackHeaders)
	}
	if first.CallbackDataSource != 1 || first.CallbackReturn != "success" || first.CallbackIPWhitelist != "1.2.3.4" {
		t.Fatalf("第一行其余字段 = %+v", first)
	}

	second := instances.got[1]
	if second.CallbackHeaders != "[]" {
		t.Fatalf("空 CallbackHeaders 应兜底序列化为 []，got %q", second.CallbackHeaders)
	}
}

func TestSyncInstances_ListInstancesError(t *testing.T) {
	channel := &syncChannelClient{
		listInstances: func(context.Context) ([]channelclient.Instance, error) { return nil, errors.New("下游不可用") },
	}
	svc := New(Config{}, Deps{Channel: channel}, testLogger())

	err := svc.SyncInstances(context.Background())

	if ec := errCode(t, err); ec.Code != errcode.ErrInternal.Code {
		t.Fatalf("code = %d, want %d", ec.Code, errcode.ErrInternal.Code)
	}
}

func TestSyncInstances_ReplaceAllError(t *testing.T) {
	channel := &syncChannelClient{
		listInstances: func(context.Context) ([]channelclient.Instance, error) { return nil, nil },
	}
	instances := &syncInstanceRepo{replaceAllErr: errors.New("写库失败")}
	svc := New(Config{}, Deps{Channel: channel, Instances: instances}, testLogger())

	err := svc.SyncInstances(context.Background())

	if ec := errCode(t, err); ec.Code != errcode.ErrInternal.Code {
		t.Fatalf("code = %d, want %d", ec.Code, errcode.ErrInternal.Code)
	}
}
