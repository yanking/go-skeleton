package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	channelclient "github.com/yanking/go-skeleton/internal/payment/channel_client"
	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/repo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// markRecord 记录一次回调状态回填，供断言留痕结果。
type markRecord struct {
	id      int64
	status  int32
	orderNo string
	note    string
}

// stubCallbackRepo 回调记录仓储桩：Create 回填自增主键，Mark 记录留痕调用。
type stubCallbackRepo struct {
	createErr    error
	createdID    int64
	createCalled bool
	marks        []markRecord
}

func (m *stubCallbackRepo) Create(_ context.Context, cb *model.Callback) error {
	m.createCalled = true
	if m.createErr != nil {
		return m.createErr
	}
	cb.ID = m.createdID // 模拟 GORM 自增主键回填
	return nil
}

func (m *stubCallbackRepo) Mark(_ context.Context, id int64, status int32, orderNo, note string) error {
	m.marks = append(m.marks, markRecord{id, status, orderNo, note})
	return nil
}

// stubInstanceRepo 渠道实例仓储桩：findByID 未设置即视为未预期调用。
type stubInstanceRepo struct {
	findByID func(id int64) (*model.ChannelInstance, error)
}

func (m *stubInstanceRepo) FindByID(_ context.Context, id int64) (*model.ChannelInstance, error) {
	if m.findByID == nil {
		panic("未预期调用 FindByID")
	}
	return m.findByID(id)
}

func (m *stubInstanceRepo) ReplaceAll(context.Context, []model.ChannelInstance) error {
	panic("未预期调用 ReplaceAll")
}

func (m *stubInstanceRepo) FindByRoute(context.Context, string, string, string) (*model.ChannelInstance, error) {
	panic("未预期调用 FindByRoute")
}

// stubChannelClient channel 客户端桩：只关心 VerifyCallback。
type stubChannelClient struct {
	verifyCallback func(r channelclient.Route, header map[string]string, data string) (channelclient.CallbackOut, error)
}

func (m *stubChannelClient) CreateOrder(context.Context, channelclient.Route, channelclient.OrderIn) (channelclient.OrderOut, error) {
	panic("未预期调用 CreateOrder")
}

func (m *stubChannelClient) VerifyCallback(_ context.Context, r channelclient.Route, header map[string]string, data string) (channelclient.CallbackOut, error) {
	if m.verifyCallback == nil {
		panic("未预期调用 VerifyCallback")
	}
	return m.verifyCallback(r, header, data)
}

func (m *stubChannelClient) ListInstances(context.Context) ([]channelclient.Instance, error) {
	panic("未预期调用 ListInstances")
}

// cbInstance 构造一个测试渠道实例：无回调 IP 限制、data 取 body、应答串为 success。
func cbInstance() *model.ChannelInstance {
	return &model.ChannelInstance{
		ID: 1, ChannelName: "a", MerchantNo: "M", Currency: "INR",
		CallbackHeaders: "[]", CallbackDataSource: 1,
		CallbackReturn: "success", CallbackIPWhitelist: "",
	}
}

func TestHandleChannelCallback_Success(t *testing.T) {
	callbacks := &stubCallbackRepo{createdID: 7}
	instances := &stubInstanceRepo{findByID: func(int64) (*model.ChannelInstance, error) { return cbInstance(), nil }}
	channel := &stubChannelClient{
		verifyCallback: func(channelclient.Route, map[string]string, string) (channelclient.CallbackOut, error) {
			return channelclient.CallbackOut{OrderNo: "P1", CallbackType: 1, Amount: 1000, ReferenceNo: "REF1"}, nil
		},
	}
	orders := &memOrderRepo{order: stateOrder(model.OrderStatusSent, "u")}
	notifier := &memNotifier{}
	svc := New(Config{}, Deps{Callbacks: callbacks, Instances: instances, Channel: channel, Orders: orders, Queue: notifier}, testLogger())

	reply := svc.HandleChannelCallback(context.Background(), CallbackIn{InstanceID: 1, IP: "1.2.3.4", RawBody: "raw"})

	if reply.HTTPStatus != http.StatusOK || reply.Body != "success" {
		t.Fatalf("reply = %+v, want {200, success}", reply)
	}
	if orders.order.Status != model.OrderStatusSuccess {
		t.Fatalf("订单状态 = %d, want success", orders.order.Status)
	}
	if len(callbacks.marks) != 1 || callbacks.marks[0] != (markRecord{7, model.CallbackStatusVerified, "P1", ""}) {
		t.Fatalf("回调留痕 = %+v, want 一条 (7, 已验证, P1)", callbacks.marks)
	}
	if len(notifier.calls) != 1 || notifier.calls[0].orderNo != "P1" {
		t.Fatalf("入队 = %+v, want 一条 P1", notifier.calls)
	}
}

