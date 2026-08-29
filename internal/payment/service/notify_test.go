package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/sign"
)

// notifyOrderRepo 手写 mock：只关心 FindByOrderNo 与 Transition，真实执行 Transition
// 的 fn（对齐 memOrderRepo 的写法），从而验证状态迁移本身而非 mock 假值。
type notifyOrderRepo struct {
	order        *model.PaymentOrder
	findErr      error
	transitErr   error
	transitioned bool
}

func (m *notifyOrderRepo) Create(context.Context, *model.PaymentOrder) error {
	panic("未预期调用 Create")
}

func (m *notifyOrderRepo) FindByOrderNo(_ context.Context, orderNo string) (*model.PaymentOrder, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	if m.order == nil || m.order.OrderNo != orderNo {
		panic("未预期的订单号：" + orderNo)
	}
	return m.order, nil
}

func (m *notifyOrderRepo) FindForMerchant(context.Context, int64, string, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindForMerchant")
}

func (m *notifyOrderRepo) FindByOut(context.Context, int64, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindByOut")
}

func (m *notifyOrderRepo) MarkSent(context.Context, string, int64, string, string, string) (bool, error) {
	panic("未预期调用 MarkSent")
}

func (m *notifyOrderRepo) MarkFailedDispatch(context.Context, string) (bool, error) {
	panic("未预期调用 MarkFailedDispatch")
}

func (m *notifyOrderRepo) ListUnfinished(context.Context, int64, time.Time) ([]model.PaymentOrder, error) {
	panic("未预期调用 ListUnfinished")
}
func (m *notifyOrderRepo) ListStaleCreated(context.Context, time.Time) ([]model.PaymentOrder, error) {
	panic("未预期调用 ListStaleCreated")
}
func (m *notifyOrderRepo) ListNotifyStuck(context.Context, time.Time, time.Time) ([]string, error) {
	panic("未预期调用 ListNotifyStuck")
}

func (m *notifyOrderRepo) Transition(_ context.Context, orderNo string, fn func(o *model.PaymentOrder) (*model.PaymentOrder, error)) error {
	if m.transitErr != nil {
		return m.transitErr
	}
	if m.order == nil || m.order.OrderNo != orderNo {
		panic("未预期的订单号：" + orderNo)
	}
	next, err := fn(m.order)
	if err != nil {
		return err
	}
	if next != nil {
		m.order = next
		m.transitioned = true
	}
	return nil
}

// notifyNotificationRepo 手写 mock：记录 Create 调用，CountByOrder 返回预设次数。
type notifyNotificationRepo struct {
	count     int64
	countErr  error
	createErr error
	created   []model.OrderNotification
}

func (m *notifyNotificationRepo) Create(_ context.Context, n *model.OrderNotification) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.created = append(m.created, *n)
	return nil
}

func (m *notifyNotificationRepo) CountByOrder(context.Context, string) (int64, error) {
	return m.count, m.countErr
}

// notifyHTTPClient 手写 mock：记录调用参数，返回预设的状态码/响应体/错误。
type notifyHTTPClient struct {
	code    int
	body    string
	err     error
	gotURL  string
	gotBody any
}

func (m *notifyHTTPClient) PostJSON(_ context.Context, url string, _ map[string]string, body any, _ time.Duration) (int, string, error) {
	m.gotURL = url
	m.gotBody = body
	return m.code, m.body, m.err
}

// notifyMerchant 构造一枚测试商户，含用于签名的密钥。
func notifyMerchant() *model.Merchant {
	return &model.Merchant{ID: 9, AppSecret: "s3cret"}
}

// merchantByID 构造一个只响应固定商户 ID 的 MerchantRepo 桩。
func merchantByID(t *testing.T, m *model.Merchant) *mockMerchantRepo {
	return &mockMerchantRepo{
		findByID: func(_ context.Context, id int64) (*model.Merchant, error) {
			if id != m.ID {
				t.Fatalf("FindByID(%d), want %d", id, m.ID)
			}
			return m, nil
		},
	}
}

