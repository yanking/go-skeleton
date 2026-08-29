package job

import (
	"context"
	"log/slog"
	"time"
)

// periodic 是本包三个定时任务共用的循环骨架：首轮立即执行，随后按 interval 周期，
// 直到 ctx 取消。单轮出错只记日志、下一轮重试——兜底任务不该把一次失败放大为服务故障。
type periodic struct {
	name     string
	interval time.Duration
	run      func(context.Context) error
	logger   *slog.Logger
}

// Name 组件名，进日志与停机顺序。
func (p *periodic) Name() string { return p.name }

// Start 轮询直到 ctx 取消（正常停机返回 nil）。
func (p *periodic) Start(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		if err := p.run(ctx); err != nil {
			p.logger.Warn("定时任务本轮失败", "job", p.name, "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Stop 循环随 ctx 退出，无须额外动作。
func (p *periodic) Stop(context.Context) error { return nil }
