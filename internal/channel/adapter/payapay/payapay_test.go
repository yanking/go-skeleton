package payapay

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"testing"
)

func TestCreateSign_SortsAndSkips(t *testing.T) {
	params := map[string]any{
		"b":    "2",
		"a":    "1",
		"sign": "should_be_ignored",
		"c":    "",
	}

	got := createSign(params, "secret")

	sum := md5.Sum([]byte("a=1&b=2&key=secret"))
	want := hex.EncodeToString(sum[:])

	if got != want {
		t.Fatalf("createSign mismatch, got=%s want=%s", got, want)
	}
}

func TestCreateSign_EmptyValuesNotSigned(t *testing.T) {
	params := map[string]any{
		"b": "",
		"a": "1",
	}

	got := createSign(params, "secret")

	sum := md5.Sum([]byte("a=1&key=secret"))
	want := hex.EncodeToString(sum[:])

	if got != want {
		t.Fatalf("createSign mismatch, got=%s want=%s", got, want)
	}
}

func TestCreateSign_IntValues(t *testing.T) {
	params := map[string]any{
		"b": 2,
		"a": 1,
	}

	got := createSign(params, "secret")

	sum := md5.Sum([]byte("a=1&b=2&key=secret"))
	want := hex.EncodeToString(sum[:])

	if got != want {
		t.Fatalf("createSign mismatch, got=%s want=%s", got, want)
	}
}

func TestPaymentCallback_VerifyAndMap(t *testing.T) {
	a, err := New(httpc.New(httpc.Config{}), []byte(`{"base_url":"https://x","app_secret":"secret"}`))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fields := map[string]any{
		"order_no":     "P1",
		"dis_order_no": "D1",
		"status":       "2",
		"real_price":   "10000",
		"sign":         "",
	}
	fields["sign"] = createSign(fields, "secret")
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out, err := a.PaymentCallback(nil, nil, string(data))
	if err != nil {
		t.Fatalf("PaymentCallback: %v", err)
	}
	if out.OrderNo != "P1" || out.OutOrderNo != "D1" || out.CallbackType != 1 || out.Amount != 10000 {
		t.Fatalf("unexpected out: %+v", out)
	}

	// 篡改金额后验签必须失败。
	fields["real_price"] = "99999"
	tampered, _ := json.Marshal(fields)
	if _, err := a.PaymentCallback(nil, nil, string(tampered)); err == nil {
		t.Fatal("tampered callback should fail verification")
	}
}

func TestPayoutCallback_UnknownStatus(t *testing.T) {
	a, _ := New(httpc.New(httpc.Config{}), []byte(`{"base_url":"https://x","app_secret":"secret"}`))

	fields := map[string]any{"order_no": "P1", "status": "5", "order_price": "1"}
	fields["sign"] = createSign(fields, "secret")
	data, _ := json.Marshal(fields)

	if _, err := a.PayoutCallback(nil, nil, string(data)); err == nil {
		t.Fatal("unknown status should be rejected")
	}
}
