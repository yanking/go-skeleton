package okx

import "testing"

// TestNew_PanicsWithoutHTTP 验证 Config.HTTP 缺失时 New 在装配期直接 panic，
// 不是留到运行期第一次真正发 REST 请求时才炸成一次空指针 panic（约定与
// binance 包同名测试一致，见 binance/binance_test.go 与 Config.HTTP 的
// 字段注释）。
func TestNew_PanicsWithoutHTTP(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Config.HTTP 为 nil 时 New 应 panic，实际正常返回")
		}
	}()
	New(Config{WSURL: "wss://x/ws/v5/public", RESTURL: "https://x"})
}