func TestHandleChannelCallback_InstanceNotFound(t *testing.T) {
	callbacks := &stubCallbackRepo{createdID: 7}
	instances := &stubInstanceRepo{findByID: func(int64) (*model.ChannelInstance, error) { return nil, repo.ErrRowNotFound }}
	// Channel/Orders 不应被触及。
	svc := New(Config{}, Deps{Callbacks: callbacks, Instances: instances}, testLogger())

	reply := svc.HandleChannelCallback(context.Background(), CallbackIn{InstanceID: 1, IP: "1.2.3.4"})

	if reply.HTTPStatus != http.StatusNotFound {
		t.Fatalf("HTTPStatus = %d, want 404", reply.HTTPStatus)
	}
	if !callbacks.createCalled {
		t.Fatal("回调原文未落库（实例不存在也须先留痕）")
	}
	if len(callbacks.marks) != 0 {
		t.Fatalf("marks = %+v, want 空（实例不存在不做状态标记）", callbacks.marks)
	}
}

func TestHandleChannelCallback_IPRejected(t *testing.T) {
	callbacks := &stubCallbackRepo{createdID: 7}
	inst := cbInstance()
	inst.CallbackIPWhitelist = "1.2.3.4,5.6.7.8" // 逗号分隔（对齐 channel 存储格式）
	instances := &stubInstanceRepo{findByID: func(int64) (*model.ChannelInstance, error) { return inst, nil }}
	svc := New(Config{}, Deps{Callbacks: callbacks, Instances: instances}, testLogger())

	reply := svc.HandleChannelCallback(context.Background(), CallbackIn{InstanceID: 1, IP: "9.9.9.9"})

	if reply.HTTPStatus != http.StatusForbidden {
		t.Fatalf("HTTPStatus = %d, want 403", reply.HTTPStatus)
	}
	if len(callbacks.marks) != 1 || callbacks.marks[0].status != model.CallbackStatusInvalid {
		t.Fatalf("回调留痕 = %+v, want 一条无效", callbacks.marks)
	}
}

func TestHandleChannelCallback_VerifyBusinessError(t *testing.T) {
	callbacks := &stubCallbackRepo{createdID: 7}
	instances := &stubInstanceRepo{findByID: func(int64) (*model.ChannelInstance, error) { return cbInstance(), nil }}
	channel := &stubChannelClient{
		verifyCallback: func(channelclient.Route, map[string]string, string) (channelclient.CallbackOut, error) {
			// 模拟 channelclient 对 gRPC 状态错误的 %w 包装（验签失败 = PermissionDenied）。
			return channelclient.CallbackOut{}, fmt.Errorf("验签回调: %w", status.Error(codes.PermissionDenied, "验签失败"))
		},
	}
	svc := New(Config{}, Deps{Callbacks: callbacks, Instances: instances, Channel: channel}, testLogger())

	reply := svc.HandleChannelCallback(context.Background(), CallbackIn{InstanceID: 1, IP: "1.2.3.4"})

	if reply.HTTPStatus != http.StatusOK || reply.Body != "success" {
		t.Fatalf("reply = %+v, want {200, success}（业务验签失败也 200 止发）", reply)
	}
	if len(callbacks.marks) != 1 || callbacks.marks[0].status != model.CallbackStatusInvalid {
		t.Fatalf("回调留痕 = %+v, want 一条无效", callbacks.marks)
	}
}

