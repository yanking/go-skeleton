package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/repo"
	"github.com/yanking/go-skeleton/pkg/queue"
)

// memOrderRepo 用内存中的单笔订单副本驱动 Transition：真实执行状态机 fn 的决策与
// 落库（fn 返回非 nil 即写回），从而验证状态迁移逻辑本身而非 mock 假值。
type memOrderRepo struct {
	order      *model.PaymentOrder
	transitErr error // 注入基础设施错误（如 DB 不可达）
	findByOut  func(instanceID int64, outOrderNo string) (*model.PaymentOrder, error)
	saved      bool // Transition 是否实际写回（fn 返回非 nil）
}

func (m *memOrderRepo) Transition(_ context.Context, orderNo string, fn func(o *model.PaymentOrder) (*model.PaymentOrder, error)) error {
	if m.transitErr != nil {
		return m.transitErr
	}
	if m.order == nil || m.order.OrderNo != orderNo {
		return repo.ErrRowNotFound
	}
	next, err := fn(m.order)
	if err != nil {
		return err
	}
	if next != nil {
		m.order = next
		m.saved = true
	}
	return nil
}

func (m *memOrderRepo) FindByOut(_ context.Context, instanceID int64, outOrderNo string) (*model.PaymentOrder, error) {
	if m.findByOut == nil {
		panic("未预期调用 FindByOut")
	}
	return m.findByOut(instanceID, outOrderNo)
}

func (m *memOrderRepo) Create(context.Context, *model.PaymentOrder) error {
	panic("未预期调用 Create")
}
func (m *memOrderRepo) FindByOrderNo(context.Context, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindByOrderNo")
}
func (m *memOrderRepo) FindForMerchant(context.Context, int64, string, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindForMerchant")
}
func (m *memOrderRepo) MarkSent(context.Context, string, int64, string, string, string) (bool, error) {
	panic("未预期调用 MarkSent")
}
func (m *memOrderRepo) MarkFailedDispatch(context.Context, string) (bool, error) {
	panic("未预期调用 MarkFailedDispatch")
}
func (m *memOrderRepo) ListUnfinished(context.Context, int64, time.Time) ([]model.PaymentOrder, error) {
	panic("未预期调用 ListUnfinished")
}
func (m *memOrderRepo) ListStaleCreated(context.Context, time.Time) ([]model.PaymentOrder, error) {
	panic("未预期调用 ListStaleCreated")
}
func (m *memOrderRepo) ListNotifyStuck(context.Context, time.Time, time.Time) ([]string, error) {
	panic("未预期调用 ListNotifyStuck")
}

// enqueueCall 记录一次入队调用，供断言任务类型名与订单号。
type enqueueCall struct {
	typename string
	orderNo  string
}

// memNotifier 记录入队调用；err 非空模拟入队失败（验证「失败只 Warn」）。
type memNotifier struct {
	err   error
	calls []enqueueCall
}

func (m *memNotifier) Enqueue(_ context.Context, typename string, payload []byte, _ ...queue.Option) error {
	if m.err != nil {
		return m.err
	}
	var p struct {
		OrderNo string `json:"order_no"`
	}
	_ = json.Unmarshal(payload, &p)
	m.calls = append(m.calls, enqueueCall{typename: typename, orderNo: p.OrderNo})
	return nil
}

// stateOrder 构造一笔金额 1000、订单号 P1 的订单，初始通知状态为未通知。
func stateOrder(status int32, notifyURL string) *model.PaymentOrder {
	return &model.PaymentOrder{
		OrderNo:      "P1",
		Amount:       1000,
		Status:       status,
		NotifyURL:    notifyURL,
		NotifyStatus: model.NotifyStatusNone,
	}
}

