package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/repo"
)

// gwInstanceRepo 渠道实例仓储桩：只关心 FindByRoute。
type gwInstanceRepo struct {
	findByRoute func(channelName, merchantNo, currency string) (*model.ChannelInstance, error)
}

func (m *gwInstanceRepo) FindByID(context.Context, int64) (*model.ChannelInstance, error) {
	panic("未预期调用 FindByID")
}

func (m *gwInstanceRepo) ReplaceAll(context.Context, []model.ChannelInstance) error {
	panic("未预期调用 ReplaceAll")
}

func (m *gwInstanceRepo) FindByRoute(_ context.Context, channelName, merchantNo, currency string) (*model.ChannelInstance, error) {
	if m.findByRoute == nil {
		panic("未预期调用 FindByRoute")
	}
	return m.findByRoute(channelName, merchantNo, currency)
}

// gwOrderRepo 支付订单仓储桩：只关心 ListUnfinished。
type gwOrderRepo struct {
	listUnfinished func(instanceID int64, since time.Time) ([]model.PaymentOrder, error)
}

func (m *gwOrderRepo) Create(context.Context, *model.PaymentOrder) error {
	panic("未预期调用 Create")
}
func (m *gwOrderRepo) FindByOrderNo(context.Context, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindByOrderNo")
}
func (m *gwOrderRepo) FindForMerchant(context.Context, int64, string, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindForMerchant")
}
func (m *gwOrderRepo) FindByOut(context.Context, int64, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindByOut")
}
func (m *gwOrderRepo) MarkSent(context.Context, string, int64, string, string, string) (bool, error) {
	panic("未预期调用 MarkSent")
}
func (m *gwOrderRepo) MarkFailedDispatch(context.Context, string) (bool, error) {
	panic("未预期调用 MarkFailedDispatch")
}
func (m *gwOrderRepo) Transition(context.Context, string, func(o *model.PaymentOrder) (*model.PaymentOrder, error)) error {
	panic("未预期调用 Transition")
}
func (m *gwOrderRepo) ListUnfinished(_ context.Context, instanceID int64, since time.Time) ([]model.PaymentOrder, error) {
	if m.listUnfinished == nil {
		panic("未预期调用 ListUnfinished")
	}
	return m.listUnfinished(instanceID, since)
}
func (m *gwOrderRepo) ListStaleCreated(context.Context, time.Time) ([]model.PaymentOrder, error) {
	panic("未预期调用 ListStaleCreated")
}
func (m *gwOrderRepo) ListNotifyStuck(context.Context, time.Time, time.Time) ([]string, error) {
	panic("未预期调用 ListNotifyStuck")
}

func TestUnfinishedOrders_Success(t *testing.T) {
	inst := &model.ChannelInstance{ID: 5}
	var gotChannel, gotMerchant, gotCurrency string
	instances := &gwInstanceRepo{
		findByRoute: func(channelName, merchantNo, currency string) (*model.ChannelInstance, error) {
			gotChannel, gotMerchant, gotCurrency = channelName, merchantNo, currency
			return inst, nil
		},
	}
	want := []model.PaymentOrder{{OrderNo: "P1"}, {OrderNo: "P2"}}
	var gotInstanceID int64
	var gotSince time.Time
	orders := &gwOrderRepo{
		listUnfinished: func(instanceID int64, since time.Time) ([]model.PaymentOrder, error) {
			gotInstanceID, gotSince = instanceID, since
			return want, nil
		},
	}
	svc := New(Config{}, Deps{Instances: instances, Orders: orders}, testLogger())

	rows, err := svc.UnfinishedOrders(context.Background(), "a", "M1", "INR", 30)
	if err != nil {
		t.Fatalf("UnfinishedOrders() error = %v, want nil", err)
	}
	if len(rows) != 2 || rows[0].OrderNo != "P1" || rows[1].OrderNo != "P2" {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	if gotChannel != "a" || gotMerchant != "M1" || gotCurrency != "INR" {
		t.Fatalf("FindByRoute(%q, %q, %q), want (a, M1, INR)", gotChannel, gotMerchant, gotCurrency)
	}
	if gotInstanceID != 5 {
		t.Fatalf("instanceID = %d, want 5", gotInstanceID)
	}
	wantSince := time.Now().Add(-30 * time.Minute)
	if diff := gotSince.Sub(wantSince); diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("since = %v, want 约 %v", gotSince, wantSince)
	}
}

func TestUnfinishedOrders_RouteNotFound(t *testing.T) {
	instances := &gwInstanceRepo{
		findByRoute: func(string, string, string) (*model.ChannelInstance, error) { return nil, repo.ErrRowNotFound },
	}
	orders := &gwOrderRepo{} // 未设置 listUnfinished：若被调用即 panic，验证路由未命中时短路
	svc := New(Config{}, Deps{Instances: instances, Orders: orders}, testLogger())

	_, err := svc.UnfinishedOrders(context.Background(), "a", "M1", "INR", 30)

	if !errors.Is(err, repo.ErrRowNotFound) {
		t.Fatalf("err = %v, want repo.ErrRowNotFound", err)
	}
}

func TestUnfinishedOrders_ListUnfinishedError(t *testing.T) {
	infra := errors.New("数据库不可达")
	instances := &gwInstanceRepo{
		findByRoute: func(string, string, string) (*model.ChannelInstance, error) {
			return &model.ChannelInstance{ID: 1}, nil
		},
	}
	orders := &gwOrderRepo{
		listUnfinished: func(int64, time.Time) ([]model.PaymentOrder, error) { return nil, infra },
	}
	svc := New(Config{}, Deps{Instances: instances, Orders: orders}, testLogger())

	_, err := svc.UnfinishedOrders(context.Background(), "a", "M1", "INR", 30)

	if !errors.Is(err, infra) {
		t.Fatalf("err = %v, want 注入的基础设施错误", err)
	}
}

