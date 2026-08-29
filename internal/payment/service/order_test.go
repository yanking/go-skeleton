package service

import (
	"context"
	"errors"
	"testing"
	"time"

	channelclient "github.com/yanking/go-skeleton/internal/payment/channel_client"
	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/repo"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

// mockBindingRepo 手写 mock：函数字段结构体实现 BindingRepo。
type mockBindingRepo struct {
	listCandidates func(ctx context.Context, merchantID int64, currency string) ([]model.ChannelInstance, error)
}

func (m *mockBindingRepo) ListCandidates(ctx context.Context, merchantID int64, currency string) ([]model.ChannelInstance, error) {
	if m.listCandidates == nil {
		panic("未预期调用 ListCandidates")
	}
	return m.listCandidates(ctx, merchantID, currency)
}

// mockOrderRepo 手写 mock：只对测试用到的方法设置函数字段，未设置时调用即 panic
// （暴露测试未预期到的调用路径，而不是静默返回零值掩盖问题）。
type mockOrderRepo struct {
	create             func(ctx context.Context, order *model.PaymentOrder) error
	findForMerchant    func(ctx context.Context, merchantID int64, orderNo, mchOrderNo string) (*model.PaymentOrder, error)
	markSent           func(ctx context.Context, orderNo string, instanceID int64, outOrderNo, payURL, response string) (bool, error)
	markFailedDispatch func(ctx context.Context, orderNo string) (bool, error)
}

func (m *mockOrderRepo) Create(ctx context.Context, order *model.PaymentOrder) error {
	return m.create(ctx, order)
}

func (m *mockOrderRepo) FindByOrderNo(context.Context, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindByOrderNo")
}

func (m *mockOrderRepo) FindForMerchant(ctx context.Context, merchantID int64, orderNo, mchOrderNo string) (*model.PaymentOrder, error) {
	if m.findForMerchant == nil {
		panic("未预期调用 FindForMerchant")
	}
	return m.findForMerchant(ctx, merchantID, orderNo, mchOrderNo)
}

func (m *mockOrderRepo) FindByOut(context.Context, int64, string) (*model.PaymentOrder, error) {
	panic("未预期调用 FindByOut")
}

func (m *mockOrderRepo) MarkSent(ctx context.Context, orderNo string, instanceID int64, outOrderNo, payURL, response string) (bool, error) {
	if m.markSent == nil {
		panic("未预期调用 MarkSent")
	}
	return m.markSent(ctx, orderNo, instanceID, outOrderNo, payURL, response)
}

func (m *mockOrderRepo) MarkFailedDispatch(ctx context.Context, orderNo string) (bool, error) {
	if m.markFailedDispatch == nil {
		panic("未预期调用 MarkFailedDispatch")
	}
	return m.markFailedDispatch(ctx, orderNo)
}

func (m *mockOrderRepo) Transition(context.Context, string, func(o *model.PaymentOrder) (*model.PaymentOrder, error)) error {
	panic("未预期调用 Transition")
}

func (m *mockOrderRepo) ListUnfinished(context.Context, int64, time.Time) ([]model.PaymentOrder, error) {
	panic("未预期调用 ListUnfinished")
}
func (m *mockOrderRepo) ListStaleCreated(context.Context, time.Time) ([]model.PaymentOrder, error) {
	panic("未预期调用 ListStaleCreated")
}
func (m *mockOrderRepo) ListNotifyStuck(context.Context, time.Time, time.Time) ([]string, error) {
	panic("未预期调用 ListNotifyStuck")
}

// mockChannelClient 手写 mock：只关心 CreateOrder，其余方法在 CreateOrder 用例
// 里不应被调用。
type mockChannelClient struct {
	createOrder func(ctx context.Context, r channelclient.Route, in channelclient.OrderIn) (channelclient.OrderOut, error)
}

func (m *mockChannelClient) CreateOrder(ctx context.Context, r channelclient.Route, in channelclient.OrderIn) (channelclient.OrderOut, error) {
	return m.createOrder(ctx, r, in)
}

func (m *mockChannelClient) VerifyCallback(context.Context, channelclient.Route, map[string]string, string) (channelclient.CallbackOut, error) {
	panic("未预期调用 VerifyCallback")
}

func (m *mockChannelClient) ListInstances(context.Context) ([]channelclient.Instance, error) {
	panic("未预期调用 ListInstances")
}

// testMerchant 构造一个限额 100~100000、费率 30‰ 的测试商户。
func testMerchant() *model.Merchant {
	return &model.Merchant{ID: 1, LimitMin: 100, LimitMax: 100000, FeeRate: 30, FeeExtra: 0}
}

// baseIn 构造一组合法的下单入参，用例按需覆盖个别字段。
func baseIn() CreateOrderIn {
	return CreateOrderIn{MchOrderNo: "mch-1", Amount: 1000, Currency: "INR"}
}

// instance 构造一个测试用渠道实例。
func instance(id int64, channelName string, limitMin, limitMax int64) model.ChannelInstance {
	return model.ChannelInstance{ID: id, ChannelName: channelName, MerchantNo: "M" + channelName, Currency: "INR",
		LimitPaymentMin: limitMin, LimitPaymentMax: limitMax}
}

// errCode 从 err 中取出 errcode.Code，非该类型直接 Fatal。
func errCode(t *testing.T, err error) errcode.Code {
	t.Helper()
	var ec errcode.Code
	if !errors.As(err, &ec) {
		t.Fatalf("err = %v, want errcode.Code", err)
	}
	return ec
}

func TestCreateOrder_InvalidParam(t *testing.T) {
	cases := []struct {
		name string
		in   CreateOrderIn
	}{
		{"缺 MchOrderNo", CreateOrderIn{Amount: 1000, Currency: "INR"}},
		{"金额非正", CreateOrderIn{MchOrderNo: "mch-1", Amount: 0, Currency: "INR"}},
		{"缺 Currency", CreateOrderIn{MchOrderNo: "mch-1", Amount: 1000}},
	}

	svc := New(Config{}, Deps{}, testLogger())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.CreateOrder(context.Background(), testMerchant(), tc.in)
			if ec := errCode(t, err); ec.Code != errcode.ErrInvalidParameter.Code {
				t.Fatalf("code = %d, want %d", ec.Code, errcode.ErrInvalidParameter.Code)
			}
		})
	}
}

