package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
)

// sweepOrderRepo 驱动两个兜底方法：列表来自注入的函数，Transition 真实执行状态机 fn
// 并把写回结果留在 saved 里，从而断言迁移后的字段而非 mock 假值。
type sweepOrderRepo struct {
	stale           []model.PaymentOrder
	staleErr        error
	staleBefore     time.Time // 记录 ListStaleCreated 收到的时间上界
	stuck           []string
	stuckErr        error
	stuckNeverTried time.Time // 记录 ListNotifyStuck 收到的两个时间上界
	stuckLastTried  time.Time
	transitErr      map[string]error // 按订单号注入 Transition 失败
	saved           map[string]*model.PaymentOrder
}

func (m *sweepOrderRepo) ListStaleCreated(_ context.Context, before time.Time) ([]model.PaymentOrder, error) {
	m.staleBefore = before
	return m.stale, m.staleErr
}

func (m *sweepOrderRepo) ListNotifyStuck(_ context.Context, neverTriedBefore, lastTriedBefore time.Time) ([]string, error) {
	m.stuckNeverTried, m.stuckLastTried = neverTriedBefore, lastTriedBefore
	return m.stuck, m.stuckErr
}

func (m *sweepOrderRepo) Transition(_ context.Context, orderNo string, fn func(o *model.PaymentOrder) (*model.PaymentOrder, error)) error {
	if err := m.transitErr[orderNo]; err != nil {
		return err
	}
	for i := range m.stale {
		if m.stale[i].OrderNo != orderNo {
			continue
		}
		next, err := fn(&m.stale[i])
		if err != nil {
			return err
		}
		if next != nil {
			if m.saved == nil {
				m.saved = map[string]*model.PaymentOrder{}
			}
			m.saved[orderNo] = next
		}
		return nil
	}
	panic("Transition 收到未在滞留列表里的订单号: " + orderNo)
}

func (m *sweepOrderRepo) Create(context.Context, *model.PaymentOrder) error {
	panic("未预期调用 Create")
}
func (m *sweepOrderRepo) FindByOrderNo(context.Context, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindByOrderNo")
}
func (m *sweepOrderRepo) FindForMerchant(context.Context, int64, string, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindForMerchant")
}
func (m *sweepOrderRepo) FindByOut(context.Context, int64, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindByOut")
}
func (m *sweepOrderRepo) MarkSent(context.Context, string, int64, string, string, string) (bool, error) {
	panic("未预期调用 MarkSent")
}
func (m *sweepOrderRepo) MarkFailedDispatch(context.Context, string) (bool, error) {
	panic("未预期调用 MarkFailedDispatch")
}
func (m *sweepOrderRepo) ListUnfinished(context.Context, int64, time.Time) ([]model.PaymentOrder, error) {
	panic("未预期调用 ListUnfinished")
}

// staleOrder 构造一笔停留在「已创建」的滞留订单。
func staleOrder(orderNo, notifyURL string) model.PaymentOrder {
	return model.PaymentOrder{
		OrderNo:      orderNo,
		Amount:       1000,
		Status:       model.OrderStatusCreated,
		NotifyURL:    notifyURL,
		NotifyStatus: model.NotifyStatusNone,
	}
}

func TestSweepStaleCreated_WithNotifyURLFailsAndEnqueues(t *testing.T) {
	orders := &sweepOrderRepo{stale: []model.PaymentOrder{staleOrder("P1", "https://m.example.com/cb")}}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	if err := svc.SweepStaleCreated(context.Background()); err != nil {
		t.Fatalf("SweepStaleCreated() error = %v, want nil", err)
	}

	got := orders.saved["P1"]
	if got == nil {
		t.Fatal("滞留单未落库，want 置为失败")
	}
	if got.Status != model.OrderStatusFailed {
		t.Errorf("Status = %d, want %d", got.Status, model.OrderStatusFailed)
	}
	if got.NotifyStatus != model.NotifyStatusPending {
		t.Errorf("NotifyStatus = %d, want %d(待通知)", got.NotifyStatus, model.NotifyStatusPending)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt = nil, want 非空")
	}
	if len(notifier.calls) != 1 || notifier.calls[0].orderNo != "P1" || notifier.calls[0].typename != TaskNotify {
		t.Errorf("入队记录 = %+v, want 一条 %s/P1", notifier.calls, TaskNotify)
	}
}

func TestSweepStaleCreated_WithoutNotifyURLSkipsNotify(t *testing.T) {
	orders := &sweepOrderRepo{stale: []model.PaymentOrder{staleOrder("P2", "")}}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	if err := svc.SweepStaleCreated(context.Background()); err != nil {
		t.Fatalf("SweepStaleCreated() error = %v, want nil", err)
	}

	got := orders.saved["P2"]
	if got == nil {
		t.Fatal("滞留单未落库，want 置为失败")
	}
	if got.NotifyStatus != model.NotifyStatusSkipped {
		t.Errorf("NotifyStatus = %d, want %d(跳过通知)", got.NotifyStatus, model.NotifyStatusSkipped)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("入队记录 = %+v, want 空(无 notify_url 不通知)", notifier.calls)
	}
}

