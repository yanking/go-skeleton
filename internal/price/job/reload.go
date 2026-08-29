package job

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/pkg/app"
)

// defaultReloadInterval 订阅集重载周期缺省值，与 configs/price.yaml
// collector.reload_interval 字段注释「零值取 5m」一致。
const defaultReloadInterval = 5 * time.Minute

// ReloadService 是 reload job 所需的连接计划生成能力，service.Price 实现。
type ReloadService interface {
	Plans(ctx context.Context) (map[string][]exchange.ConnPlan, error)
}

// Rebuilder 是 job 所需的连接重建能力，stream.Manager 实现。
type Rebuilder interface {
	Rebuild(ctx context.Context, plans []exchange.ConnPlan) error
}

// reload 订阅集重载任务：按 interval 周期读取全部启用订阅生成的连接计划，
// 逐个交易所调用对应 Rebuilder.Rebuild 使其生效。
type reload struct {
	svc      ReloadService
	mgrs     map[string]Rebuilder
	interval time.Duration
	logger   *slog.Logger

	// started、done 供 Stop 判断该怎么等（细则见 Stop 的方法注释）：started
	// 在 Start 真正开始运行时置位；done 在 Start 的循环彻底返回后关闭。两者
	// 都是本组件自己的状态，不依赖调用方的 ctx——Stop 用 context.Background()
	// 调用时（如 TestReload_Stop_ToleratesNotYetStarted）也不能永久阻塞。
	started atomic.Bool
	done    chan struct{}
}

// NewReload 构造订阅集重载任务；mgrs 以交易所名为键，须与 svc.Plans 返回值
// 同键——一个 job 内遍历逐个交易所重建，不必每个交易所各起一个 job（Task 11
// 裁决 R27）。interval 非正数时取 defaultReloadInterval。
func NewReload(svc ReloadService, mgrs map[string]Rebuilder, interval time.Duration, logger *slog.Logger) app.Component {
	if interval <= 0 {
		interval = defaultReloadInterval
	}
	return &reload{svc: svc, mgrs: mgrs, interval: interval, logger: logger, done: make(chan struct{})}
}

// Name 实现 app.Component。
func (j *reload) Name() string { return "price-reload" }

// Start 实现 app.Component：首轮立即执行一次，随后按 interval 周期重复，
// 直到 ctx 取消为止——这是唯一的正常退出路径，返回 nil。单轮内的失败（读取
// 计划失败、单个交易所重建失败）都只 Warn、不中断循环——兜底任务不该把一次
// 失败放大为服务故障，下一周期用同样的入参重试即可。
func (j *reload) Start(ctx context.Context) error {
	j.started.Store(true)
	defer close(j.done)

	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()

	for {
		j.reloadOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Stop 实现 app.Component：等待 Start 的循环真正返回，最多等到 ctx（pkg/app
// 全部组件共享的停机预算）到期；Start 从未被调用过（started 仍是零值 false）
// 时无事可等，直接返回 nil——满足 pkg/app.Component 「Stop 可能在组件尚未
// 真正运行时被调用，实现须容忍」的约定（TestReload_Stop_ToleratesNotYetStarted
// 用 context.Background() 调用本方法，若这里无条件等 done 会永久阻塞）。
//
// 曾经这里是无条件立即返回 nil（评审 Important 4）：本组件按约定注册在全部
// stream.Manager 之后，pkg/app 逆序停止时本组件的 Stop 先被调用——如果不等
// 循环真正退出，存在一个窄窗口：Start 的循环这一刻可能恰好停在「reloadOnce
// 内 Plans 已成功返回、尚未跑完对某个 Rebuilder.Rebuild 的调用」这个中间态
// （根 ctx 已取消但 goroutine 还没被调度到），Stop 一旦提前返回，pkg/app
// 会紧接着调用下一个组件（某个 stream.Manager）的 Stop；若这次迟到的
// Rebuild 调用恰好在对应 Manager.Stop 已经跑完（清空了连接、释放了锁）之后
// 才真正执行到，会凭空建出一批不受任何 Stop 管辖的新连接——它们在停机期间
// 照常拨号、触发 OnReady 补洞，撞上正在关闭的 DB/Redis 连接池。等 Start
// 真正返回，从时序上保证这种「迟到的 Rebuild」不可能发生：本方法一旦返回，
// Start 的循环已经彻底退出，不会再调用任何 Rebuild。
func (j *reload) Stop(ctx context.Context) error {
	if !j.started.Load() {
		return nil
	}
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("停止订阅集重载任务: 宽限期耗尽，循环未在预期时间内返回: %w", ctx.Err())
	}
}

// reloadOnce 跑一轮重载：读取当前应有的连接计划，逐个交易所调用 Rebuild 使
// 其生效。读取失败或单个交易所 Rebuild 失败都只 Warn 并继续其余交易所——
// 一家交易所出问题不该让其余几家一起停摆，与 service.Backfill/Plans 既有的
// 容错思路一致。
func (j *reload) reloadOnce(ctx context.Context) {
	plans, err := j.svc.Plans(ctx)
	if err != nil {
		j.logger.Warn("读取连接计划失败，本轮重载跳过", "err", err)
		return
	}
	for ex, p := range plans {
		mgr, ok := j.mgrs[ex]
		if !ok {
			j.logger.Warn("连接计划引用的交易所未装配 Manager，跳过", "exchange", ex)
			continue
		}
		if err := mgr.Rebuild(ctx, p); err != nil {
			j.logger.Warn("重建连接失败，跳过该交易所", "exchange", ex, "err", err)
		}
	}
}