func TestCreateOrder_AmountOutOfMerchantLimit(t *testing.T) {
	svc := New(Config{}, Deps{Bindings: &mockBindingRepo{}}, testLogger())

	in := baseIn()
	in.Amount = 999999 // 超出 testMerchant 的 LimitMax=100000
	_, _, err := svc.CreateOrder(context.Background(), testMerchant(), in)

	if ec := errCode(t, err); ec.Code != ErrAmountOutOfLimit.Code {
		t.Fatalf("code = %d, want %d", ec.Code, ErrAmountOutOfLimit.Code)
	}
}

func TestCreateOrder_ChannelNameFilteredEmpty(t *testing.T) {
	svc := New(Config{}, Deps{
		Bindings: &mockBindingRepo{
			listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
				return []model.ChannelInstance{instance(1, "wechat", 100, 100000)}, nil
			},
		},
	}, testLogger())

	in := baseIn()
	in.ChannelName = "alipay" // 候选里没有这个渠道
	_, _, err := svc.CreateOrder(context.Background(), testMerchant(), in)

	if ec := errCode(t, err); ec.Code != ErrChannelNotBound.Code {
		t.Fatalf("code = %d, want %d", ec.Code, ErrChannelNotBound.Code)
	}
}

func TestCreateOrder_InstanceLimitFilteredEmpty(t *testing.T) {
	svc := New(Config{}, Deps{
		Bindings: &mockBindingRepo{
			listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
				// 实例限额上限低于下单金额，未指定 channel_name 时应统统被筛掉。
				return []model.ChannelInstance{instance(1, "wechat", 100, 500)}, nil
			},
		},
	}, testLogger())

	_, _, err := svc.CreateOrder(context.Background(), testMerchant(), baseIn()) // Amount=1000

	if ec := errCode(t, err); ec.Code != ErrAmountOutOfLimit.Code {
		t.Fatalf("code = %d, want %d", ec.Code, ErrAmountOutOfLimit.Code)
	}
}

