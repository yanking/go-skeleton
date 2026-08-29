// Package ratelimit 提供交易所 REST 的共享限速令牌桶。
//
// 「共享」的准确含义是进程内单例：常驻 daemon 与 instruments/backfill 两个
// 子命令各自的装配代码都按同一份 config.Exchange 构造出一个 *Bucket，且都
// 只构造这一次，同时传给该进程内全部会打 REST 的调用方（见
// cmd/price/initial/init_app.go 的 createServer、oneshot.go 的
// oneshotExchange）——同一个 *Bucket 实例只在同一个进程内被共享，不可能跨
// 进程共享（Go 的 *rate.Limiter 没有进程间同步能力）。
//
// 这意味着：daemon 常驻运行期间，人工另开一个进程跑 backfill 子命令，会用
// 同一份配置各自独立地构造一个令牌桶——两个桶叠加发出的 REST 请求速率会
// 超过配置里写的那个数字，因为交易所的配额是按 IP/账号算的，不认进程边界。
// 需要在 daemon 运行期间对同一交易所人工补洞时，要么接受这段时间内 REST
// 压力翻倍（多数交易所的限速有余量，短时借用通常无碍），要么先把该交易所在
// configs/price.yaml 里改成 enabled: false 重启 daemon（daemon 侧不再消费
// 这个交易所的限速桶），跑完 backfill 再改回来。
package ratelimit

import (
	"context"

	xrate "golang.org/x/time/rate"
)

// Bucket 是对 golang.org/x/time/rate.Limiter 的薄封装。
type Bucket struct {
	l *xrate.Limiter
}

// New 创建一个新的限速桶，perSecond 为每秒令牌数，burst 为容量。
func New(perSecond float64, burst int) *Bucket {
	return &Bucket{
		l: xrate.NewLimiter(xrate.Limit(perSecond), burst),
	}
}

// Wait 阻塞直到获得一个令牌或上下文取消。
func (b *Bucket) Wait(ctx context.Context) error {
	return b.l.Wait(ctx)
}