func TestApplyChannelResult_TransitionMatrix(t *testing.T) {
	const success = 1 // channelResultSuccess
	const failure = 2 // channelResultFailure

	cases := []struct {
		name          string
		initStatus    int32
		notifyURL     string
		callbackType  int32
		amount        int64
		wantConverged bool
		wantSaved     bool
		wantStatus    int32 // wantSaved 为 true 时校验；否则应保持 initStatus
		wantNotify    int32 // 落库终态后的 notify_status
		wantEnqueue   bool
	}{
		{"sent+成功+有通知URL", model.OrderStatusSent, "u", success, 1000, true, true, model.OrderStatusSuccess, model.NotifyStatusPending, true},
		{"sent+成功+无通知URL", model.OrderStatusSent, "", success, 1000, true, true, model.OrderStatusSuccess, model.NotifyStatusSkipped, false},
		{"created+成功(宕机残留)", model.OrderStatusCreated, "u", success, 1000, true, true, model.OrderStatusSuccess, model.NotifyStatusPending, true},
		{"failed+成功(先失败后成功)", model.OrderStatusFailed, "u", success, 1000, true, true, model.OrderStatusSuccess, model.NotifyStatusPending, true},
		{"sent+失败", model.OrderStatusSent, "u", failure, 0, true, true, model.OrderStatusFailed, model.NotifyStatusPending, true},
		{"created+失败", model.OrderStatusCreated, "u", failure, 0, true, true, model.OrderStatusFailed, model.NotifyStatusPending, true},
		{"金额不符标无效", model.OrderStatusSent, "u", success, 900, false, false, model.OrderStatusSent, 0, false},
		{"success+成功幂等", model.OrderStatusSuccess, "u", success, 1000, true, false, model.OrderStatusSuccess, 0, false},
		{"success+失败不反转", model.OrderStatusSuccess, "u", failure, 0, false, false, model.OrderStatusSuccess, 0, false},
		{"failed+失败幂等", model.OrderStatusFailed, "u", failure, 0, true, false, model.OrderStatusFailed, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orders := &memOrderRepo{order: stateOrder(tc.initStatus, tc.notifyURL)}
			notifier := &memNotifier{}
			svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

			converged, err := svc.ApplyChannelResult(context.Background(), ChannelResult{
				InstanceID:   1,
				OrderNo:      "P1",
				CallbackType: tc.callbackType,
				Amount:       tc.amount,
				ReferenceNo:  "REF1",
			})
			if err != nil {
				t.Fatalf("ApplyChannelResult() error = %v, want nil", err)
			}
			if converged != tc.wantConverged {
				t.Fatalf("converged = %v, want %v", converged, tc.wantConverged)
			}
			if orders.saved != tc.wantSaved {
				t.Fatalf("saved = %v, want %v", orders.saved, tc.wantSaved)
			}

			o := orders.order
			if tc.wantSaved {
				if o.Status != tc.wantStatus {
					t.Fatalf("status = %d, want %d", o.Status, tc.wantStatus)
				}
				if o.NotifyStatus != tc.wantNotify {
					t.Fatalf("notify_status = %d, want %d", o.NotifyStatus, tc.wantNotify)
				}
				if o.CompletedAt == nil {
					t.Fatal("completed_at 未设置，终态迁移应回填完成时间")
				}
				if tc.callbackType == success && o.ReferenceNo != "REF1" {
					t.Fatalf("reference_no = %q, want REF1", o.ReferenceNo)
				}
			} else {
				if o.Status != tc.initStatus {
					t.Fatalf("status = %d, want 不变 %d", o.Status, tc.initStatus)
				}
				if o.CompletedAt != nil {
					t.Fatal("completed_at 被改动，不收敛/幂等场景不应写库")
				}
				if o.NotifyStatus != model.NotifyStatusNone {
					t.Fatalf("notify_status = %d, want 不变 %d", o.NotifyStatus, model.NotifyStatusNone)
				}
			}

			if tc.wantEnqueue {
				if len(notifier.calls) != 1 {
					t.Fatalf("入队次数 = %d, want 1", len(notifier.calls))
				}
				if notifier.calls[0].typename != TaskNotify {
					t.Fatalf("任务类型 = %q, want %q", notifier.calls[0].typename, TaskNotify)
				}
				if notifier.calls[0].orderNo != "P1" {
					t.Fatalf("入队订单号 = %q, want P1", notifier.calls[0].orderNo)
				}
			} else if len(notifier.calls) != 0 {
				t.Fatalf("入队次数 = %d, want 0", len(notifier.calls))
			}
		})
	}
}

func TestApplyChannelResult_ResolveByOut(t *testing.T) {
	order := stateOrder(model.OrderStatusSent, "u")
	var gotInstance int64
	var gotOut string
	orders := &memOrderRepo{
		order: order,
		findByOut: func(instanceID int64, outOrderNo string) (*model.PaymentOrder, error) {
			gotInstance, gotOut = instanceID, outOrderNo
			return order, nil
		},
	}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	converged, err := svc.ApplyChannelResult(context.Background(), ChannelResult{
		InstanceID: 7, OrderNo: "", OutOrderNo: "OUT1", CallbackType: 1, Amount: 1000,
	})
	if err != nil {
		t.Fatalf("ApplyChannelResult() error = %v, want nil", err)
	}
	if !converged {
		t.Fatal("converged = false, want true")
	}
	if gotInstance != 7 || gotOut != "OUT1" {
		t.Fatalf("FindByOut(%d, %q), want (7, \"OUT1\")", gotInstance, gotOut)
	}
	if order.Status != model.OrderStatusSuccess {
		t.Fatalf("status = %d, want success", order.Status)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].orderNo != "P1" {
		t.Fatalf("入队 = %+v, want 一条 P1", notifier.calls)
	}
}