func TestApplyReconcilePush_NonPaymentOrderTypeIgnored(t *testing.T) {
	instances := &gwInstanceRepo{} // 未设置 findByRoute：若被调用即 panic，验证代付类型短路跳过
	svc := New(Config{}, Deps{Instances: instances}, testLogger())

	err := svc.ApplyReconcilePush(context.Background(), "a", "M1", "INR", 2, "P1", "OUT1", 1000, 1, "REF1")

	if err != nil {
		t.Fatalf("ApplyReconcilePush() error = %v, want nil（代付未接入，忽略不算错误）", err)
	}
}

func TestApplyReconcilePush_RouteNotFound(t *testing.T) {
	instances := &gwInstanceRepo{
		findByRoute: func(string, string, string) (*model.ChannelInstance, error) { return nil, repo.ErrRowNotFound },
	}
	svc := New(Config{}, Deps{Instances: instances}, testLogger())

	err := svc.ApplyReconcilePush(context.Background(), "a", "M1", "INR", 1, "P1", "OUT1", 1000, 1, "REF1")

	if !errors.Is(err, repo.ErrRowNotFound) {
		t.Fatalf("err = %v, want repo.ErrRowNotFound", err)
	}
}

func TestApplyReconcilePush_CallbackCreateFails(t *testing.T) {
	instances := &gwInstanceRepo{
		findByRoute: func(string, string, string) (*model.ChannelInstance, error) {
			return &model.ChannelInstance{ID: 5}, nil
		},
	}
	callbacks := &stubCallbackRepo{createErr: errors.New("写库失败")}
	svc := New(Config{}, Deps{Instances: instances, Callbacks: callbacks}, testLogger())

	err := svc.ApplyReconcilePush(context.Background(), "a", "M1", "INR", 1, "P1", "OUT1", 1000, 1, "REF1")

	if err == nil {
		t.Fatal("ApplyReconcilePush() error = nil, want 落库失败上抛")
	}
}

func TestApplyReconcilePush_Converged(t *testing.T) {
	instances := &gwInstanceRepo{
		findByRoute: func(string, string, string) (*model.ChannelInstance, error) {
			return &model.ChannelInstance{ID: 5}, nil
		},
	}
	callbacks := &stubCallbackRepo{createdID: 9}
	orders := &memOrderRepo{order: stateOrder(model.OrderStatusSent, "u")}
	svc := New(Config{}, Deps{Instances: instances, Callbacks: callbacks, Orders: orders, Queue: &memNotifier{}}, testLogger())

	err := svc.ApplyReconcilePush(context.Background(), "a", "M1", "INR", 1, "P1", "OUT1", 1000, 1, "REF1")

	if err != nil {
		t.Fatalf("ApplyReconcilePush() error = %v, want nil", err)
	}
	if orders.order.Status != model.OrderStatusSuccess {
		t.Fatalf("订单状态 = %d, want success", orders.order.Status)
	}
	if !callbacks.createCalled {
		t.Fatal("回调记录未落库")
	}
	if len(callbacks.marks) != 0 { // ApplyReconcilePush 不做 Mark（不同于 HTTP 回调路径，brief 未要求）
		t.Fatalf("marks = %+v, want 空", callbacks.marks)
	}
}

func TestApplyReconcilePush_NotConvergedStillSuccess(t *testing.T) {
	instances := &gwInstanceRepo{
		findByRoute: func(string, string, string) (*model.ChannelInstance, error) {
			return &model.ChannelInstance{ID: 5}, nil
		},
	}
	callbacks := &stubCallbackRepo{createdID: 9}
	orders := &memOrderRepo{order: stateOrder(model.OrderStatusSent, "u")}
	svc := New(Config{}, Deps{Instances: instances, Callbacks: callbacks, Orders: orders, Queue: &memNotifier{}}, testLogger())

	// 金额不符：ApplyChannelResult 返回 (converged=false, err=nil)，按裁定仍视为成功
	// （不可收敛也返回成功，同 §8「不可收敛也返回成功、仅基础设施错误才重试」整体语义）。
	err := svc.ApplyReconcilePush(context.Background(), "a", "M1", "INR", 1, "P1", "OUT1", 900, 1, "REF1")

	if err != nil {
		t.Fatalf("ApplyReconcilePush() error = %v, want nil", err)
	}
	if orders.order.Status != model.OrderStatusSent {
		t.Fatalf("订单状态 = %d, want 不变 sent", orders.order.Status)
	}
}

func TestApplyReconcilePush_InfraErrorPropagates(t *testing.T) {
	instances := &gwInstanceRepo{
		findByRoute: func(string, string, string) (*model.ChannelInstance, error) {
			return &model.ChannelInstance{ID: 5}, nil
		},
	}
	callbacks := &stubCallbackRepo{createdID: 9}
	infra := errors.New("数据库不可达")
	orders := &memOrderRepo{order: stateOrder(model.OrderStatusSent, "u"), transitErr: infra}
	svc := New(Config{}, Deps{Instances: instances, Callbacks: callbacks, Orders: orders, Queue: &memNotifier{}}, testLogger())

	err := svc.ApplyReconcilePush(context.Background(), "a", "M1", "INR", 1, "P1", "OUT1", 1000, 1, "REF1")

	if !errors.Is(err, infra) {
		t.Fatalf("err = %v, want 注入的基础设施错误", err)
	}
}
