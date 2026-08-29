package payapay

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
)

// stringifyCallback 回调报文按 UseNumber 解成 map 后统一字符串化，
// 数值保持字面形式不丢精度，供签名重算使用。
func stringifyCallback(data string) (map[string]string, error) {
	raw := make(map[string]any)
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: 回调报文非 JSON: %v", adapter.ErrBadResponse, err)
	}

	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			out[k] = val
		case json.Number:
			out[k] = val.String()
		case float64:
			out[k] = fmt.Sprintf("%.0f", val)
		default:
			out[k] = fmt.Sprintf("%v", val)
		}
	}
	return out, nil
}

// verifyCallback 验签并抽取公共字段。
func (a *Client) verifyCallback(data string) (map[string]string, error) {
	fields, err := stringifyCallback(data)
	if err != nil {
		return nil, err
	}

	signParams := make(map[string]any, len(fields))
	for k, v := range fields {
		signParams[k] = v
	}
	if sign := createSign(signParams, a.conf.AppSecret); sign != fields["sign"] {
		return nil, fmt.Errorf("%w: 我方 %s, 渠道 %s", adapter.ErrVerifyFailed, sign, fields["sign"])
	}
	return fields, nil
}

// parseCallbackAmount 解析回调金额；解析失败按渠道响应问题处理。
func parseCallbackAmount(fields map[string]string, key string) (int64, error) {
	amount, err := strconv.ParseInt(fields[key], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: 解析回调金额 %s=%q: %v", adapter.ErrBadResponse, key, fields[key], err)
	}
	return amount, nil
}

// PaymentCallback 代收回调验签：status 2 成功取 real_price，3 失败取 order_price。
func (a *Client) PaymentCallback(_ context.Context, _ map[string]string, data string) (adapter.CallbackOut, error) {
	fields, err := a.verifyCallback(data)
	if err != nil {
		return adapter.CallbackOut{}, err
	}

	var callbackType int32
	switch fields["status"] {
	case "2":
		callbackType = adapter.CallbackSuccess
	case "3":
		callbackType = adapter.CallbackFailure
	default:
		return adapter.CallbackOut{}, fmt.Errorf("%w: payment status %q", adapter.ErrUnknownCallbackStatus, fields["status"])
	}

	amountKey := "order_price"
	if callbackType == adapter.CallbackSuccess {
		amountKey = "real_price"
	}
	amount, err := parseCallbackAmount(fields, amountKey)
	if err != nil {
		return adapter.CallbackOut{}, err
	}
	return adapter.CallbackOut{
		OrderNo:      fields["order_no"],
		OutOrderNo:   fields["dis_order_no"],
		CallbackType: callbackType,
		Amount:       amount,
	}, nil
}

// PayoutCallback 代付回调验签：status 2 成功，3/7/9 失败，金额取 order_price。
func (a *Client) PayoutCallback(_ context.Context, _ map[string]string, data string) (adapter.CallbackOut, error) {
	fields, err := a.verifyCallback(data)
	if err != nil {
		return adapter.CallbackOut{}, err
	}

	var callbackType int32
	switch fields["status"] {
	case "2":
		callbackType = adapter.CallbackSuccess
	case "3", "7", "9":
		callbackType = adapter.CallbackFailure
	default:
		return adapter.CallbackOut{}, fmt.Errorf("%w: payout status %q", adapter.ErrUnknownCallbackStatus, fields["status"])
	}

	amount, err := parseCallbackAmount(fields, "order_price")
	if err != nil {
		return adapter.CallbackOut{}, err
	}
	return adapter.CallbackOut{
		OrderNo:      fields["order_no"],
		OutOrderNo:   fields["dis_order_no"],
		CallbackType: callbackType,
		Amount:       amount,
	}, nil
}
