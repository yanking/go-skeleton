// Package config 定义 price 服务的配置结构，由 configs/price.yaml 绑定。
package config

import (
	"time"

	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/pgsql"
	"github.com/yanking/go-skeleton/pkg/redis"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

// Config price 服务的全部配置。
type Config struct {
	Log       log.Config          `yaml:"log"`
	App       app.Config          `yaml:"app"`
	Telemetry telemetry.Config    `yaml:"telemetry"`
	Pgsql     pgsql.Config        `yaml:"pgsql"`
	Redis     redis.Config        `yaml:"redis"`
	Collector Collector           `yaml:"collector"`
	Exchanges map[string]Exchange `yaml:"exchanges"`
}

// Collector 采集器的通用参数,与具体交易所无关。
type Collector struct {
	// ReloadInterval 订阅集重载周期,零值取 5m。
	ReloadInterval time.Duration `yaml:"reload_interval"`
	// MaxBackfillWindow 新标的首次补洞的最大回溯窗口,零值取 720h(30 天)。
	MaxBackfillWindow time.Duration `yaml:"max_backfill_window"`
	// BackfillConcurrency 同时补洞的订阅数,零值取 2。
	BackfillConcurrency int `yaml:"backfill_concurrency"`
	// KlineQueueSize kline 队列容量,满即阻塞上游(不可丢)。零值取 1024。
	KlineQueueSize int `yaml:"kline_queue_size"`
	// SnapshotQueueSize ticker/depth 队列容量,满即弃旧(可丢)。零值取 256。
	SnapshotQueueSize int `yaml:"snapshot_queue_size"`
}

// Exchange 单个交易所的连接与限速参数。
type Exchange struct {
	// Enabled 是否装配这家交易所：false（含漏写本字段的零值）时装配层跳过
	// 它——不建 ws 连接、不建限速桶，运维不必删掉整个配置块就能临时关停
	// 单个交易所。零值 false 就等于关停，不是「未设置就报错」，改这个字段
	// 前确认这一点，避免以为漏写等同于「保持之前的开启状态」。
	Enabled           bool          `yaml:"enabled"`
	WSURL             string        `yaml:"ws_url"`
	RESTURL           string        `yaml:"rest_url"`
	MaxStreamsPerConn int           `yaml:"max_streams_per_conn"`
	RESTPerSecond     float64       `yaml:"rest_per_second"`
	RESTBurst         int           `yaml:"rest_burst"`
	DialTimeout       time.Duration `yaml:"dial_timeout"`
	// ReconnectBackoffMin ws 断线重连的退避基准间隔，零值时由 stream 包套用
	// 包内安全默认——留空不是「不限速重连」，是防止把交易所打爆的兜底。
	// 同时是 stream 包判定「一次连接是否算真正活过」的门槛（存活时长短于它
	// 就不清零退避计数，见 stream.Conn.runOnce）：调成很小的值（如
	// 10ms）虽然能让断线恢复更快，但这个存活门槛会跟着塌到 10ms，等于把
	// 「握手通过又立刻断开不该被当作成功连接」这条防线也削没了——调这个值
	// 之前想清楚这层含义。
	ReconnectBackoffMin time.Duration `yaml:"reconnect_backoff_min"`
	// ReconnectBackoffMax ws 断线重连的退避上限，零值时由 stream 包套用
	// 包内安全默认。
	ReconnectBackoffMax time.Duration `yaml:"reconnect_backoff_max"`
	ImportQuotes        []string      `yaml:"import_quotes"`
}
