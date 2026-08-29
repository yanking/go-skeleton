package job

import (
	"context"
	"log/slog"
	"time"

	"github.com/yanking/go-skeleton/pkg/app"
)

// defaultSyncInterval 实例同步周期缺省值：渠道实例元数据是低频变更的配置类数据，
// 5 分钟的滞后可接受；配置里 channel.sync_interval 留空即取此值。
const defaultSyncInterval = 5 * time.Minute

// SyncService job 所需的实例同步能力，service 实现。
type SyncService interface {
	SyncInstances(ctx context.Context) error
}

// NewSync 构造渠道实例同步任务：周期性从 channel 服务拉取实例元数据覆盖本地副本。
// interval 非正数时取 defaultSyncInterval。
//
// 单轮失败只 Warn：本地副本仍是上一轮的有效数据，下轮再试即可（装配期的首次全量
// 同步失败则直接让进程死，见 cmd/payment/initial——那是「不带病上线」，与此处不同）。
func NewSync(svc SyncService, interval time.Duration, logger *slog.Logger) app.Component {
	if interval <= 0 {
		interval = defaultSyncInterval
	}
	return &periodic{name: "instance-sync", interval: interval, run: svc.SyncInstances, logger: logger}
}
