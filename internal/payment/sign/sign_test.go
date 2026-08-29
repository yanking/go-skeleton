package sign

import (
	"strings"
	"testing"

	paymentv1 "github.com/yanking/go-skeleton/gen/payment/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestCanonical(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "乱序字段按字段名升序拼接",
			fields: map[string]string{"b": "2", "a": "1"},
			want:   "a=1&b=2",
		},
		{
			name:   "空值字段仍参与拼接",
			fields: map[string]string{"b": "1", "a": ""},
			want:   "a=&b=1",
		},
		{
			name:   "三个字段升序拼接不带尾部&",
			fields: map[string]string{"amount": "500", "app_id": "demo", "notify_url": ""},
			want:   "amount=500&app_id=demo&notify_url=",
		},
		{
			name:   "空map返回空串",
			fields: map[string]string{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Canonical(tt.fields)
			if got != tt.want {
				t.Errorf("Canonical(%v) = %q, want %q", tt.fields, got, tt.want)
			}
		})
	}
}

func TestHMAC(t *testing.T) {
	tests := []struct {
		name      string
		secret    string
		canonical string
		want      string
	}{
		{
			// 预计算值：secret="test-secret"、canonical="a=1&b=2" 下，用一次性 Go 程序
			// （crypto/hmac + crypto/sha256）现算得到，并用 `openssl dgst -sha256 -hmac`
			// 交叉核对一致，钉进用例防回归。
			name:      "固定输入返回预计算的hex值",
			secret:    "test-secret",
			canonical: "a=1&b=2",
			want:      "ec7fe6e1524630e1bbf333bf640bd5bcfddb27fe2d122f1832ee46be17fec9b7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HMAC(tt.secret, tt.canonical)
			if got != tt.want {
				t.Errorf("HMAC(%q, %q) = %q, want %q", tt.secret, tt.canonical, got, tt.want)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	const secret = "test-secret"
	// 含空值字段 notify_url 的基准字段集，签名后作为各用例的正确签名。
	baseFields := map[string]string{"amount": "500", "notify_url": ""}
	validSig := HMAC(secret, Canonical(baseFields))

	tests := []struct {
		name   string
		secret string
		fields map[string]string
		sig    string
		want   bool
	}{
		{
			name:   "正确签名验签通过",
			secret: secret,
			fields: map[string]string{"amount": "500", "notify_url": ""},
			sig:    validSig,
			want:   true,
		},
		{
			name:   "篡改字段值验签失败",
			secret: secret,
			fields: map[string]string{"amount": "600", "notify_url": ""},
			sig:    validSig,
			want:   false,
		},
		{
			name:   "剥离空值字段验签失败",
			secret: secret,
			fields: map[string]string{"amount": "500"},
			sig:    validSig,
			want:   false,
		},
		{
			name:   "签名大小写变体验签失败",
			secret: secret,
			fields: map[string]string{"amount": "500", "notify_url": ""},
			sig:    strings.ToUpper(validSig),
			want:   false,
		},
		{
			// 空密钥的 HMAC 是任何人都能公开算出的常量：即便 sig 与「空密钥下的正确签名」
			// 完全吻合，也必须判失败，否则商户 app_secret 配成空值就等于对该商户不设防。
			name:   "空密钥验签失败",
			secret: "",
			fields: baseFields,
			sig:    HMAC("", Canonical(baseFields)),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Verify(tt.secret, tt.fields, tt.sig)
			if got != tt.want {
				t.Errorf("Verify(%q, %v, %q) = %v, want %v", tt.secret, tt.fields, tt.sig, got, tt.want)
			}
		})
	}
}

func TestFieldsFromProto(t *testing.T) {
	req := &paymentv1.CreatePaymentOrderRequest{
		AppId:     "demo",
		Amount:    500,
		Timestamp: 1756300800000,
		Sign:      "abc",
	}

	fields, sig, err := FieldsFromProto(req)
	if err != nil {
		t.Fatalf("FieldsFromProto() 返回意外错误: %v", err)
	}

	if sig != "abc" {
		t.Errorf("sig = %q, want %q", sig, "abc")
	}
	if _, ok := fields["sign"]; ok {
		t.Errorf("fields 不应包含 sign 字段: %v", fields)
	}
	wantSubset := map[string]string{
		"app_id":     "demo",
		"amount":     "500",
		"timestamp":  "1756300800000",
		"notify_url": "", // proto3 未设置字段取零值形态，仍参与签名
		"currency":   "",
		"payer_name": "",
	}
	for k, want := range wantSubset {
		got, ok := fields[k]
		if !ok {
			t.Errorf("fields 缺少字段 %q", k)
			continue
		}
		if got != want {
			t.Errorf("fields[%q] = %q, want %q", k, got, want)
		}
	}
	// CreatePaymentOrderRequest 共 11 个标量字段（含 sign），排除 sign 后应有 10 个。
	if len(fields) != 10 {
		t.Errorf("len(fields) = %d, want 10; fields=%v", len(fields), fields)
	}
}

// TestFieldsFromProto_不支持的标量类型拒签 验证 double/enum 等本包未支持的标量 Kind
// 会让 FieldsFromProto 返回 error，而不是静默跳过该字段（静默跳过会让该字段脱离签名
// 保护，可被任意篡改）。structpb.Value 的 oneof 首个成员 null_value 是 EnumKind，
// 零值消息即可稳定触发；该消息与本仓库已依赖的 google.golang.org/protobuf 同模块，
// 不引入新依赖。
func TestFieldsFromProto_不支持的标量类型拒签(t *testing.T) {
	_, _, err := FieldsFromProto(&structpb.Value{})
	if err == nil {
		t.Fatal("FieldsFromProto(structpb.Value{}) 应返回 error，实际为 nil")
	}
}

// TestSignEndToEnd 用固定的下单请求走完整链路 FieldsFromProto → Canonical → HMAC，
// 把最终 hex 钉成字面量：字段名或值的格式化方式一旦漂移（哪怕 Canonical/HMAC 本身
// 不变），这条用例也会先炸，防止「拼接细节改了但没人发现」的回归。
// 预计算值：用一次性 Go 程序调用本包三个函数现算 canonical 串与 hex，再用
// `openssl dgst -sha256 -hmac "e2e-secret"` 对同一 canonical 串交叉核对一致。
func TestSignEndToEnd(t *testing.T) {
	req := &paymentv1.CreatePaymentOrderRequest{
		AppId:       "demo",
		MchOrderNo:  "MCH20260827001",
		Amount:      500,
		Currency:    "INR",
		ChannelName: "payapay",
		NotifyUrl:   "https://merchant.example.com/notify",
		PayerName:   "Alice",
		PayerPhone:  "9876543210",
		PayerEmail:  "alice@example.com",
		Timestamp:   1756300800000,
		Sign:        "will-be-overwritten", // sign 字段不参与签名计算，取值无关
	}
	const secret = "e2e-secret"
	const wantCanonical = "amount=500&app_id=demo&channel_name=payapay&currency=INR&" +
		"mch_order_no=MCH20260827001&notify_url=https://merchant.example.com/notify&" +
		"payer_email=alice@example.com&payer_name=Alice&payer_phone=9876543210&" +
		"timestamp=1756300800000"
	const wantHMAC = "01e09276074fd5165f6d73aad2eb7321816536fb3cb25ab3a2d13812a0ae9564"

	fields, _, err := FieldsFromProto(req)
	if err != nil {
		t.Fatalf("FieldsFromProto() 返回意外错误: %v", err)
	}

	canonical := Canonical(fields)
	if canonical != wantCanonical {
		t.Fatalf("Canonical() = %q, want %q", canonical, wantCanonical)
	}

	got := HMAC(secret, canonical)
	if got != wantHMAC {
		t.Errorf("HMAC() = %q, want %q", got, wantHMAC)
	}
}
