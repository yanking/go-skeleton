// Package neokred 是 Neokred 渠道适配器：下单走渠道 API（client_secret/program_id
// 头），查询与余额走 Dashboard API（邮箱登录换 Bearer token，401 时单飞刷新重试）；
// 不提供回调，订单状态只经查询轮询获知。移植自 gateway-channel 仓库 neokred-001 分支。
package neokred

import (
	"encoding/json"
	"fmt"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
	"github.com/yanking/go-skeleton/pkg/httpc"
)

// Client Neokred 渠道客户端：实现 adapter.Adapter,封装令牌与渠道 API 报文。
type Client struct {
	conf  Platform
	http  *httpc.Client
	token *tokenHolder
}

// New 反序列化 platform 配置并构造适配器；配置不合法当场报错（装配期暴露）。
func New(hc *httpc.Client, platform json.RawMessage) (adapter.Adapter, error) {
	var conf Platform
	if err := json.Unmarshal(platform, &conf); err != nil {
		return nil, fmt.Errorf("解析 neokred platform 配置: %w", err)
	}
	if conf.DashboardAPIs.Login == "" || conf.Payment.APIs.Order == "" || conf.Payout.APIs.Order == "" {
		return nil, fmt.Errorf("neokred platform 配置缺 dashboard_apis 或下单 API")
	}
	return &Client{conf: conf, http: hc, token: newTokenHolder(conf, hc)}, nil
}

// Name 渠道名。
func (a *Client) Name() string { return "neokred" }

// baseResult Dashboard 类响应的公共状态字段；statusCode=200 为成功。
type baseResult struct {
	StatusCode int `json:"statusCode"`
}

// unmarshalResult 解析响应并校验渠道业务码。
func unmarshalResult(body string, base *baseResult, out any) error {
	if err := json.Unmarshal([]byte(body), out); err != nil {
		return fmt.Errorf("%w: %s: %v", adapter.ErrBadResponse, body, err)
	}
	if base.StatusCode != 200 {
		return fmt.Errorf("%w: 渠道业务失败, body %s", adapter.ErrChannelRejected, body)
	}
	return nil
}
