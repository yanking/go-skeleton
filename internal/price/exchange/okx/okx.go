package okx

import (
	"fmt"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/pkg/httpc"
)

// market 本包只服务 OKX 现货（本期项目本身也「只做现货」，见
// configs/price.yaml 顶部注释），故 Decode 产出的中立类型统一填这个常量。
// Instruments/Klines 有调用方传入的 market/Sub.Market 可用，不走这个常量。
const market = "spot"

// defaultMaxStreamsPerConn 是 Config.MaxStreamsPerConn 未配置（零值）时的
// 包内安全默认。OKX 官方文档没有给出像 Binance「单连接最多 1024 条流」那样
// 明确的条数上限（已核实，见 doc.go），240 是按官方「多频道订阅总长度不超过
// 64KB」这条限制换算出的自选保守值：240 条 candle/tickers 类订阅报文打包后
// 约 10KB，远低于 64KB 上限，留有余量。
const defaultMaxStreamsPerConn = 240

// Config 构造 *OKX 所需的运行参数，逐字段对应 price 服务配置里 exchanges.okx
// 段（internal/price/config.Exchange），由装配层传入；本包不读配置文件、
// 不知道 yaml 的存在。字段名与 binance.Config 保持一致（简报约定），两个
// adapter 可共用同一套装配代码模式。
type Config struct {
	// WSURL 公共频道 WebSocket 地址，如 wss://ws.okx.com:8443/ws/v5/public；
	// 必须以 /public 结尾——candle 类频道实际要连的 /ws/v5/business 端点由
	// 本包内部替换该后缀派生（已核实，见 doc.go 与 ws.go businessWSURL 的
	// 注释），不在 Config 里为此另开字段。
	WSURL string
	// RESTURL REST 基址，如 https://www.okx.com；Instruments/Klines 在其后
	// 拼具体路径。
	RESTURL string
	// MaxStreamsPerConn 单条连接最多承载的订阅数；零值或负值退回
	// defaultMaxStreamsPerConn。
	MaxStreamsPerConn int
	// ImportQuotes Instruments 只导入计价币在此列表内的交易对；为空则
	// Instruments 返回空结果（不导入任何交易对，不是「导入全部」）。
	ImportQuotes []string
	// HTTP 出站 HTTP 客户端，Instruments/Klines 复用；必填，New 会在装配期
	// 校验并 panic——理由与 binance.Config.HTTP 完全一致（同一份
	// pkg/httpc.Client，nil 客户端会在第一次真正调用时直接空指针 panic，
	// 不会走到任何 return ..., err 分支，装配期就地报错比拖到运行期某次
	// 请求才炸更早暴露、更好定位）。
	HTTP *httpc.Client
}

// OKX 实现 exchange.Exchange：把中立类型与 OKX 现货原生报文互译。不持有连接、
// 不做限速与重试——这些是 stream 包与 collector 的职责。
type OKX struct {
	cfg Config
}

// 装配期断言：*OKX 必须完整实现 exchange.Exchange，缺方法在此就地报编译错误，
// 不必等到接线处才发现。
var _ exchange.Exchange = (*OKX)(nil)

// New 按 cfg 构造 *OKX；cfg.HTTP 为 nil（必填字段缺失）直接 panic——装配期
// 配置错误按宪法第 1 条允许 panic（同 binance.New、pkg/log.New、pkg/pgsql.New
// 的既有约定），好过把这个缺陷拖到运行期第一次真正发 REST 请求时才暴露成
// 一次空指针 panic（见 Config.HTTP 的字段注释）。WSURL/RESTURL 等字段留空
// 只会在实际调用 Plan/Klines/Instruments 时产出格式错误的 URL 或返回 error，
// 不会 panic，因此不在此处校验。
func New(cfg Config) *OKX {
	if cfg.HTTP == nil {
		panic(fmt.Errorf("构造 OKX: HTTP 不能为空"))
	}
	return &OKX{cfg: cfg}
}

// Name 返回交易所名称。
func (o *OKX) Name() string { return "okx" }

// maxStreamsPerConn 返回本次生效的单连接订阅数上限：cfg 未配置时退回包内默认。
func (o *OKX) maxStreamsPerConn() int {
	if o.cfg.MaxStreamsPerConn <= 0 {
		return defaultMaxStreamsPerConn
	}
	return o.cfg.MaxStreamsPerConn
}
