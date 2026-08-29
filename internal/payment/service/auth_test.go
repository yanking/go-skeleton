package service

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/sign"
	"github.com/yanking/go-skeleton/pkg/errcode"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// mockMerchantRepo 手写 mock：函数字段结构体实现 MerchantRepo。
type mockMerchantRepo struct {
	findByAppID func(ctx context.Context, appID string) (*model.Merchant, error)
	findByID    func(ctx context.Context, id int64) (*model.Merchant, error)
}

func (m *mockMerchantRepo) FindByAppID(ctx context.Context, appID string) (*model.Merchant, error) {
	return m.findByAppID(ctx, appID)
}

func (m *mockMerchantRepo) FindByID(ctx context.Context, id int64) (*model.Merchant, error) {
	if m.findByID == nil {
		panic("未预期调用 FindByID")
	}
	return m.findByID(ctx, id)
}

// errRowNotFound 模拟 repo 层「未命中」哨兵，测试只关心 Authenticate 是否把
// 任意查询失败统一收拢为未认证，不依赖 repo 包的具体哨兵类型。
var errRowNotFound = errors.New("记录不存在")

// signedFields 构造一组签名字段：固定 app_id 与给定时间戳，用 merchant 的密钥算出合法签名。
func signedFields(appID, secret string, ts time.Time) (map[string]string, string) {
	fields := map[string]string{
		"app_id":    appID,
		"timestamp": strconv.FormatInt(ts.UnixMilli(), 10),
	}
	sig := sign.HMAC(secret, sign.Canonical(fields))
	return fields, sig
}

// ctxWithIP 构造带 x-forwarded-for 头的 incoming context，模拟经反向代理转发的来源 IP。
func ctxWithIP(ip string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("x-forwarded-for", ip+", 10.0.0.1"))
}

func TestAuthenticate(t *testing.T) {
	const (
		appID  = "app-1"
		secret = "s3cret"
	)

	cases := []struct {
		name        string
		merchant    *model.Merchant
		findErr     error
		clientIP    string
		tsOffset    time.Duration // 请求时间戳相对当前时间的偏移
		badSig      bool
		wantErrCode int  // 0 表示不校验错误码（配合 wantOK 使用）
		wantOK      bool // 是否期望鉴权通过
	}{
		{
			name:        "app_id 未命中",
			findErr:     errRowNotFound,
			clientIP:    "1.2.3.4",
			wantErrCode: errcode.ErrUnauthenticated.Code,
		},
		{
			name: "IP 不在白名单——精确匹配，1.2.3.41 不因前缀命中 1.2.3.4 被放行",
			merchant: &model.Merchant{
				AppID: appID, AppSecret: secret, Status: model.MerchantStatusNormal,
				IPWhitelist: `["1.2.3.4"]`,
			},
			clientIP:    "1.2.3.41",
			wantErrCode: errcode.ErrUnauthenticated.Code,
		},
		{
			name: "白名单为空——不限制来源，放行",
			merchant: &model.Merchant{
				AppID: appID, AppSecret: secret, Status: model.MerchantStatusNormal,
				IPWhitelist: "",
			},
			clientIP: "9.9.9.9",
			wantOK:   true,
		},
		{
			name: "时间戳偏差 6 分钟",
			merchant: &model.Merchant{
				AppID: appID, AppSecret: secret, Status: model.MerchantStatusNormal,
				IPWhitelist: `["1.2.3.4"]`,
			},
			clientIP:    "1.2.3.4",
			tsOffset:    -6 * time.Minute,
			wantErrCode: errcode.ErrUnauthenticated.Code,
		},
		{
			name: "签名错误",
			merchant: &model.Merchant{
				AppID: appID, AppSecret: secret, Status: model.MerchantStatusNormal,
				IPWhitelist: `["1.2.3.4"]`,
			},
			clientIP:    "1.2.3.4",
			badSig:      true,
			wantErrCode: errcode.ErrUnauthenticated.Code,
		},
		{
			name: "商户已封禁",
			merchant: &model.Merchant{
				AppID: appID, AppSecret: secret, Status: model.MerchantStatusBanned,
				IPWhitelist: `["1.2.3.4"]`,
			},
			clientIP:    "1.2.3.4",
			wantErrCode: ErrMerchantRestricted.Code,
		},
		{
			name: "全部校验通过",
			merchant: &model.Merchant{
				AppID: appID, AppSecret: secret, Status: model.MerchantStatusNormal,
				IPWhitelist: `["1.2.3.4"]`,
			},
			clientIP: "1.2.3.4",
			wantOK:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &mockMerchantRepo{
				findByAppID: func(_ context.Context, gotAppID string) (*model.Merchant, error) {
					if gotAppID != appID {
						t.Fatalf("app_id = %q, want %q", gotAppID, appID)
					}
					if tc.findErr != nil {
						return nil, tc.findErr
					}
					return tc.merchant, nil
				},
			}
			svc := New(Config{}, Deps{Merchants: repo}, testLogger())

			fields, sig := signedFields(appID, secret, time.Now().Add(tc.tsOffset))
			if tc.badSig {
				sig = strings.Repeat("0", 64) // 长度与合法 HMAC-SHA256 十六进制签名一致，但内容错误
			}

			got, err := svc.Authenticate(ctxWithIP(tc.clientIP), fields, sig)

			if tc.wantOK {
				if err != nil {
					t.Fatalf("Authenticate() error = %v, want nil", err)
				}
				if got != tc.merchant {
					t.Fatalf("Authenticate() merchant = %v, want %v", got, tc.merchant)
				}
				return
			}

			if err == nil {
				t.Fatalf("Authenticate() error = nil, want code %d", tc.wantErrCode)
			}
			var ec errcode.Code
			if !errors.As(err, &ec) {
				t.Fatalf("Authenticate() error = %v, want errcode.Code", err)
			}
			if ec.Code != tc.wantErrCode {
				t.Fatalf("Authenticate() code = %d, want %d", ec.Code, tc.wantErrCode)
			}
		})
	}
}

func TestClientIP(t *testing.T) {
	t.Run("取 x-forwarded-for 首跳", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-forwarded-for", "1.2.3.4, 10.0.0.1"))
		if got := clientIP(ctx); got != "1.2.3.4" {
			t.Fatalf("clientIP() = %q, want %q", got, "1.2.3.4")
		}
	})

	t.Run("缺省回退 peer.FromContext", func(t *testing.T) {
		ctx := peer.NewContext(context.Background(), &peer.Peer{
			Addr: &net.TCPAddr{IP: net.ParseIP("5.6.7.8"), Port: 12345},
		})
		if got := clientIP(ctx); got != "5.6.7.8" {
			t.Fatalf("clientIP() = %q, want %q", got, "5.6.7.8")
		}
	})

	t.Run("两者皆无返回空串", func(t *testing.T) {
		if got := clientIP(context.Background()); got != "" {
			t.Fatalf("clientIP() = %q, want empty", got)
		}
	})
}

func testLogger() *slog.Logger { return slog.Default() }
