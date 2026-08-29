// Package payapay 是 PayaPay 渠道适配器：MD5 签名（按 key 排序拼 k=v&…&key=secret）、
// JSON POST 对接、渠道私有状态码映射。移植自 gateway-channel 仓库 payapay-016 分支。
package payapay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
	"github.com/yanking/go-skeleton/pkg/httpc"
)

// Client PayaPay 渠道客户端：实现 adapter.Adapter,封装签名与渠道 API 报文。
type Client struct {
	conf Platform
	http *httpc.Client
}

// New 反序列化 platform 配置并构造适配器；配置不合法当场报错（装配期暴露）。
func New(hc *httpc.Client, platform json.RawMessage) (adapter.Adapter, error) {
	var conf Platform
	if err := json.Unmarshal(platform, &conf); err != nil {
		return nil, fmt.Errorf("解析 payapay platform 配置: %w", err)
	}
	if conf.BaseURL == "" || conf.AppSecret == "" {
		return nil, fmt.Errorf("payapay platform 配置缺 base_url 或 app_secret")
	}
	return &Client{conf: conf, http: hc}, nil
}

// Name 渠道名。
func (a *Client) Name() string { return "payapay" }

// baseResult 各响应的公共字段；code=200 为渠道侧成功。
type baseResult struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// post 统一的「签名 → POST → 校验 HTTP 200 → 解析」前半程。
// 回调里的 reply 原文保留在出参 Response 字段，供网关侧留痕排障。
func (a *Client) post(ctx context.Context, path string, params map[string]any) (string, error) {
	params["sign"] = createSign(params, a.conf.AppSecret)

	code, body, err := a.http.PostJSON(ctx, a.conf.BaseURL+path, nil, params, 0)
	if err != nil {
		return "", fmt.Errorf("%w: %v", adapter.ErrChannelRejected, err)
	}
	if code != 200 {
		return "", fmt.Errorf("%w: http %d, body %s", adapter.ErrChannelRejected, code, body)
	}
	return body, nil
}

// call post 之后按渠道约定校验业务码并解析到 out，返回响应原文供出参留痕。
func call[T any](ctx context.Context, a *Client, path string, params map[string]any, out *T) (string, error) {
	body, err := a.post(ctx, path, params)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(body), out); err != nil {
		return "", fmt.Errorf("%w: %s: %v", adapter.ErrBadResponse, body, err)
	}
	var base baseResult
	if err := json.Unmarshal([]byte(body), &base); err != nil {
		return "", fmt.Errorf("%w: %s: %v", adapter.ErrBadResponse, body, err)
	}
	if base.Code != 200 {
		return "", fmt.Errorf("%w: 渠道业务失败, body %s", adapter.ErrChannelRejected, body)
	}
	return body, nil
}
