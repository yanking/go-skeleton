package payapay

import (
	"fmt"
	"math/rand"
)

// indiaPrefixes 印度主要 IP 段前缀及权重，按权重随机选择。
var (
	indiaPrefixes = []int{103, 115, 117, 122, 125}
	indiaWeights  = []int{25, 20, 25, 20, 10}
)

// generateIndiaIP 生成随机印度 IPv4 地址。PayaPay 下单参数要求 user_ip 为
// 印度地址，此为该渠道对接的既有约定，行为自原实现等价移植。
// math/rand 全局源自 Go 1.20 起自动播种且并发安全。
func generateIndiaIP() string {
	total := 0
	for _, w := range indiaWeights {
		total += w
	}
	r := rand.Intn(total)
	selected := indiaPrefixes[len(indiaPrefixes)-1]
	cumulative := 0
	for i, weight := range indiaWeights {
		cumulative += weight
		if r < cumulative {
			selected = indiaPrefixes[i]
			break
		}
	}

	// 末段避开 0 结尾，模拟真实分配。
	return fmt.Sprintf("%d.%d.%d.%d", selected, rand.Intn(256), rand.Intn(256), rand.Intn(255)+1)
}