func TestApplyChannelResult_InstanceMismatch(t *testing.T) {
	order := stateOrder(model.OrderStatusSent, "u")
	order.ChannelInstanceID = 2 // 订单归属实例 B
	orders := &memOrderRepo{order: order}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	// 回调来自实例 A（≠B 且都非 0）：被攻陷的渠道 A 用自身凭证验签通过后，
	// 以回调体里的 order_no 试图推进他人实例的订单——应标无效留人工，不迁移不通知。
	converged, err := svc.ApplyChannelResult(context.Background(), ChannelResult{
		InstanceID: 1, OrderNo: "P1", CallbackType: 1, Amount: 1000,
	})
	if err != nil {
		t.Fatalf("ApplyChannelResult() error = %v, want nil", err)
	}
	if converged {
		t.Fatal("converged = true, want false（跨实例串单应标无效不迁移）")
	}
	if orders.saved {
		t.Fatal("saved = true, want false（跨实例不应写库）")
	}
	if order.Status != model.OrderStatusSent {
		t.Fatalf("status = %d, want 不变 sent", order.Status)
	}
	if len(notifier.calls) != 0 {
		t.Fatal("跨实例串单不应入队通知")
	}
}

func TestApplyChannelResult_OrderNotFound(t *testing.T) {
	orders := &memOrderRepo{order: nil} // Transition 找不到 P404
	svc := New(Config{}, Deps{Orders: orders, Queue: &memNotifier{}}, testLogger())

	converged, err := svc.ApplyChannelResult(context.Background(), ChannelResult{
		OrderNo: "P404", CallbackType: 1, Amount: 1000,
	})
	if converged {
		t.Fatal("converged = true, want false")
	}
	if !errors.Is(err, repo.ErrRowNotFound) {
		t.Fatalf("err = %v, want repo.ErrRowNotFound（上抛供调用方翻译）", err)
	}
}

func TestApplyChannelResult_FindByOutError(t *testing.T) {
	orders := &memOrderRepo{
		findByOut: func(int64, string) (*model.PaymentOrder, error) { return nil, repo.ErrRowNotFound },
	}
	svc := New(Config{}, Deps{Orders: orders, Queue: &memNotifier{}}, testLogger())

	converged, err := svc.ApplyChannelResult(context.Background(), ChannelResult{
		InstanceID: 1, OrderNo: "", OutOrderNo: "OUT404", CallbackType: 1, Amount: 1000,
	})
	if converged {
		t.Fatal("converged = true, want false")
	}
	if !errors.Is(err, repo.ErrRowNotFound) {
		t.Fatalf("err = %v, want repo.ErrRowNotFound", err)
	}
}

func TestApplyChannelResult_InfraErrorPropagates(t *testing.T) {
	infra := errors.New("数据库不可达")
	orders := &memOrderRepo{order: stateOrder(model.OrderStatusSent, "u"), transitErr: infra}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	converged, err := svc.ApplyChannelResult(context.Background(), ChannelResult{
		OrderNo: "P1", CallbackType: 1, Amount: 1000,
	})
	if converged {
		t.Fatal("converged = true, want false")
	}
	if !errors.Is(err, infra) {
		t.Fatalf("err = %v, want 注入的基础设施错误", err)
	}
	if len(notifier.calls) != 0 {
		t.Fatal("基础设施错误时不应入队通知")
	}
}

func TestApplyChannelResult_EnqueueFailureOnlyWarns(t *testing.T) {
	orders := &memOrderRepo{order: stateOrder(model.OrderStatusSent, "u")}
	notifier := &memNotifier{err: errors.New("入队失败")}
	svc := New(Config{}, Deps{Orders: orders, Queue: notifier}, testLogger())

	converged, err := svc.ApplyChannelResult(context.Background(), ChannelResult{
		OrderNo: "P1", CallbackType: 1, Amount: 1000,
	})
	if err != nil {
		t.Fatalf("ApplyChannelResult() error = %v, want nil（入队失败只 Warn，交 notify-sweep 兜底）", err)
	}
	if !converged {
		t.Fatal("converged = false, want true")
	}
	if orders.order.Status != model.OrderStatusSuccess {
		t.Fatalf("status = %d, want success（终态迁移已提交，与入队解耦）", orders.order.Status)
	}
}