func TestCreateOrder_FirstFailsSecondSucceeds(t *testing.T) {
	var calls []string
	orders := &mockOrderRepo{
		create: func(_ context.Context, o *model.PaymentOrder) error {
			calls = append(calls, "create")
			if o.Status != model.OrderStatusCreated {
				t.Fatalf("Create 时 Status = %d, want %d（先落单要求初始状态为已创建）", o.Status, model.OrderStatusCreated)
			}
			return nil
		},
		markSent: func(_ context.Context, _ string, instanceID int64, outOrderNo, payURL, _ string) (bool, error) {
			calls = append(calls, "markSent")
			if instanceID != 2 {
				t.Fatalf("MarkSent instanceID = %d, want 2（应为成功的实例 B）", instanceID)
			}
			if payURL != "https://pay.example/b" {
				t.Fatalf("MarkSent payURL = %q, want B 的支付链接", payURL)
			}
			_ = outOrderNo
			return true, nil
		},
	}
	channel := &mockChannelClient{
		createOrder: func(_ context.Context, r channelclient.Route, _ channelclient.OrderIn) (channelclient.OrderOut, error) {
			calls = append(calls, "channel:"+r.ChannelName)
			if r.ChannelName == "a" {
				return channelclient.OrderOut{}, errors.New("渠道 A 下单失败")
			}
			return channelclient.OrderOut{PayURL: "https://pay.example/b", OutOrderNo: "out-b"}, nil
		},
	}
	svc := New(Config{CallbackBaseURL: "https://cb.example"}, Deps{
		Bindings: &mockBindingRepo{
			listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
				return []model.ChannelInstance{
					instance(1, "a", 100, 100000),
					instance(2, "b", 100, 100000),
				}, nil
			},
		},
		Orders:  orders,
		Channel: channel,
	}, testLogger())

	orderNo, payURL, err := svc.CreateOrder(context.Background(), testMerchant(), baseIn())
	if err != nil {
		t.Fatalf("CreateOrder() error = %v, want nil", err)
	}
	if orderNo == "" {
		t.Fatal("orderNo 为空")
	}
	if payURL != "https://pay.example/b" {
		t.Fatalf("payURL = %q, want B 的支付链接", payURL)
	}

	want := []string{"create", "channel:a", "channel:b", "markSent"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
}

func TestCreateOrder_AllChannelsFail(t *testing.T) {
	markFailedCalled := false
	orders := &mockOrderRepo{
		create: func(context.Context, *model.PaymentOrder) error { return nil },
		markFailedDispatch: func(_ context.Context, orderNo string) (bool, error) {
			markFailedCalled = true
			if orderNo == "" {
				t.Fatal("MarkFailedDispatch orderNo 为空")
			}
			return true, nil
		},
	}
	channel := &mockChannelClient{
		createOrder: func(context.Context, channelclient.Route, channelclient.OrderIn) (channelclient.OrderOut, error) {
			return channelclient.OrderOut{}, errors.New("下游渠道异常")
		},
	}
	svc := New(Config{}, Deps{
		Bindings: &mockBindingRepo{
			listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
				return []model.ChannelInstance{instance(1, "a", 100, 100000)}, nil
			},
		},
		Orders:  orders,
		Channel: channel,
	}, testLogger())

	_, _, err := svc.CreateOrder(context.Background(), testMerchant(), baseIn())

	if ec := errCode(t, err); ec.Code != ErrNoAvailableChannel.Code {
		t.Fatalf("code = %d, want %d", ec.Code, ErrNoAvailableChannel.Code)
	}
	if !markFailedCalled {
		t.Fatal("MarkFailedDispatch 未被调用")
	}
}

func TestCreateOrder_DuplicateMchOrderNo(t *testing.T) {
	orders := &mockOrderRepo{
		create: func(context.Context, *model.PaymentOrder) error { return repo.ErrDuplicate },
		findForMerchant: func(_ context.Context, _ int64, orderNo, mchOrderNo string) (*model.PaymentOrder, error) {
			if orderNo != "" || mchOrderNo != "mch-1" {
				t.Fatalf("FindForMerchant(%q, %q)，want (\"\", \"mch-1\")", orderNo, mchOrderNo)
			}
			return &model.PaymentOrder{}, nil // 命中：确有此商户订单号
		},
	}
	svc := New(Config{}, Deps{
		Bindings: &mockBindingRepo{
			listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
				return []model.ChannelInstance{instance(1, "a", 100, 100000)}, nil
			},
		},
		Orders: orders,
	}, testLogger())

	_, _, err := svc.CreateOrder(context.Background(), testMerchant(), baseIn())

	if ec := errCode(t, err); ec.Code != ErrDuplicateOrder.Code {
		t.Fatalf("code = %d, want %d", ec.Code, ErrDuplicateOrder.Code)
	}
}