// pendingOrder 构造一笔待通知的订单：终态成功，notify_status=待通知。
func pendingOrder() *model.PaymentOrder {
	return &model.PaymentOrder{
		OrderNo: "P1", MchOrderNo: "mch-1", MerchantID: 9,
		Status: model.OrderStatusSuccess, Amount: 1000, Fee: 30, ReferenceNo: "REF1",
		NotifyURL: "https://merchant.example/notify", NotifyStatus: model.NotifyStatusPending,
	}
}

func notifyPayload(orderNo string) []byte {
	b, _ := json.Marshal(struct {
		OrderNo string `json:"order_no"`
	}{OrderNo: orderNo})
	return b
}

func TestSendNotify_AlreadyProcessedIsIdempotent(t *testing.T) {
	order := pendingOrder()
	order.NotifyStatus = model.NotifyStatusDone // 已处理
	orders := &notifyOrderRepo{order: order}
	svc := New(Config{}, Deps{Orders: orders}, testLogger())

	if err := svc.SendNotify(context.Background(), notifyPayload("P1")); err != nil {
		t.Fatalf("SendNotify() error = %v, want nil（已处理应幂等跳过，不发起 HTTP）", err)
	}
}

func TestSendNotify_SuccessCaseInsensitive(t *testing.T) {
	order := pendingOrder()
	orders := &notifyOrderRepo{order: order}
	notifications := &notifyNotificationRepo{count: 2}
	httpClient := &notifyHTTPClient{code: http.StatusOK, body: " Success "} // 大小写混合 + 首尾空白
	merchants := merchantByID(t, notifyMerchant())
	svc := New(Config{}, Deps{Orders: orders, Notifications: notifications, HTTP: httpClient, Merchants: merchants}, testLogger())

	if err := svc.SendNotify(context.Background(), notifyPayload("P1")); err != nil {
		t.Fatalf("SendNotify() error = %v, want nil", err)
	}

	if httpClient.gotURL != order.NotifyURL {
		t.Fatalf("notify url = %q, want %q", httpClient.gotURL, order.NotifyURL)
	}
	if !orders.transitioned {
		t.Fatal("成功场景应写回 Transition")
	}
	if order.NotifyStatus != model.NotifyStatusDone {
		t.Fatalf("notify_status = %d, want %d", order.NotifyStatus, model.NotifyStatusDone)
	}
	if order.NotifiedAt == nil {
		t.Fatal("notified_at 未回填")
	}

	if len(notifications.created) != 1 {
		t.Fatalf("留痕次数 = %d, want 1", len(notifications.created))
	}
	rec := notifications.created[0]
	if rec.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3（CountByOrder=2 + 1）", rec.Attempt)
	}
	if rec.ResponseCode != http.StatusOK || rec.ResponseBody != " Success " {
		t.Fatalf("留痕响应 = %+v", rec)
	}

	// 校验通知体自洽：sign 字段应能用商户密钥对其余字段重新算出（同 sign 包规范）。
	sentBody, ok := httpClient.gotBody.(map[string]string)
	if !ok {
		t.Fatalf("发送的 body 类型 = %T, want map[string]string", httpClient.gotBody)
	}
	if sentBody["order_no"] != "mch-1" || sentBody["sys_order_no"] != "P1" {
		t.Fatalf("通知体订单号字段 = %+v", sentBody)
	}
	if sentBody["status"] != strconv.FormatInt(int64(model.OutStatus(model.OrderStatusSuccess)), 10) {
		t.Fatalf("status = %q", sentBody["status"])
	}
	if sentBody["amount"] != "1000" || sentBody["fee"] != "30" || sentBody["reference_no"] != "REF1" {
		t.Fatalf("通知体金额/参考号字段 = %+v", sentBody)
	}
	gotSig := sentBody["sign"]
	fields := make(map[string]string, len(sentBody)-1)
	for k, v := range sentBody {
		if k != "sign" {
			fields[k] = v
		}
	}
	wantSig := sign.HMAC(notifyMerchant().AppSecret, sign.Canonical(fields))
	if gotSig != wantSig {
		t.Fatalf("sign = %q, want 用密钥重算得到的 %q", gotSig, wantSig)
	}
}

