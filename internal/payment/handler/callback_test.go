package handler

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseCallbackPath 覆盖固定形状 "/callbacks/payment/{instanceID}" 的合法与非法路径。
func TestParseCallbackPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		want    int64
		wantErr bool
	}{
		{"合法路径", "/callbacks/payment/123", 123, false},
		{"负数实例ID仍是合法整数", "/callbacks/payment/-5", -5, false},
		{"缺失实例ID", "/callbacks/payment/", 0, true},
		{"实例ID非数字", "/callbacks/payment/abc", 0, true},
		{"末尾多余斜杠", "/callbacks/payment/123/", 0, true},
		{"多出一段路径", "/callbacks/payment/123/extra", 0, true},
		{"前缀不匹配", "/callbacks/other/123", 0, true},
		{"根路径", "/", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCallbackPath(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseCallbackPath(%q) error = %v, wantErr %v", tc.path, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("parseCallbackPath(%q) = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}

// TestExtractHeaders 同名多值取首个。
func TestExtractHeaders(t *testing.T) {
	r := httptest.NewRequest("POST", "/callbacks/payment/1", nil)
	r.Header.Add("X-Sign", "first")
	r.Header.Add("X-Sign", "second")
	r.Header.Set("Content-Type", "application/json")

	got := extractHeaders(r)

	if got["X-Sign"] != "first" {
		t.Fatalf("X-Sign = %q, want 首个值 first", got["X-Sign"])
	}
	if got["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got["Content-Type"])
	}
}

// TestReadCallbackBody 校验读取上限：超过 1MB 的请求体被截断到 maxCallbackBodyBytes。
func TestReadCallbackBody(t *testing.T) {
	t.Run("正常大小原样读取", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/callbacks/payment/1", strings.NewReader("raw-body"))
		if got := readCallbackBody(r); got != "raw-body" {
			t.Fatalf("readCallbackBody() = %q, want raw-body", got)
		}
	})

	t.Run("超过上限被截断", func(t *testing.T) {
		oversized := strings.Repeat("a", maxCallbackBodyBytes+100)
		r := httptest.NewRequest("POST", "/callbacks/payment/1", strings.NewReader(oversized))
		got := readCallbackBody(r)
		if len(got) != maxCallbackBodyBytes {
			t.Fatalf("len(readCallbackBody()) = %d, want %d", len(got), maxCallbackBodyBytes)
		}
	})
}

// TestCallbackClientIP 覆盖 X-Forwarded-For 首跳（带/不带端口）与回退 RemoteAddr 两条路径。
func TestCallbackClientIP(t *testing.T) {
	cases := []struct {
		name       string
		xff        string
		remoteAddr string
		want       string
	}{
		{"XFF单跳纯IP", "1.2.3.4", "9.9.9.9:5678", "1.2.3.4"},
		{"XFF首跳带端口去端口", "1.2.3.4:6666, 5.6.7.8", "9.9.9.9:5678", "1.2.3.4"},
		{"XFF多跳取第一段并去空白", "  1.2.3.4  ,5.6.7.8", "9.9.9.9:5678", "1.2.3.4"},
		{"无XFF回退RemoteAddr去端口", "", "9.9.9.9:5678", "9.9.9.9"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/callbacks/payment/1", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := callbackClientIP(r); got != tc.want {
				t.Fatalf("callbackClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}