func TestCreateOrder_OrderNoCollisionRetrySucceeds(t *testing.T) {
	createCalls := 0
	orders := &mockOrderRepo{
		create: func(context.Context, *model.PaymentOrder) error {
			createCalls++
			if createCalls == 1 {
				return repo.ErrDuplicate // 第一次撞号（模拟 order_no 碰撞）
			}
			return nil // 重试第二次成功
		},
		findForMerchant: func(context.Context, int64, string, string) (*model.PaymentOrder, error) {
			return nil, repo.ErrRowNotFound // 商户订单号本身并未重复，说明撞的是 order_no
		},
		markSent: func(context.Context, string, int64, string, string, string) (bool, error) {
			return true, nil
		},
	}
	channel := &mockChannelClient{
		createOrder: func(context.Context, channelclient.Route, channelclient.OrderIn) (channelclient.OrderOut, error) {
			return channelclient.OrderOut{PayURL: "https://pay.example/a"}, nil
		},
	}
	svc := New(Config{}, Deps{
		Bindings: &mockBindingRepo{
			listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
				return []model.ChannelInstance{instance(1, "a", 100, 100000)}, nil
			},
		},
		Orders:  orders,
		Channel: channel,
	}, testLogger())

	orderNo, payURL, err := svc.CreateOrder(context.Background(), testMerchant(), baseIn())
	if err != nil {
		t.Fatalf("CreateOrder() error = %v, want nil", err)
	}
	if orderNo == "" {
		t.Fatal("orderNo 为空")
	}
	if payURL != "https://pay.example/a" {
		t.Fatalf("payURL = %q, want https://pay.example/a", payURL)
	}
	if createCalls != 2 {
		t.Fatalf("Create 调用次数 = %d, want 2（重试一次）", createCalls)
	}
}

func TestCreateOrder_MarkSentReturnsFalseStillReturnsPayURL(t *testing.T) {
	orders := &mockOrderRepo{
		create: func(context.Context, *model.PaymentOrder) error { return nil },
		markSent: func(context.Context, string, int64, string, string, string) (bool, error) {
			return false, nil // 并发回调已推进，非本次调用的错误
		},
	}
	channel := &mockChannelClient{
		createOrder: func(context.Context, channelclient.Route, channelclient.OrderIn) (channelclient.OrderOut, error) {
			return channelclient.OrderOut{PayURL: "https://pay.example/a"}, nil
		},
	}
	svc := New(Config{}, Deps{
		Bindings: &mockBindingRepo{
			listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
				return []model.ChannelInstance{instance(1, "a", 100, 100000)}, nil
			},
		},
		Orders:  orders,
		Channel: channel,
	}, testLogger())

	_, payURL, err := svc.CreateOrder(context.Background(), testMerchant(), baseIn())
	if err != nil {
		t.Fatalf("CreateOrder() error = %v, want nil", err)
	}
	if payURL != "https://pay.example/a" {
		t.Fatalf("payURL = %q, want https://pay.example/a", payURL)
	}
}

func TestCreateOrder_MarkSentErrorWrapsInternal(t *testing.T) {
	orders := &mockOrderRepo{
		create: func(context.Context, *model.PaymentOrder) error { return nil },
		markSent: func(context.Context, string, int64, string, string, string) (bool, error) {
			return false, errors.New("写库异常")
		},
	}
	channel := &mockChannelClient{
		createOrder: func(context.Context, channelclient.Route, channelclient.OrderIn) (channelclient.OrderOut, error) {
			// 渠道已受理下单（已生成 pay_url/out_order_no），但本地 MarkSent 写库失败：
			// 属于需要人工对账的资金路径缺口（渠道有单、本地无痕），行为上仍判失败，
			// 只在实现里补了日志留痕，此处锁定返回码不变。
			return channelclient.OrderOut{PayURL: "https://pay.example/a", OutOrderNo: "out-a"}, nil
		},
	}
	svc := New(Config{}, Deps{
		Bindings: &mockBindingRepo{
			listCandidates: func(context.Context, int64, string) ([]model.ChannelInstance, error) {
				return []model.ChannelInstance{instance(1, "a", 100, 100000)}, nil
			},
		},
		Orders:  orders,
		Channel: channel,
	}, testLogger())

	_, _, err := svc.CreateOrder(context.Background(), testMerchant(), baseIn())

	if ec := errCode(t, err); ec.Code != errcode.ErrInternal.Code {
		t.Fatalf("code = %d, want %d", ec.Code, errcode.ErrInternal.Code)
	}
}

func TestFee(t *testing.T) {
	cases := []struct {
		name   string
		amount int64
		rate   int32
		extra  int32
		want   int64
	}{
		{"500 * 30‰ = 15", 500, 30, 0, 15},
		{"999 * 25‰ 四舍五入 = 25", 999, 25, 0, 25},
		{"带 extra", 1000, 10, 5, 15},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fee(tc.amount, tc.rate, tc.extra); got != tc.want {
				t.Fatalf("fee(%d, %d, %d) = %d, want %d", tc.amount, tc.rate, tc.extra, got, tc.want)
			}
		})
	}
}