func TestHandleChannelCallback_VerifyInfraError(t *testing.T) {
	callbacks := &stubCallbackRepo{createdID: 7}
	instances := &stubInstanceRepo{findByID: func(int64) (*model.ChannelInstance, error) { return cbInstance(), nil }}
	channel := &stubChannelClient{
		verifyCallback: func(channelclient.Route, map[string]string, string) (channelclient.CallbackOut, error) {
			return channelclient.CallbackOut{}, fmt.Errorf("验签回调: %w", status.Error(codes.Unavailable, "下游不可用"))
		},
	}
	svc := New(Config{}, Deps{Callbacks: callbacks, Instances: instances, Channel: channel}, testLogger())

	reply := svc.HandleChannelCallback(context.Background(), CallbackIn{InstanceID: 1, IP: "1.2.3.4"})

	if reply.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("HTTPStatus = %d, want 500（基础设施错误等三方重发）", reply.HTTPStatus)
	}
	if len(callbacks.marks) != 0 {
		t.Fatalf("marks = %+v, want 空（基础设施错误不标无效）", callbacks.marks)
	}
}

func TestHandleChannelCallback_CreateFails(t *testing.T) {
	callbacks := &stubCallbackRepo{createErr: errors.New("写库失败")}
	// Instances 未设置 findByID：若被调用即 panic，验证落库失败时短路。
	instances := &stubInstanceRepo{}
	svc := New(Config{}, Deps{Callbacks: callbacks, Instances: instances}, testLogger())

	reply := svc.HandleChannelCallback(context.Background(), CallbackIn{InstanceID: 1, IP: "1.2.3.4"})

	if reply.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("HTTPStatus = %d, want 500（原文落库失败，宁重不丢）", reply.HTTPStatus)
	}
}

func TestHandleChannelCallback_StateMachineInvalid(t *testing.T) {
	callbacks := &stubCallbackRepo{createdID: 7}
	instances := &stubInstanceRepo{findByID: func(int64) (*model.ChannelInstance, error) { return cbInstance(), nil }}
	channel := &stubChannelClient{
		verifyCallback: func(channelclient.Route, map[string]string, string) (channelclient.CallbackOut, error) {
			return channelclient.CallbackOut{OrderNo: "P1", CallbackType: 1, Amount: 900}, nil // 金额不符
		},
	}
	orders := &memOrderRepo{order: stateOrder(model.OrderStatusSent, "u")}
	svc := New(Config{}, Deps{Callbacks: callbacks, Instances: instances, Channel: channel, Orders: orders, Queue: &memNotifier{}}, testLogger())

	reply := svc.HandleChannelCallback(context.Background(), CallbackIn{InstanceID: 1, IP: "1.2.3.4"})

	if reply.HTTPStatus != http.StatusOK || reply.Body != "success" {
		t.Fatalf("reply = %+v, want {200, success}（状态机标无效也 200 止发）", reply)
	}
	if orders.order.Status != model.OrderStatusSent {
		t.Fatalf("订单状态 = %d, want 不变 sent（金额不符不迁移）", orders.order.Status)
	}
	if len(callbacks.marks) != 1 || callbacks.marks[0].status != model.CallbackStatusInvalid {
		t.Fatalf("回调留痕 = %+v, want 一条无效", callbacks.marks)
	}
}

func TestHandleChannelCallback_OrderNotFound(t *testing.T) {
	callbacks := &stubCallbackRepo{createdID: 7}
	instances := &stubInstanceRepo{findByID: func(int64) (*model.ChannelInstance, error) { return cbInstance(), nil }}
	channel := &stubChannelClient{
		verifyCallback: func(channelclient.Route, map[string]string, string) (channelclient.CallbackOut, error) {
			return channelclient.CallbackOut{OrderNo: "P404", CallbackType: 1, Amount: 1000}, nil
		},
	}
	orders := &memOrderRepo{order: nil} // Transition 找不到订单
	svc := New(Config{}, Deps{Callbacks: callbacks, Instances: instances, Channel: channel, Orders: orders, Queue: &memNotifier{}}, testLogger())

	reply := svc.HandleChannelCallback(context.Background(), CallbackIn{InstanceID: 1, IP: "1.2.3.4"})

	if reply.HTTPStatus != http.StatusOK || reply.Body != "success" {
		t.Fatalf("reply = %+v, want {200, success}（验签通过但订单不存在，标无效止发）", reply)
	}
	if len(callbacks.marks) != 1 || callbacks.marks[0].status != model.CallbackStatusInvalid {
		t.Fatalf("回调留痕 = %+v, want 一条无效", callbacks.marks)
	}
}
