package neokred

import (
	"context"
	"fmt"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
)

// txQueryResult Dashboard 查询响应：transactions 数组取首条。
type txQueryResult struct {
	baseResult
	Data struct {
		Transactions []struct {
			CustRefNo     string  `json:"custRefNo"`
			TransactionID string  `json:"transactionId"`
			ReferenceID   string  `json:"referenceId"`
			Utr           string  `json:"utr"`
			ActualAmount  float64 `json:"actual_amount"`
			Status        string  `json:"status"`
		} `json:"transactions"`
	} `json:"data"`
}

// dashboardQuery Dashboard API 查询：Bearer token 鉴权，401 时刷新 token 重试一次。
func (a *Client) dashboardQuery(ctx context.Context, params map[string]any) (*txQueryResult, string, error) {
	for attempt := 0; ; attempt++ {
		token, err := a.token.fetch(ctx)
		if err != nil {
			return nil, "", err
		}

		code, body, err := a.http.PostJSON(ctx, a.conf.DashboardAPIs.Query,
			map[string]string{"Authorization": token}, params, 0)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", adapter.ErrChannelRejected, err)
		}
		if code == 401 && attempt == 0 {
			if _, err := a.token.refresh(ctx); err != nil {
				return nil, "", err
			}
			continue
		}
		if code != 200 {
			return nil, "", fmt.Errorf("%w: http %d, body %s", adapter.ErrChannelRejected, code, body)
		}

		var result txQueryResult
		if err := unmarshalResult(body, &result.baseResult, &result); err != nil {
			return nil, "", err
		}
		if len(result.Data.Transactions) == 0 {
			return nil, "", fmt.Errorf("%w: 查询无交易记录, body %s", adapter.ErrChannelRejected, body)
		}
		return &result, body, nil
	}
}

// PaymentQuery 代收查询。失败类状态刻意映射为处理中：渠道存在先失败又成功的情况，
// 失败结论只承认成功或持续处理（原实现的既定语义，等价移植）。
func (a *Client) PaymentQuery(ctx context.Context, in adapter.QueryIn) (adapter.QueryOut, error) {
	if in.OutOrderNo == "" {
		return adapter.QueryOut{}, fmt.Errorf("%w: 代收查询须带 out_order_no", adapter.ErrBadResponse)
	}

	result, body, err := a.dashboardQuery(ctx, map[string]any{
		"page":                0,
		"size":                1,
		"serviceType":         "payin",
		"transactionId":       in.OutOrderNo,
		"serviceProviderName": "FINO",
	})
	if err != nil {
		return adapter.QueryOut{}, err
	}

	tx := result.Data.Transactions[0]
	status := adapter.StatusProcessing
	switch tx.Status {
	case "CREDITED", "DEBITED", "CREDIT", "SUCCESS":
		status = adapter.StatusSuccess
	}
	return adapter.QueryOut{
		Status:      status,
		Amount:      int64(tx.ActualAmount * 100),
		OutOrderNo:  tx.TransactionID,
		Response:    body,
		ReferenceNo: tx.CustRefNo,
	}, nil
}

// PayoutQuery 代付查询：处理/成功之外的未知状态按失败计。
func (a *Client) PayoutQuery(ctx context.Context, in adapter.QueryIn) (adapter.QueryOut, error) {
	if in.OutOrderNo == "" {
		return adapter.QueryOut{}, fmt.Errorf("%w: 代付查询须带 out_order_no", adapter.ErrBadResponse)
	}

	result, body, err := a.dashboardQuery(ctx, map[string]any{
		"page":                0,
		"size":                1,
		"serviceType":         "payout",
		"transactionId":       in.OutOrderNo,
		"serviceProviderName": "AXIS",
	})
	if err != nil {
		return adapter.QueryOut{}, err
	}

	tx := result.Data.Transactions[0]
	status := adapter.StatusFailure
	switch tx.Status {
	case "INITIATED", "RECEIVED", "INPROGRESS":
		status = adapter.StatusProcessing
	case "TRANSFER_SUCCESS", "CREDIT_CONFIRMATION", "TRANSFER_ACKNOWLEDGED", "PREFUND", "SUCCESS":
		status = adapter.StatusSuccess
	}
	return adapter.QueryOut{
		Status:      status,
		Amount:      int64(tx.ActualAmount * 100),
		OutOrderNo:  tx.ReferenceID,
		Response:    body,
		ReferenceNo: tx.Utr,
	}, nil
}

// balanceResult Dashboard 余额响应，单位元。
type balanceResult struct {
	baseResult
	Data struct {
		Balance float64 `json:"balance"`
	} `json:"data"`
}

// BalanceQuery 商户余额查询；Userid 头取代付 client_secret（渠道既定约定）。
func (a *Client) BalanceQuery(ctx context.Context) (adapter.BalanceOut, error) {
	for attempt := 0; ; attempt++ {
		token, err := a.token.fetch(ctx)
		if err != nil {
			return adapter.BalanceOut{}, err
		}

		code, body, err := a.http.Get(ctx, a.conf.DashboardAPIs.Balance,
			map[string]string{"Authorization": token, "Userid": a.conf.Payout.ClientSecret}, 0)
		if err != nil {
			return adapter.BalanceOut{}, fmt.Errorf("%w: %v", adapter.ErrChannelRejected, err)
		}
		if code == 401 && attempt == 0 {
			if _, err := a.token.refresh(ctx); err != nil {
				return adapter.BalanceOut{}, err
			}
			continue
		}
		if code != 200 {
			return adapter.BalanceOut{}, fmt.Errorf("%w: http %d, body %s", adapter.ErrChannelRejected, code, body)
		}

		var result balanceResult
		if err := unmarshalResult(body, &result.baseResult, &result); err != nil {
			return adapter.BalanceOut{}, err
		}
		return adapter.BalanceOut{Balance: int64(result.Data.Balance * 100)}, nil
	}
}
