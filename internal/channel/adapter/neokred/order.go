package neokred

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
)

// orderResult 下单响应：data 内为渠道单号与支付链接。
type orderResult struct {
	baseResult
	Data struct {
		UpiIntentString string `json:"upiIntentString"`
		TransactionID   string `json:"transactionId"`
		TransferID      string `json:"transferId"`
	} `json:"data"`
}

// amountYuan 金额分转渠道要求的元字符串（两位小数）。
func amountYuan(cents int64) string {
	return strconv.FormatFloat(float64(cents)/100, 'f', 2, 64)
}

// PaymentOrder 代收下单：UPI 二维码，返回 upiIntentString 作为支付链接。
func (a *Client) PaymentOrder(ctx context.Context, in adapter.PaymentOrderIn) (adapter.PaymentOrderOut, error) {
	header := map[string]string{
		"client_secret": a.conf.Payment.ClientSecret,
		"program_id":    a.conf.Payment.ProgramID,
	}
	params := map[string]any{
		"amount":      amountYuan(in.Amount),
		"orderId":     in.OrderNo,
		"expireAfter": "30",
		"remark":      in.OrderNo,
	}

	code, body, err := a.http.PostJSON(ctx, a.conf.Payment.APIs.Order, header, params, 0)
	if err != nil {
		return adapter.PaymentOrderOut{}, fmt.Errorf("%w: %v", adapter.ErrChannelRejected, err)
	}
	if code != 200 {
		return adapter.PaymentOrderOut{}, fmt.Errorf("%w: http %d, body %s", adapter.ErrChannelRejected, code, body)
	}

	var result orderResult
	if err := unmarshalResult(body, &result.baseResult, &result); err != nil {
		return adapter.PaymentOrderOut{}, err
	}
	return adapter.PaymentOrderOut{
		URL:               result.Data.UpiIntentString,
		OutOrderNo:        result.Data.TransactionID,
		Response:          body,
		TripartiteAccount: a.conf.Email,
	}, nil
}

// PayoutOrder 代付下单：IMPS 出款，超过十万（分单位 10000000）走 RTGS。
func (a *Client) PayoutOrder(ctx context.Context, in adapter.PayoutOrderIn) (adapter.PayoutOrderOut, error) {
	// 渠道要求本地手机号，去掉国际区号前缀 91。
	phone := in.Phone
	if len(phone) > 10 && strings.HasPrefix(phone, "91") {
		phone = phone[2:]
	}

	header := map[string]string{
		"client_secret": a.conf.Payout.ClientSecret,
		"program_id":    a.conf.Payout.ProgramID,
	}
	params := map[string]any{
		"amount":       amountYuan(in.Amount),
		"transferMode": "IMPS",
		"beneDetails": map[string]string{
			"bankName":    in.BankName,
			"bankAccount": in.AccountNo,
			"ifsc":        in.BankCode,
			"phone":       phone,
			"name":        in.Name,
			"email":       in.Email,
		},
		"remarks": in.OrderNo,
	}
	if in.Amount > 10000000 {
		params["transferMode"] = "RTGS"
	}

	code, body, err := a.http.PostJSON(ctx, a.conf.Payout.APIs.Order, header, params, 0)
	if err != nil {
		return adapter.PayoutOrderOut{}, fmt.Errorf("%w: %v", adapter.ErrChannelRejected, err)
	}
	if code != 200 {
		return adapter.PayoutOrderOut{}, fmt.Errorf("%w: http %d, body %s", adapter.ErrChannelRejected, code, body)
	}

	var result orderResult
	if err := unmarshalResult(body, &result.baseResult, &result); err != nil {
		return adapter.PayoutOrderOut{}, err
	}
	return adapter.PayoutOrderOut{
		OutOrderNo:        result.Data.TransferID,
		Response:          body,
		TripartiteAccount: a.conf.Email,
	}, nil
}
