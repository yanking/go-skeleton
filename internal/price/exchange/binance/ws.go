package binance

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// validIntervals 是 K 线流支持的全部周期拼写白名单（官方 16 个），streamName
// 用它校验 kline 订阅的 Sub.Interval。不校验会有一条隐蔽的静默失败链，且比
// OKX 侧同类问题（见 okx/ws.go validBars 的注释）爆炸半径更大：Binance 把
// 一家交易所的全部订阅打进一条合并流 URL（见本文件 Plan），一条 interval
// 拼写错误（如 "1H" 而不是 "1h"）会让整条连接的拨号被拒或握手后持续报错，
// 不是只有这一条订阅失效；而 price_subscriptions 是人工维护的表（全仓没有
// 代码写入订阅行），一次手误的爆炸半径就是这家交易所全停。不能用 rest.go 的
// splitInterval 代替这道校验——它只校验「数字+任意单位」的形状，"1H" 这类
// 大小写错误能原样通过，挡不住最典型的那类错误。
var validIntervals = map[string]bool{
	"1s": true, "1m": true, "3m": true, "5m": true, "15m": true, "30m": true,
	"1h": true, "2h": true, "4h": true, "6h": true, "8h": true, "12h": true,
	"1d": true, "3d": true, "1w": true, "1M": true,
}

// streamName 把一条订阅翻成 Binance 原生的流名（已核实的拼写见 doc.go）：
//   - kline: <symbol 小写>@kline_<interval>，interval 先过 validIntervals
//     白名单校验，校验不通过直接返回 error，不放行到字符串拼接（校验放在
//     Plan 阶段而不是留到运行时，错误在装配/重载订阅集时就暴露成一条可见
//     的 error）；
//   - ticker: <symbol 小写>@ticker
//   - depth: <symbol 小写>@depth20@100ms（档位与推送速度固定，Sub 未携带
//     可配置项，本包不引入这份配置——按「只写要求的东西」处理）
func streamName(s exchange.Sub) (string, error) {
	symbol := strings.ToLower(s.NativeSymbol)
	switch s.StreamType {
	case exchange.StreamKline:
		if !validIntervals[s.Interval] {
			return "", fmt.Errorf("binance: 不支持的 K 线周期 %q", s.Interval)
		}
		return symbol + "@kline_" + s.Interval, nil
	case exchange.StreamTicker:
		return symbol + "@ticker", nil
	case exchange.StreamDepth:
		return symbol + "@depth20@100ms", nil
	default:
		return "", fmt.Errorf("binance: 不支持的流类型 %q", s.StreamType)
	}
}

// Plan 把订阅按 MaxStreamsPerConn 切分成若干条连接计划，流名拼进合并流 URL
// 的 streams 参数——这种形态订阅已编在 URL 里，故 ConnPlan.Subscribe 留空；
// ClientPing 留 nil，由服务端主动 ping（见 doc.go），coder/websocket 自动
// 应答 pong，本包无需生成心跳帧。
func (b *Binance) Plan(subs []exchange.Sub) ([]exchange.ConnPlan, error) {
	limit := b.maxStreamsPerConn()

	var plans []exchange.ConnPlan
	for start := 0; start < len(subs); start += limit {
		end := start + limit
		if end > len(subs) {
			end = len(subs)
		}
		chunk := subs[start:end]

		names := make([]string, 0, len(chunk))
		for _, s := range chunk {
			name, err := streamName(s)
			if err != nil {
				return nil, err
			}
			names = append(names, name)
		}

		plans = append(plans, exchange.ConnPlan{
			URL:  b.cfg.WSURL + "?streams=" + strings.Join(names, "/"),
			Subs: chunk,
		})
	}
	return plans, nil
}

// frame 合并流的外层包裹：{"stream":"<name>","data":<原始报文>}。订阅确认
// （{"result":null,"id":1}）、错误响应（{"code":...,"msg":...}）等控制帧不带
// 这层包裹，解出来 Data 为空。Stream 只在 Decode 需要从流名反推字段时使用
// （目前只有浅层盘口——它的 data 不带 symbol，见 decodeDepth）。
type frame struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// eventHead 只取 data.e 用于分派，具体流类型各自的完整结构见对应 decode 函数。
//
// EventTime 字段本身不使用，但必须显式声明：Binance 报文里 e（事件类型，字符串）
// 与 E（事件时间，数字）是含义完全不同的两个同名大小写字段，encoding/json 对
// 结构体里没有精确 tag 命中的键会退化成大小写不敏感匹配——若这里不声明 "E"，
// 解到 E 键时会退化匹配到 E 字段（因为只有它 tag 名折叠后是 "e"），把数字塞进
// 字符串字段直接报错；有的字段（下面 klineFrame/tickerFrame 里同名大小写对）
// 类型恰好相同，退化匹配不会报错，而是让后处理的键静默覆盖先处理的键，更隐蔽。
type eventHead struct {
	E         string `json:"e"`
	EventTime int64  `json:"E"` // 未使用，仅占住精确匹配防止退化覆盖 E，见上方注释
}

