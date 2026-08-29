// Package exchange 定义交易所协议翻译层的公共类型：订阅描述(Sub)、连接计划(ConnPlan)、事件(Event)、中立类型(Kline/Snapshot/Instrument)等。
// service、stream、binance、okx 等包均依赖本包做协议无关的中间表示，具体交易所的
// 报文拼装、签名、响应解析见各自子包。
package exchange

import (
	"context"
	"time"
)

// 流类型：即 price_subscriptions 表 stream_type 列的取值，标识一条订阅要建立的推送流种类。
const (
	StreamKline  = "kline"  // K 线流
	StreamTicker = "ticker" // 行情快照流
	StreamDepth  = "depth"  // 深度快照流
)

// Sub 一条订阅，来自 subscriptions 表，已是交易所原生形态。
type Sub struct {
	Market       string // 子市场（如 spot）；不是交易所名——交易所维度由 service.RouteFor 绑定
	NativeSymbol string // 交易所原生交易对符号
	StreamType   string // 流类型（kline/ticker/depth）
	Interval     string // 时间间隔（仅 kline 需要）
}

// ConnPlan 一条 ws 连接的声明式描述：交易所包只产出它，不负责怎么连。
type ConnPlan struct {
	URL        string        // 完整拨号地址（合并流形态可能已把订阅编进 query）
	Subscribe  [][]byte      // 连上后按序发送的订阅帧；为空表示订阅已在 URL 里
	Subs       []Sub         // 本连接覆盖的订阅，供日志与补洞使用
	ClientPing []byte        // 需要客户端主动心跳时的帧内容；nil 表示由服务端 ping
	PingEvery  time.Duration // ClientPing 非 nil 时的发送间隔
}

// Kline 中立 K 线，只在已收线时产出。
type Kline struct {
	Market       string // 子市场（如 spot）；不是交易所名——交易所维度由 service.RouteFor 绑定
	NativeSymbol string // 交易所原生交易对符号
	Interval     string // 时间间隔
	OpenTime     int64  // UTC 毫秒
	Open         string // 开盘价，十进制字符串（保留精度，勿转 float）
	High         string // 最高价，十进制字符串
	Low          string // 最低价，十进制字符串
	Close        string // 收盘价，十进制字符串
	Volume       string // 成交量（基础币），十进制字符串
	QuoteVolume  string // 成交额（计价币），十进制字符串
}

// Snapshot 中立快照（ticker 或 depth），只留最新值。EventTime 只填交易所报文
// 自带的时间戳；报文不带时间戳（如 Binance 的浅层盘口）则填 0，不得拿本地
// 接收时间冒充——recv_ts 与写进 Redis 的最终值由 service 层组装，不进本包。
//
// Payload 是归一化后的报文主体，形状是 binance/okx 等 adapter 共享的契约（新增
// 交易所接入时须遵守，不要另起一套字段名）：价格与数量一律十进制字符串，不转
// float（避免精度丢失）；
//   - StreamType == StreamTicker 时：{"last":"..","bid":"..","ask":"..",
//     "high24h":"..","low24h":"..","volume24h":".."}，volume24h 取基础币口径
//     （即 24 小时成交量按成交对里的基础币计量，不是按计价币/成交额计量的
//     口径——Binance 的 v 字段、OKX 的 vol24h 都是这个口径，两家一致）。
//   - StreamType == StreamDepth 时：{"bids":[["价","量"],...],
//     "asks":[["价","量"],...]}，每档只保留价与量两项，交易所原始档位里的
//     其余字段（如订单数、废弃字段）不透传。
type Snapshot struct {
	Market       string // 子市场（如 spot）；不是交易所名——交易所维度由 service.RouteFor 绑定
	NativeSymbol string // 交易所原生交易对符号
	StreamType   string // 流类型（ticker/depth）
	EventTime    int64  // 交易所事件时间，UTC 毫秒
	Payload      []byte // 归一化后的 JSON，直接写 Redis，形状见上方类型注释
}

// Event 一帧解码结果：以下指针中至多一个非 nil；全 nil 表示该帧无需处理
// （心跳应答、订阅确认、未收线的 K 线）。
type Event struct {
	Kline    *Kline
	Snapshot *Snapshot
}

// Exchange 一家交易所的协议翻译。实现者不得触碰存储、重连与限速。
type Exchange interface {
	// Name 返回交易所名称（binance/okx 等）。
	Name() string

	// Plan 把订阅切分成若干条连接的计划；切分上限由实现按自身文档决定。
	Plan(subs []Sub) ([]ConnPlan, error)

	// Decode 解一帧原始消息。无法识别的帧返回零值 Event 与 nil error——
	// 交易所会推送订阅确认、心跳应答等与业务无关的帧，它们不是错误。
	Decode(raw []byte) (Event, error)

	// Instruments 拉全量交易对（REST）。
	Instruments(ctx context.Context, market string) ([]Instrument, error)

	// Klines 拉一段历史 K 线，返回一律按开盘时间正序。
	// 返回的第二个值为下一页起点；为 0 表示已到 end。
	Klines(ctx context.Context, s Sub, start, end int64) ([]Kline, int64, error)
}

// Instrument 中立交易对。
type Instrument struct {
	Market       string // 子市场（如 spot）；不是交易所名——交易所维度由 service.RouteFor 绑定
	NativeSymbol string // 交易所原生交易对符号
	Symbol       string // 归一化符号
	Base         string // 基础币
	Quote        string // 计价币
	Trading      bool   // 是否可交易
}
