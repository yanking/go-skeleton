package binance

import "testing"

// TestNew_PanicsWithoutHTTP 验证 Config.HTTP 缺失时 New 在装配期直接 panic，
// 不是留到运行期第一次真正发 REST 请求时才炸成一次空指针 panic（见
// Config.HTTP 与 New 的注释、任务 6 修复轮 2 的评审记录）。
func TestNew_PanicsWithoutHTTP(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Config.HTTP 为 nil 时 New 应 panic，实际正常返回")
		}
	}()
	New(Config{WSURL: "wss://x/stream", RESTURL: "https://x"})
}
