package binance

import (
	"fmt"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/pkg/httpc"
)

// market 本包只服务 Binance 现货（本期项目本身也「只做现货」，见
// configs/price.yaml 顶部注释），故 Decode 产出的中立类型统一填这个常量。
// Instruments/Klines 有调用方传入的 market/Sub.Market 可用，不走这个常量。
const market = "spot"

// defaultMaxStreamsPerConn 是 Config.MaxStreamsPerConn 未配置（零值）时的
// 包内安全默认，取值见 doc.go 的核实结论（官方文档：单连接最多 1024 条流）。
const defaultMaxStreamsPerConn = 1024

// Config 构造 *Binance 所需的运行参数，逐字段对应 price 服务配置里
// exchanges.binance 段（internal/price/config.Exchange），由装配层传入；
// 本包不读配置文件、不知道 yaml 的存在。
type Config struct {
	// WSURL 合并流拨号地址前缀，如 wss://stream.binance.com:9443/stream；
	// Plan 会在其后拼上 "?streams=" 与流名列表。
	WSURL string
	// RESTURL REST 基址，如 https://api.binance.com；Instruments/Klines
	// 在其后拼具体路径。
	RESTURL string
	// MaxStreamsPerConn 单条连接最多承载的流数；零值或负值退回
	// defaultMaxStreamsPerConn。
	MaxStreamsPerConn int
	// ImportQuotes Instruments 只导入计价币在此列表内的交易对；为空则
	// Instruments 返回空结果（不导入任何交易对，不是「导入全部」）。
	ImportQuotes []string
	// HTTP 出站 HTTP 客户端，Instruments/Klines 复用；必填，New 会在装配期
	// 校验并 panic——不是留到真正发请求时报错：pkg/httpc.Client.Get 内部
	// 无条件访问 c.timeout 等字段，nil 客户端会在第一次调用时直接空指针
	// panic，不会走到任何 return ..., err 分支（业务路径 panic 违反宪法
	// 第 1 条），装配期就地报错比拖到运行期某次请求才炸更早暴露、更好定位。
	HTTP *httpc.Client
}

// Binance 实现 exchange.Exchange：把中立类型与 Binance 现货原生报文互译。
// 不持有连接、不做限速与重试——这些是 stream 包与 collector 的职责。
type Binance struct {
	cfg Config
}

// 装配期断言：*Binance 必须完整实现 exchange.Exchange，缺方法在此就地报编译错误，
// 不必等到接线处才发现。
var _ exchange.Exchange = (*Binance)(nil)

// New 按 cfg 构造 *Binance；cfg.HTTP 为 nil（必填字段缺失）直接 panic——
// 装配期配置错误按宪法第 1 条允许 panic（同 pkg/log.New、pkg/pgsql.New 的
// 既有约定），好过把这个缺陷拖到运行期第一次真正发 REST 请求时才暴露成
// 一次空指针 panic（见 Config.HTTP 的字段注释）。WSURL/RESTURL 等字段留空
// 只会在实际调用 Plan/Klines/Instruments 时产出格式错误的 URL、走正常的
// error 返回路径，不会 panic，因此不在此处校验。
func New(cfg Config) *Binance {
	if cfg.HTTP == nil {
		panic(fmt.Errorf("构造 Binance: HTTP 不能为空"))
	}
	return &Binance{cfg: cfg}
}

// Name 返回交易所名称。
func (b *Binance) Name() string { return "binance" }

// maxStreamsPerConn 返回本次生效的单连接流数上限：cfg 未配置时退回包内默认。
func (b *Binance) maxStreamsPerConn() int {
	if b.cfg.MaxStreamsPerConn <= 0 {
		return defaultMaxStreamsPerConn
	}
	return b.cfg.MaxStreamsPerConn
}
