package payapay

import (
	"context"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
)

type queryResult struct {
	baseResult
	RealPrice  int64  `json:"real_price"`
	Status     int32  `json:"status"`
	OrderNo    string `json:"order_no"`
	DisOrderNo string `json:"dis_order_no"`
	Utr2       string `json:"utr2"`
}

// query 代收/代付查询共用：order_type 区分，渠道私有状态 2 成功 3 失败其余处理中。
func (a *Client) query(ctx context.Context, path, orderType string, in adapter.QueryIn) (adapter.QueryOut, error) {
	params := map[string]any{
		"order_type": orderType,
		"trade_no":   a.conf.MerID,
		"app_id":     a.conf.AppID,
		"order_no":   in.OrderNo,
	}

	var result queryResult
	body, err := call(ctx, a, path, params, &result)
	if err != nil {
		return adapter.QueryOut{}, err
	}

	var status int32
	switch result.Status {
	case 2:
		status = adapter.StatusSuccess
	case 3:
		status = adapter.StatusFailure
	default:
		status = adapter.StatusProcessing
	}
	return adapter.QueryOut{
		Status:      status,
		Amount:      result.RealPrice,
		OutOrderNo:  result.DisOrderNo,
		Response:    body,
		ReferenceNo: result.Utr2,
	}, nil
}

// PaymentQuery 代收查询。
func (a *Client) PaymentQuery(ctx context.Context, in adapter.QueryIn) (adapter.QueryOut, error) {
	return a.query(ctx, a.conf.APIs.PaymentQuery, "pay_in", in)
}

// PayoutQuery 代付查询。
func (a *Client) PayoutQuery(ctx context.Context, in adapter.QueryIn) (adapter.QueryOut, error) {
	return a.query(ctx, a.conf.APIs.PayoutQuery, "pay_out", in)
}

type balanceResult struct {
	baseResult
	Balance       int64 `json:"balance"`
	BalanceFrozen int64 `json:"balance_frozen"`
}

// BalanceQuery 商户余额查询。
func (a *Client) BalanceQuery(ctx context.Context) (adapter.BalanceOut, error) {
	params := map[string]any{
		"trade_no": a.conf.MerID,
		"app_id":   a.conf.AppID,
	}

	var result balanceResult
	if _, err := call(ctx, a, a.conf.APIs.BalanceQuery, params, &result); err != nil {
		return adapter.BalanceOut{}, err
	}
	return adapter.BalanceOut{Balance: result.Balance, FrozenBalance: result.BalanceFrozen}, nil
}
