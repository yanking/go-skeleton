package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/sign"
	"github.com/yanking/go-skeleton/pkg/errcode"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// authWindow 请求时间戳允许的最大偏差：客户端与服务器时钟误差容忍窗口，
// 超出视为疑似重放或伪造请求。
const authWindow = 5 * time.Minute

// Authenticate 商户鉴权：固定顺序执行——查商户→IP 白名单→时间戳窗口→签名验证→商户状态。
// 查商户失败与签名验证失败对外统一返回未认证（10004），不区分具体原因：
// 若分别暴露不同错误码，攻击者可据此探测某个 app_id 是否存在，故在此收拢。
// 底层错误只用 errcode.Wrap 挂 cause 进日志链，不体现在返回给客户端的 Message 中。
func (s *Payment) Authenticate(ctx context.Context, fields map[string]string, sig string) (*model.Merchant, error) {
	merchant, err := s.deps.Merchants.FindByAppID(ctx, fields["app_id"])
	if err != nil {
		return nil, errcode.Wrap(err, errcode.ErrUnauthenticated)
	}

	if err := checkIPWhitelist(merchant.IPWhitelist, clientIP(ctx)); err != nil {
		return nil, err
	}

	if err := checkTimestamp(fields["timestamp"]); err != nil {
		return nil, err
	}

	if !sign.Verify(merchant.AppSecret, fields, sig) {
		return nil, errcode.Wrap(errors.New("签名校验未通过"), errcode.ErrUnauthenticated)
	}

	if merchant.Status == model.MerchantStatusBanned {
		return nil, ErrMerchantRestricted
	}

	return merchant, nil
}

// checkIPWhitelist 校验 ip 是否在白名单内，逐项精确匹配（禁子串匹配，避免
// "1.2.3.4" 误放行 "1.2.3.41"）。白名单为空（未设置或空数组）视为不限制来源，直接放行。
func checkIPWhitelist(rawWhitelist, ip string) error {
	if rawWhitelist == "" {
		return nil
	}

	var whitelist []string
	if err := json.Unmarshal([]byte(rawWhitelist), &whitelist); err != nil {
		return errcode.Wrap(err, errcode.ErrUnauthenticated)
	}
	if len(whitelist) == 0 {
		return nil
	}

	for _, w := range whitelist {
		if w == ip {
			return nil
		}
	}
	return errcode.ErrUnauthenticated
}

// checkTimestamp 校验请求携带的毫秒时间戳与服务器当前时间的偏差是否在 authWindow 内。
func checkTimestamp(rawTimestamp string) error {
	ms, err := strconv.ParseInt(rawTimestamp, 10, 64)
	if err != nil {
		return errcode.Wrap(err, errcode.ErrUnauthenticated)
	}

	drift := time.Since(time.UnixMilli(ms))
	if math.Abs(drift.Seconds()) > authWindow.Seconds() {
		return errcode.ErrUnauthenticated
	}
	return nil
}

// clientIP 提取调用方来源 IP：优先取 x-forwarded-for 首个跳点（经反向代理时最贴近
// 真实客户端，逗号分隔取第一段、去首尾空白）；metadata 缺失该头时回退取 gRPC
// 连接的对端地址（无反代直连场景）。
func clientIP(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get("x-forwarded-for"); len(values) > 0 {
			hop, _, _ := strings.Cut(values[0], ",")
			return strings.TrimSpace(hop)
		}
	}

	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
			return host
		}
		return p.Addr.String()
	}
	return ""
}