// Decode 解一帧原始 WebSocket 消息。先看外层是否带 data 包裹：没有则是订阅
// 确认、错误响应一类控制帧，直接判定为可忽略；带 data 再取 data.e 分派——
// kline/ticker 报文都带 e 字段，按值路由；浅层盘口（Partial Book Depth）
// 报文本身不带 e 字段（e 取零值），按流名是否含 "@depth" 识别，这也是
// Plan 里 streamName 对 StreamDepth 生成的流名格式，两处对应，不会出现
// 订了 depth 却在这里无法识别的情况。其余未识别取值（本包未订阅、也未实现
// 翻译的流类型，如 aggTrade）一律按「未识别，可忽略」处理，不当作错误：
// 这类帧交易所会持续推送，误判成错误会在 stream.Conn.readLoop 里被当解码
// 失败刷成日志噪音。
func (b *Binance) Decode(raw []byte) (exchange.Event, error) {
	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return exchange.Event{}, fmt.Errorf("binance: 解析帧: %w", err)
	}
	if len(f.Data) == 0 {
		return exchange.Event{}, nil
	}

	var head eventHead
	if err := json.Unmarshal(f.Data, &head); err != nil {
		return exchange.Event{}, fmt.Errorf("binance: 解析帧 data: %w", err)
	}

	switch head.E {
	case "kline":
		return decodeKline(f.Data)
	case "24hrTicker":
		return decodeTicker(f.Data)
	case "":
		if strings.Contains(f.Stream, "@depth") {
			return decodeDepth(f.Stream, f.Data)
		}
		return exchange.Event{}, nil
	default:
		return exchange.Event{}, nil
	}
}

// klineFrame 对应 Kline/Candlestick Streams 的 data 字段，字段含义见 doc.go
// 引用的官方样例；k.t 是开盘时间、k.T 是收盘时间——中立类型只取前者。
//
// CloseTime/LastTradeID/TakerBuyBaseVolume/TakerBuyQuoteVolume 四个字段本身
// 不使用，但必须显式声明：k 对象里 t/T、v/V、q/Q、l/L 是含义完全不同、类型
// 恰好相同的同名大小写字段对（如 l 是最低价字符串、L 是最后成交 ID 数字），
// 不声明会被 encoding/json 的大小写不敏感兜底匹配退化合并——多数情况类型不同
// 会直接报错，但同类型的（如 t/T 都是数字）会静默用后处理的键覆盖先处理的键，
// 把开盘时间悄悄换成收盘时间，比报错更危险；理由细节同 eventHead 的注释。
type klineFrame struct {
	K struct {
		OpenTime            int64  `json:"t"`
		CloseTime           int64  `json:"T"` // 未使用，占位见上方类型注释
		Symbol              string `json:"s"`
		Interval            string `json:"i"`
		Open                string `json:"o"`
		Close               string `json:"c"`
		High                string `json:"h"`
		Low                 string `json:"l"`
		LastTradeID         int64  `json:"L"` // 未使用，占位见上方类型注释
		Volume              string `json:"v"`
		TakerBuyBaseVolume  string `json:"V"` // 未使用，占位见上方类型注释
		QuoteVolume         string `json:"q"`
		TakerBuyQuoteVolume string `json:"Q"` // 未使用，占位见上方类型注释
		Closed              bool   `json:"x"`
	} `json:"k"`
}

// decodeKline 把 kline 帧的 data 翻成中立事件；未收线（k.x == false）不产出
// 事件——上层（stream/collector）只关心已定案的 K 线，未收线的值还会变。
func decodeKline(data json.RawMessage) (exchange.Event, error) {
	var f klineFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return exchange.Event{}, fmt.Errorf("binance: 解析 kline 帧: %w", err)
	}
	if !f.K.Closed {
		return exchange.Event{}, nil
	}
	return exchange.Event{Kline: &exchange.Kline{
		Market:       market,
		NativeSymbol: f.K.Symbol,
		Interval:     f.K.Interval,
		OpenTime:     f.K.OpenTime,
		Open:         f.K.Open,
		High:         f.K.High,
		Low:          f.K.Low,
		Close:        f.K.Close,
		Volume:       f.K.Volume,
		QuoteVolume:  f.K.QuoteVolume,
	}}, nil
}

// tickerFrame 对应 Individual Symbol Ticker Streams（24hrTicker）的 data
// 字段，字段含义见 doc.go 引用的官方样例。
//
// EventType/CloseStatsTime/BestBidQty/BestAskQty/LastTradeID 五个字段本身
// 不使用，但必须显式声明：该报文里同名大小写字段对极多——e/E（已在 eventHead
// 说明）、c/C（最新价字符串 vs 统计区间收盘时间数字）、b/B（买一价 vs 买一量，
// 两者都是字符串，类型相同，退化匹配不报错、静默覆盖，最危险）、a/A（卖一价
// vs 卖一量，同上）、l/L（最低价字符串 vs 最后成交 ID 数字）——都要占位，
// 理由同 klineFrame 的类型注释。p/P、o/O、q/Q 这三对本包都不需要，两个字母
// 都不声明即可安全跳过（没有字段声明就不会有任何一个退化匹配上）。
type tickerFrame struct {
	EventType      string `json:"e"` // 未使用，占位见上方类型注释
	EventTime      int64  `json:"E"`
	Symbol         string `json:"s"`
	LastPrice      string `json:"c"`
	CloseStatsTime int64  `json:"C"` // 未使用，占位见上方类型注释
	BidPrice       string `json:"b"`
	BestBidQty     string `json:"B"` // 未使用，占位见上方类型注释
	AskPrice       string `json:"a"`
	BestAskQty     string `json:"A"` // 未使用，占位见上方类型注释
	HighPrice      string `json:"h"`
	LowPrice       string `json:"l"`
	LastTradeID    int64  `json:"L"` // 未使用，占位见上方类型注释
	BaseVolume     string `json:"v"`
}

