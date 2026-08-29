package job

import (
	"context"
	"log/slog"
	"time"

	"github.com/yanking/go-skeleton/pkg/app"
)

// orderSweepInterval 滞留单扫描周期：判定阈值是 30 分钟，5 分钟的扫描粒度足够及时。
const orderSweepInterval = 5 * time.Minute

// OrderSweepService job 所需的滞留单收敛能力，service 实现。
type OrderSweepService interface {
	SweepStaleCreated(ctx context.Context) error
}

// NewOrderSweep 构造滞留单兜底任务：把派单中途进程退出留下的「已创建」残单收敛为失败。
func NewOrderSweep(svc OrderSweepService, logger *slog.Logger) app.Component {
	return &periodic{name: "order-sweep", interval: orderSweepInterval, run: svc.SweepStaleCreated, logger: logger}
}
