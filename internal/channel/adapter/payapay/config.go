package payapay

// Platform PayaPay 渠道私有配置，与 channels.platform 的 JSON 结构对应。
type Platform struct {
	BaseURL string `json:"base_url"`
	APIs    struct {
		Payment      string `json:"payment"`
		PaymentQuery string `json:"payment_query"`
		Payout       string `json:"payout"`
		PayoutQuery  string `json:"payout_query"`
		BalanceQuery string `json:"balance_query"`
	} `json:"apis"`
	MerID      int64  `json:"mer_id"`
	AppID      int64  `json:"app_id"`
	AppSecret  string `json:"app_secret"`
	PayInCode  int64  `json:"pay_in_code"`
	PayOutCode int64  `json:"pay_out_code"`
}
