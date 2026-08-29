package job

import (
	"context"
	"log/slog"
	"time"

	"github.com/yanking/go-skeleton/pkg/app"
)

// notifySweepInterval 卡住通知的重投扫描周期：最短判定阈值是 10 分钟，5 分钟粒度足够。
const notifySweepInterval = 5 * time.Minute

// NotifySweepService job 所需的通知重投能力，service 实现。
type NotifySweepService interface {
	SweepStuckNotify(ctx context.Context) error
}

// NewNotifySweep 构造通知重投兜底任务：把入队丢失或长期未推进的商户通知重新入队。
func NewNotifySweep(svc NotifySweepService, logger *slog.Logger) app.Component {
	return &periodic{name: "notify-sweep", interval: notifySweepInterval, run: svc.SweepStuckNotify, logger: logger}
}