func TestSendNotify_NonSuccessBodyReturnsErrorAndRecords(t *testing.T) {
	order := pendingOrder()
	orders := &notifyOrderRepo{order: order}
	notifications := &notifyNotificationRepo{count: 0}
	httpClient := &notifyHTTPClient{code: http.StatusOK, body: "fail"}
	merchants := merchantByID(t, notifyMerchant())
	svc := New(Config{}, Deps{Orders: orders, Notifications: notifications, HTTP: httpClient, Merchants: merchants}, testLogger())

	err := svc.SendNotify(context.Background(), notifyPayload("P1"))

	if err == nil {
		t.Fatal("SendNotify() error = nil, want 非 nil（body 不符应触发 asynq 重试）")
	}
	if orders.transitioned {
		t.Fatal("失败场景不应写回 Transition")
	}
	if len(notifications.created) != 1 {
		t.Fatalf("失败也应留痕，len = %d, want 1", len(notifications.created))
	}
}

func TestSendNotify_NonOKStatusReturnsError(t *testing.T) {
	order := pendingOrder()
	orders := &notifyOrderRepo{order: order}
	notifications := &notifyNotificationRepo{count: 0}
	httpClient := &notifyHTTPClient{code: http.StatusInternalServerError, body: "success"}
	merchants := merchantByID(t, notifyMerchant())
	svc := New(Config{}, Deps{Orders: orders, Notifications: notifications, HTTP: httpClient, Merchants: merchants}, testLogger())

	err := svc.SendNotify(context.Background(), notifyPayload("P1"))

	if err == nil {
		t.Fatal("SendNotify() error = nil, want 非 nil（非 200 应触发重试）")
	}
	if orders.transitioned {
		t.Fatal("失败场景不应写回 Transition")
	}
}

func TestSendNotify_ResponseBodyTruncatedTo500(t *testing.T) {
	order := pendingOrder()
	orders := &notifyOrderRepo{order: order}
	notifications := &notifyNotificationRepo{count: 0}
	httpClient := &notifyHTTPClient{code: http.StatusOK, body: strings.Repeat("x", 600)}
	merchants := merchantByID(t, notifyMerchant())
	svc := New(Config{}, Deps{Orders: orders, Notifications: notifications, HTTP: httpClient, Merchants: merchants}, testLogger())

	_ = svc.SendNotify(context.Background(), notifyPayload("P1"))

	if len(notifications.created) != 1 {
		t.Fatalf("留痕次数 = %d, want 1", len(notifications.created))
	}
	if len(notifications.created[0].ResponseBody) != 500 {
		t.Fatalf("留痕响应体长度 = %d, want 500（截断）", len(notifications.created[0].ResponseBody))
	}
}

func TestSendNotify_TransportErrorReturnsError(t *testing.T) {
	order := pendingOrder()
	orders := &notifyOrderRepo{order: order}
	notifications := &notifyNotificationRepo{count: 0}
	httpClient := &notifyHTTPClient{err: errors.New("连接超时")}
	merchants := merchantByID(t, notifyMerchant())
	svc := New(Config{}, Deps{Orders: orders, Notifications: notifications, HTTP: httpClient, Merchants: merchants}, testLogger())

	err := svc.SendNotify(context.Background(), notifyPayload("P1"))

	if err == nil {
		t.Fatal("SendNotify() error = nil, want 非 nil（网络错误应触发重试）")
	}
	if len(notifications.created) != 1 {
		t.Fatalf("网络错误也应留痕，len = %d, want 1", len(notifications.created))
	}
}
