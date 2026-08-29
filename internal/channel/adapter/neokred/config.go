package neokred

// Platform Neokred 渠道私有配置，与 channels.platform 的 JSON 结构对应。
type Platform struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	DashboardAPIs struct {
		Login   string `json:"login"`
		Query   string `json:"query"`
		Balance string `json:"balance"`
	} `json:"dashboard_apis"`
	Payment struct {
		ClientSecret string `json:"client_secret"`
		ProgramID    string `json:"program_id"`
		APIs         struct {
			Order string `json:"order"`
			Query string `json:"query"`
		} `json:"apis"`
	} `json:"payment"`
	Payout struct {
		ClientSecret string `json:"client_secret"`
		ProgramID    string `json:"program_id"`
		APIs         struct {
			Order string `json:"order"`
			Query string `json:"query"`
		} `json:"apis"`
	} `json:"payout"`
}