func TestSweepStaleCreated_SkipsOrderAlreadyAdvanced(t *testing.T) {
	// 列表读出到拿到行锁之间，订单可能已被回调推进离开「已创建」——
	// 此时兜底不得再把它打成失败，否则会把一笔已成功的单改写成 failed。
	advanced := staleOrder("P3", "https://m.example.com/cb")
	advanced.Status = model.OrderStatusSuccess
	orders := &sweepOrderRepo{stale: []model.PaymentOrder{advanced}}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	if err := svc.SweepStaleCreated(context.Background()); err != nil {
		t.Fatalf("SweepStaleCreated() error = %v, want nil", err)
	}

	if orders.saved["P3"] != nil {
		t.Errorf("已推进的订单被落库为 %+v, want 不落库", orders.saved["P3"])
	}
	if orders.stale[0].Status != model.OrderStatusSuccess {
		t.Errorf("订单状态被改成 %d, want 保持 %d", orders.stale[0].Status, model.OrderStatusSuccess)
	}
	if len(notifier.calls) != 0 {
		t.Errorf("入队记录 = %+v, want 空(未发生状态迁移就不该通知)", notifier.calls)
	}
}

func TestSweepStaleCreated_UsesStaleAgeWindow(t *testing.T) {
	orders := &sweepOrderRepo{}
	svc := New(Config{}, Deps{Orders: orders, Queue: &memNotifier{}}, testLogger())

	before := time.Now()
	if err := svc.SweepStaleCreated(context.Background()); err != nil {
		t.Fatalf("SweepStaleCreated() error = %v, want nil", err)
	}

	want := before.Add(-staleCreatedAge)
	if orders.staleBefore.Before(want.Add(-time.Second)) || orders.staleBefore.After(want.Add(time.Minute)) {
		t.Errorf("ListStaleCreated(before=%v), want 约 now-%v", orders.staleBefore, staleCreatedAge)
	}
}

func TestSweepStaleCreated_OneOrderFailureDoesNotAbortBatch(t *testing.T) {
	orders := &sweepOrderRepo{
		stale: []model.PaymentOrder{
			staleOrder("P1", "https://m.example.com/cb"),
			staleOrder("P2", "https://m.example.com/cb"),
		},
		transitErr: map[string]error{"P1": errors.New("db 不可达")},
	}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	if err := svc.SweepStaleCreated(context.Background()); err != nil {
		t.Fatalf("SweepStaleCreated() error = %v, want nil(单单失败不放大为整批失败)", err)
	}
	if orders.saved["P2"] == nil {
		t.Error("P1 失败后未继续处理 P2")
	}
}

func TestSweepStaleCreated_ListErrorPropagates(t *testing.T) {
	orders := &sweepOrderRepo{staleErr: errors.New("db 不可达")}
	svc := New(Config{}, Deps{Orders: orders, Queue: &memNotifier{}}, testLogger())

	if err := svc.SweepStaleCreated(context.Background()); err == nil {
		t.Fatal("SweepStaleCreated() error = nil, want 非空(取不到整批是基础设施故障，须上抛)")
	}
}

func TestSweepStuckNotify_EnqueuesEachOrder(t *testing.T) {
	orders := &sweepOrderRepo{stuck: []string{"P1", "P2"}}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	if err := svc.SweepStuckNotify(context.Background()); err != nil {
		t.Fatalf("SweepStuckNotify() error = %v, want nil", err)
	}

	if len(notifier.calls) != 2 || notifier.calls[0].orderNo != "P1" || notifier.calls[1].orderNo != "P2" {
		t.Errorf("入队记录 = %+v, want P1、P2 各一条", notifier.calls)
	}
}

func TestSweepStuckNotify_UsesRetryWindows(t *testing.T) {
	orders := &sweepOrderRepo{}
	svc := New(Config{}, Deps{Orders: orders, Queue: &memNotifier{}}, testLogger())

	before := time.Now()
	if err := svc.SweepStuckNotify(context.Background()); err != nil {
		t.Fatalf("SweepStuckNotify() error = %v, want nil", err)
	}

	wantNever, wantLast := before.Add(-notifyNeverTriedAge), before.Add(-notifyLastTriedAge)
	if orders.stuckNeverTried.Before(wantNever.Add(-time.Second)) || orders.stuckNeverTried.After(wantNever.Add(time.Minute)) {
		t.Errorf("neverTriedBefore = %v, want 约 now-%v", orders.stuckNeverTried, notifyNeverTriedAge)
	}
	if orders.stuckLastTried.Before(wantLast.Add(-time.Second)) || orders.stuckLastTried.After(wantLast.Add(time.Minute)) {
		t.Errorf("lastTriedBefore = %v, want 约 now-%v", orders.stuckLastTried, notifyLastTriedAge)
	}
}

func TestSweepStuckNotify_ListErrorPropagates(t *testing.T) {
	orders := &sweepOrderRepo{stuckErr: errors.New("db 不可达")}
	svc := New(Config{}, Deps{Orders: orders, Queue: &memNotifier{}}, testLogger())

	if err := svc.SweepStuckNotify(context.Background()); err == nil {
		t.Fatal("SweepStuckNotify() error = nil, want 非空")
	}
}