// normalizedTicker 是 ticker 快照归一化后写进 Snapshot.Payload 的报文主体，
// 字段形状由 OKX 一侧的实现共用，取两家交易所都有的公共子集；价格与数量
// 一律十进制字符串，理由同 Kline（转 float 会丢精度）。
type normalizedTicker struct {
	Last      string `json:"last"`
	Bid       string `json:"bid"`
	Ask       string `json:"ask"`
	High24h   string `json:"high24h"`
	Low24h    string `json:"low24h"`
	Volume24h string `json:"volume24h"`
}

// decodeTicker 把 ticker 帧的 data 翻成中立快照事件；EventTime 取报文自带
// 的事件时间（E 字段），Payload 是归一化后的报文主体，不透传 Binance 原始
// 单字母字段名——交易所方言不出本包。
func decodeTicker(data json.RawMessage) (exchange.Event, error) {
	var f tickerFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return exchange.Event{}, fmt.Errorf("binance: 解析 ticker 帧: %w", err)
	}
	payload, err := json.Marshal(normalizedTicker{
		Last:      f.LastPrice,
		Bid:       f.BidPrice,
		Ask:       f.AskPrice,
		High24h:   f.HighPrice,
		Low24h:    f.LowPrice,
		Volume24h: f.BaseVolume,
	})
	if err != nil {
		return exchange.Event{}, fmt.Errorf("binance: 序列化归一化 ticker: %w", err)
	}
	return exchange.Event{Snapshot: &exchange.Snapshot{
		Market:       market,
		NativeSymbol: f.Symbol,
		StreamType:   exchange.StreamTicker,
		EventTime:    f.EventTime,
		Payload:      payload,
	}}, nil
}

// depthFrame 对应 Partial Book Depth Streams 的 data 字段；bids/asks 每项
// 是 [价, 量] 两元素字符串数组，与本包要写的归一化形状恰好一致，字段名本身
// 是完整单词（lastUpdateId/bids/asks），互相之间没有大小写同名字段，不存在
// klineFrame/tickerFrame 那种退化匹配风险。该报文不带任何事件时间字段。
type depthFrame struct {
	Bids [][2]string `json:"bids"`
	Asks [][2]string `json:"asks"`
}

// normalizedDepth 是 depth 快照归一化后写进 Snapshot.Payload 的报文主体，
// 与 normalizedTicker 同理，两家交易所共用这个形状。
type normalizedDepth struct {
	Bids [][2]string `json:"bids"`
	Asks [][2]string `json:"asks"`
}

// decodeDepth 把浅层盘口帧的 data 翻成中立快照事件。该报文本身不带任何
// 时间戳（只有 lastUpdateId，不是事件时间），Snapshot.EventTime 按裁决要求
// 填 0，不拿本地接收时间冒充——那是 service 层的职责（recv_ts 语义是「服务
// 何时收到」，adapter 不该关心本地时钟，且集中在一处能避免两家 adapter 各写
// 一份、日后分叉）。symbol 不在 data 里，从流名（<symbol>@depth20@100ms）
// 反推还原：Binance 现货原生符号全大写字母，流名小写只是 Plan 拼 URL 的
// 约定，故直接转大写即还原。
func decodeDepth(stream string, data json.RawMessage) (exchange.Event, error) {
	var f depthFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return exchange.Event{}, fmt.Errorf("binance: 解析 depth 帧: %w", err)
	}
	payload, err := json.Marshal(normalizedDepth{Bids: f.Bids, Asks: f.Asks})
	if err != nil {
		return exchange.Event{}, fmt.Errorf("binance: 序列化归一化 depth: %w", err)
	}
	return exchange.Event{Snapshot: &exchange.Snapshot{
		Market:       market,
		NativeSymbol: symbolFromStream(stream),
		StreamType:   exchange.StreamDepth,
		EventTime:    0,
		Payload:      payload,
	}}, nil
}

// symbolFromStream 从合并流的流名里取出交易对部分并转回大写，还原成 Plan
// 生成 URL 时小写化前的原生符号形态，供 data 里不带 symbol 字段的浅层盘口
// 快照使用。
func symbolFromStream(stream string) string {
	if i := strings.IndexByte(stream, '@'); i >= 0 {
		return strings.ToUpper(stream[:i])
	}
	return strings.ToUpper(stream)
}
