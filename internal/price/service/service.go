package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
	"github.com/yanking/go-skeleton/internal/price/ratelimit"
)

// defaultKlineQueueSize、defaultSnapshotQueueSize 是 Config.KlineQueueSize/
// SnapshotQueueSize 零值时的兜底默认，与 configs/price.yaml
// collector.kline_queue_size/snapshot_queue_size 字段注释「零值取
// 1024/256」一致——在这两个常量写出来之前，New 里零值会直接传给 make(chan
// T, 0)，退化成无缓冲 channel（每次发送都要等到有人接收），与 yaml 文档
// 承诺的默认值对不上：MaxBackfillWindow/BackfillConcurrency
// /ReloadInterval 三个同组字段都各自在使用点实现了零值兜底，只有这两个
// 是纸面承诺，装配层（Task 12）第一次真正把这段 yaml 接进来时被评审抓到
// （Important 修复），这里一并补齐。
const (
	defaultKlineQueueSize    = 1024
	defaultSnapshotQueueSize = 256
)

// Config 是 price 业务层的运行参数，与 config.Collector 的四个字段一一对应，
// 由装配层（cmd/price/initial）从 yaml 绑好的配置转换而来——本包不读 yaml。
type Config struct {
	// KlineQueueSize kline 队列容量：队列满时 Route 阻塞在发送上，直到
	// RunWriters 腾出位置——收线帧不可丢。零值取 defaultKlineQueueSize。
	KlineQueueSize int
	// SnapshotQueueSize ticker/depth 队列容量：队列满时丢弃队列里最旧的一帧，
	// 换新帧进去——它们是快照，只有最新一帧有意义。零值取
	// defaultSnapshotQueueSize。
	SnapshotQueueSize int
	// MaxBackfillWindow 新标的首次补洞时，库里没有任何已存 K 线可续接的情况下，
	// 起点回溯的最大窗口。
	MaxBackfillWindow time.Duration
	// BackfillConcurrency 补洞任务同时处理的订阅数。
	BackfillConcurrency int
}

// InstrumentRepo 标的仓储接口，方法集与 repo.Instrument 的导出方法逐字对齐
// （依赖倒置支点：本层只声明契约，repo 包实现）。
type InstrumentRepo interface {
	UpsertAll(ctx context.Context, rows []model.Instrument) error
	MarkDelistedExcept(ctx context.Context, exchange, market string, keep []string) error
}

// SubscriptionRepo 订阅声明仓储接口，方法集与 repo.Subscription 的导出方法
// 逐字对齐。
type SubscriptionRepo interface {
	ListEnabled(ctx context.Context) ([]model.Subscription, error)
}

// KlineRepo K 线仓储接口，方法集与 repo.Kline 的导出方法逐字对齐。
type KlineRepo interface {
	Upsert(ctx context.Context, rows []model.Kline) error
	MaxOpenTime(ctx context.Context, exchange, market, nativeSymbol, interval string) (int64, bool, error)
}

// LatestRepo 最新行情仓储接口，方法集与 repo.Latest 的导出方法逐字对齐。
type LatestRepo interface {
	Set(ctx context.Context, key string, payload []byte) error
}

// Deps 是 Price 的外部依赖集合，均以接口声明；具体实现（repo 包、各交易所
// adapter、限速桶）由装配层注入。
type Deps struct {
	Instruments InstrumentRepo
	Subs        SubscriptionRepo
	Klines      KlineRepo
	Latest      LatestRepo
	// Exchanges 以交易所名（configs/price.yaml 的 exchanges 段键名，如
	// binance/okx）为键，Backfill/ImportInstruments/Plans 据此按名字查具体
	// 交易所实现。
	Exchanges map[string]exchange.Exchange
	// Limits 与 Exchanges 同键：每家交易所一个限速桶，本进程内全部会打
	// REST 的调用方（Backfill/BackfillRange）必须共用同一个实例；「共用」
	// 止于进程边界——常驻 daemon 与 backfill 子命令是两个进程，各自的装配
	// 代码按同一份配置各构造一次，不是同一个 Go 对象，细则见 ratelimit
	// 包注释。
	Limits map[string]*ratelimit.Bucket
}

// Price 是 price 服务的业务编排层：订阅集读取与连接计划生成（plan.go）、事件
// 路由与按语义分流的背压、批量落库/写 Redis（route.go）、K 线补洞
// （backfill.go，Task 10）、交易对导入（instruments.go，Task 11）。
type Price struct {
	cfg    Config
	deps   Deps
	logger *slog.Logger

	// klineCh、snapCh 是按数据语义分开的两个队列，不按连接分——否则 depth
	// 的洪流会把同连接上的 kline 拖住，见 Route 的类型注释。
	klineCh chan model.Kline
	snapCh  chan snapshotItem
}

// New 构造 Price；两个队列的容量取自 cfg，零值分别兜底到
// defaultKlineQueueSize/defaultSnapshotQueueSize，不会退化成无缓冲
// channel——无缓冲的 kline 队列会让 Route 的每次发送都必须等到 RunWriters
// 恰好在同一时刻接收，正常运行时几乎总在阻塞，这不是「零值等于最小合法
// 配置」，而是把队列这个设计意图直接抹掉。
func New(cfg Config, deps Deps, logger *slog.Logger) *Price {
	klineQueueSize := cfg.KlineQueueSize
	if klineQueueSize <= 0 {
		klineQueueSize = defaultKlineQueueSize
	}
	snapshotQueueSize := cfg.SnapshotQueueSize
	if snapshotQueueSize <= 0 {
		snapshotQueueSize = defaultSnapshotQueueSize
	}
	return &Price{
		cfg:     cfg,
		deps:    deps,
		logger:  logger,
		klineCh: make(chan model.Kline, klineQueueSize),
		snapCh:  make(chan snapshotItem, snapshotQueueSize),
	}
}
