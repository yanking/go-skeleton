package payapay

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
)

type paymentOrderResult struct {
	baseResult
	PayURL     string `json:"pay_url"`
	DisOrderNo string `json:"dis_order_no"`
}

// PaymentOrder 代收下单。
func (a *Client) PaymentOrder(ctx context.Context, in adapter.PaymentOrderIn) (adapter.PaymentOrderOut, error) {
	params := map[string]any{
		"trade_no":       a.conf.MerID,
		"app_id":         a.conf.AppID,
		"pay_code":       a.conf.PayInCode,
		"pay_method":     "india",
		"price":          in.Amount,
		"order_no":       in.OrderNo,
		"pay_notice_url": in.NotifyURL,
		"user_id":        fmt.Sprintf("%d", a.conf.MerID),
		"user_ip":        generateIndiaIP(),
		"attach":         `{"account_no":"1234567890","account_type":"UPI"}`,
	}

	var result paymentOrderResult
	body, err := call(ctx, a, a.conf.APIs.Payment, params, &result)
	if err != nil {
		return adapter.PaymentOrderOut{}, err
	}
	if result.PayURL == "" {
		return adapter.PaymentOrderOut{}, fmt.Errorf("%w: 渠道未返回支付链接, body 缺 pay_url", adapter.ErrChannelRejected)
	}
	return adapter.PaymentOrderOut{
		URL:        result.PayURL,
		OutOrderNo: result.DisOrderNo,
		Response:   body,
	}, nil
}

type payoutOrderResult struct {
	baseResult
	DisOrderNo string `json:"dis_order_no"`
}

// PayoutOrder 代付下单。
func (a *Client) PayoutOrder(ctx context.Context, in adapter.PayoutOrderIn) (adapter.PayoutOrderOut, error) {
	params := map[string]any{
		"trade_no":       a.conf.MerID,
		"order_no":       in.OrderNo,
		"app_id":         a.conf.AppID,
		"pay_code":       a.conf.PayOutCode,
		"price":          in.Amount,
		"account_type":   "BANK",
		"bank_code":      "INR_BANK",
		"account_no":     in.AccountNo,
		"account_name":   in.Name,
		"identify_type":  "IFSC",
		"identify_num":   in.BankCode,
		"pay_notice_url": in.NotifyURL,
		"user_ip":        generateIndiaIP(),
	}

	var result payoutOrderResult
	body, err := call(ctx, a, a.conf.APIs.Payout, params, &result)
	if err != nil {
		return adapter.PayoutOrderOut{}, err
	}
	return adapter.PayoutOrderOut{OutOrderNo: result.DisOrderNo, Response: body}, nil
}
