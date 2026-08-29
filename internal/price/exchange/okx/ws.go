package okx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// clientPingInterval 客户端主动心跳的发送间隔。OKX 要求连接在 30 秒内收不到
// 任何消息就会被服务端断开，官方建议「收到上一条消息后设一个 <30s 的定时器，
// 触发即发送文本 'ping'」（已核实，见 doc.go）；本包取 20s，留 10s 安全余量，
// 与任务简报给出的建议值一致。
const clientPingInterval = 20 * time.Second

// clientPingText OKX 要求的客户端心跳帧内容：纯文本 "ping"（不是 JSON），
// 期待服务端回应同样是纯文本的 "pong"（见 Decode 对 pong 的特判）。
var clientPingText = []byte("ping")

// businessWSURL 从 Config.WSURL（公共频道地址）派生 candle 类频道实际要连的
// business 端点地址。OKX 把 candle 频道单独放在 /ws/v5/business 这一独立
// 连接上，与 tickers/books5 所在的 /ws/v5/public 是两条不同的连接，不能通过
// 订阅帧或 query 参数切换（已核实，见 doc.go——这是简报未提及、核实阶段才
// 发现的协议事实）。cfg.WSURL 按约定必须是 public 地址（configs/price.yaml
// 现值即如此），故用「替换末段路径」的方式派生，不在 Config 里为此新增字段
// （Config 字段名须与 binance 保持一致，见 okx.go）。WSURL 不以 /public
// 结尾时返回 error——这是运行期取决于 yaml 怎么填的情况，不在装配期必然
// 发生，按宪法第 1 条不 panic，走正常错误路径。
func businessWSURL(publicURL string) (string, error) {
	if !strings.HasSuffix(publicURL, "/public") {
		return "", fmt.Errorf("okx: WSURL 应以 /public 结尾以便派生 business 端点地址(candle 频道所在), got %q", publicURL)
	}
	return strings.TrimSuffix(publicURL, "/public") + "/business", nil
}

// wsArg 是 OKX 订阅帧与推送帧共用的 arg 对象形状：channel 与 instId 合起来
// 唯一确定一条频道；candle 类频道把周期直接编进 channel 名本身（如
// "candle1m"），不是另开一个 interval 字段。
type wsArg struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

// validBars 是 candle 频道支持的全部周期拼写白名单（27 个，已核实，见
// doc.go），channelFor 用它校验 kline 订阅的 Sub.Interval。不校验会有一条
// 隐蔽的静默失败链：上游若把周期写成 "1h" 而不是 "1H"，Plan 会照样拼出一个
// 形如 "candle1h" 的无效频道名发给 OKX；OKX 会回一个 event:"error" 控制帧，
// 但按 Decode 的既定契约这类帧要当零值事件忽略（见 Decode 注释）；而
// stream.Conn.readLoop 只在 Decode 返回非 nil error 时才记日志——两层叠加，
// 这条订阅会永远静默不工作、零日志痕迹（协调者裁决 R21）。把校验挪到 Plan
// 阶段，错误在装配/重载订阅集时就暴露成一条可见的 error，而不是留到运行时
// 变成一条沉默订阅。
var validBars = map[string]bool{
	"3M": true, "1M": true, "1W": true, "1D": true, "2D": true, "3D": true, "5D": true,
	"12H": true, "6H": true, "4H": true, "2H": true, "1H": true,
	"30m": true, "15m": true, "5m": true, "3m": true, "1m": true, "1s": true,
	"3Mutc": true, "1Mutc": true, "1Wutc": true, "1Dutc": true, "2Dutc": true,
	"3Dutc": true, "5Dutc": true, "12Hutc": true, "6Hutc": true,
}

// channelFor 把一条订阅翻成 OKX 原生的 arg 对象（已核实的频道名拼写见
// doc.go）：
//   - kline: candle<bar>，bar 先过 validBars 白名单校验（OKX 的周期拼写
//     大小写敏感——分钟/秒小写、小时/天/周/月大写，本包不做任何大小写转换，
//     配置/DB 里存的 Interval 必须已经是 OKX 原生拼写；校验不通过直接
//     返回 error，不放行到字符串拼接），校验通过后原样拼接；
//   - ticker: tickers；
//   - depth: books5（5 档快照，档位固定，Sub 未携带可配置项，本包不引入这份
//     配置——按「只写要求的东西」处理，与 binance 包 streamName 对 depth 的
//     处理方式一致）。
func channelFor(s exchange.Sub) (wsArg, error) {
	switch s.StreamType {
	case exchange.StreamKline:
		if !validBars[s.Interval] {
			return wsArg{}, fmt.Errorf("okx: 不支持的 K 线周期 %q", s.Interval)
		}
		return wsArg{Channel: "candle" + s.Interval, InstID: s.NativeSymbol}, nil
	case exchange.StreamTicker:
		return wsArg{Channel: "tickers", InstID: s.NativeSymbol}, nil
	case exchange.StreamDepth:
		return wsArg{Channel: "books5", InstID: s.NativeSymbol}, nil
	default:
		return wsArg{}, fmt.Errorf("okx: 不支持的流类型 %q", s.StreamType)
	}
}

// subscribeFrame 是 OKX 订阅请求的报文形状：一个 op 消息可以带多个 args，
// 本包按 MaxStreamsPerConn 切出的每个分片只发一条 subscribe 帧、把分片内
// 全部订阅一次性带上，不逐条发送。
type subscribeFrame struct {
	Op   string  `json:"op"`
	Args []wsArg `json:"args"`
}

// Plan 把订阅按频道所在端点分成两组（candle → business，ticker/depth →
// public，理由见 businessWSURL），组内再按 MaxStreamsPerConn 切分成若干条
// 连接；每条连接的订阅打包成一条 subscribe 帧发送；ClientPing/PingEvery
// 对两组连接都要声明——OKX 的心跳要求对 public/business 端点同样生效
// （已核实，见 doc.go 的 Connect 小节，未按端点区分）。
func (o *OKX) Plan(subs []exchange.Sub) ([]exchange.ConnPlan, error) {
	limit := o.maxStreamsPerConn()

	var klineSubs, snapshotSubs []exchange.Sub
	for _, s := range subs {
		if s.StreamType == exchange.StreamKline {
			klineSubs = append(klineSubs, s)
		} else {
			snapshotSubs = append(snapshotSubs, s)
		}
	}

	var plans []exchange.ConnPlan
	if len(klineSubs) > 0 {
		bizURL, err := businessWSURL(o.cfg.WSURL)
		if err != nil {
			return nil, err
		}
		chunks, err := planChunks(klineSubs, bizURL, limit)
		if err != nil {
			return nil, err
		}
		plans = append(plans, chunks...)
	}
	if len(snapshotSubs) > 0 {
		chunks, err := planChunks(snapshotSubs, o.cfg.WSURL, limit)
		if err != nil {
			return nil, err
		}
		plans = append(plans, chunks...)
	}
	return plans, nil
}

// planChunks 把同一个 URL 下的订阅按 limit 切分成若干条连接计划，每条连接
// 一条打包了本分片全部订阅的 subscribe 帧。
func planChunks(subs []exchange.Sub, url string, limit int) ([]exchange.ConnPlan, error) {
	var plans []exchange.ConnPlan
	for start := 0; start < len(subs); start += limit {
		end := start + limit
		if end > len(subs) {
			end = len(subs)
		}
		chunk := subs[start:end]

		args := make([]wsArg, 0, len(chunk))
		for _, s := range chunk {
			arg, err := channelFor(s)
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
		}
		frame, err := json.Marshal(subscribeFrame{Op: "subscribe", Args: args})
		if err != nil {
			return nil, fmt.Errorf("okx: 序列化订阅帧: %w", err)
		}

		plans = append(plans, exchange.ConnPlan{
			URL:        url,
			Subscribe:  [][]byte{frame},
			Subs:       chunk,
			ClientPing: clientPingText,
			PingEvery:  clientPingInterval,
		})
	}
	return plans, nil
}

// pushFrame 是 OKX 推送帧与控制帧共用的外层包裹。控制帧（订阅确认、错误
// 响应、channel-conn-count 提示等）都带非空 Event，Data 为空；业务推送帧
// Event 为空、Data 非空，按 Arg.Channel 分派。两类帧不会同时满足「Event
// 非空且 Data 非空」，故先判 Event 再判 Data 即可覆盖全部已知形状。
type pushFrame struct {
	Event string          `json:"event"`
	Arg   wsArg           `json:"arg"`
	Data  json.RawMessage `json:"data"`
}

// Decode 解一帧原始 WebSocket 消息。OKX 要求客户端心跳、服务端应答的 pong
// 是纯文本（不是 JSON），必须在尝试 json.Unmarshal 之前特判掉，否则会被当成
// 一次真正的解析失败报错（已核实，见 doc.go；这正是简报要求补的行为级测试
// 要防的那类风险）。JSON 帧里 Event 非空即控制帧（订阅确认/退订确认/错误
// 响应/channel-conn-count 等），一律忽略；Data 为空同样忽略；否则按
// Arg.Channel 前缀/精确匹配分派——candle 前缀→K 线，tickers/books5 精确匹配
// →快照，其余（本包未订阅、也未实现翻译的频道，如 trades）一律按「未识别，
// 可忽略」处理，不当作错误：这类帧交易所会持续推送，误判成错误会在
// stream.Conn.readLoop 里被当解码失败刷成日志噪音。
func (o *OKX) Decode(raw []byte) (exchange.Event, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("pong")) {
		return exchange.Event{}, nil
	}

	var f pushFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 解析帧: %w", err)
	}
	if f.Event != "" || len(f.Data) == 0 {
		return exchange.Event{}, nil
	}

	switch {
	case strings.HasPrefix(f.Arg.Channel, "candle"):
		return decodeKline(f.Arg.InstID, f.Arg.Channel, f.Data)
	case f.Arg.Channel == "tickers":
		return decodeTicker(f.Arg.InstID, f.Data)
	case f.Arg.Channel == "books5":
		return decodeDepth(f.Arg.InstID, f.Data)
	default:
		return exchange.Event{}, nil
	}
}

// candleFields 是 candle 原生数组的字段个数：[ts,o,h,l,c,vol,volCcy,
// volCcyQuote,confirm]（已核实，见 doc.go）。WS 推送与两个 REST 历史端点
// 三处报文形状完全一致，共用同一套下标解析（见 parseCandleArray），不重复
// 写三遍。
const candleFields = 9

// okxCandle 是 parseCandleArray 的解析结果，WS 推送（decodeKline）与 REST
// 响应（rest.go 的 Klines）共用。
type okxCandle struct {
	OpenTime               int64
	Open, High, Low, Close string
	Volume                 string // 基础币成交量，对应数组索引 5（vol）
	QuoteVolume            string // 计价币成交额，对应数组索引 6（volCcy）；理由见下方注释
	Confirmed              bool   // 数组索引 8（confirm）=="1"
}

// parseCandleArray 把 candle 的原生数组形态解析成 okxCandle。QuoteVolume 取
// 索引 6（volCcy）而不是索引 7（volCcyQuote）：官方文档里 vol/volCcy 是同一
// 对按 SPOT/MARGIN 场景明确描述的字段（"vol...If it is SPOT/MARGIN, the
// value is the quantity in base currency."、"volCcy...If it is SPOT/MARGIN,
// the value is the quantity in quote currency."），与 Binance 的 v（基础币）
// /q（计价币）语义直接对应；volCcyQuote 是衍生品场景下的规范化计价币字段，
// 在 SPOT 上与 volCcy 数值相同，两者等价，选前者是为了和"vol/volCcy 成对"
// 的官方叙述保持一致，不是因为后者不可用。数组是按下标解析，不经过
// encoding/json 的字段名匹配，不受大小写折叠影响（见 doc.go）。
func parseCandleArray(row []string) (okxCandle, error) {
	if len(row) < candleFields {
		return okxCandle{}, fmt.Errorf("okx: candle 数组字段不足: 需要至少 %d 个,got %d", candleFields, len(row))
	}
	ts, err := strconv.ParseInt(row[0], 10, 64)
	if err != nil {
		return okxCandle{}, fmt.Errorf("okx: 解析 candle 开盘时间: %w", err)
	}
	return okxCandle{
		OpenTime: ts, Open: row[1], High: row[2], Low: row[3], Close: row[4],
		Volume: row[5], QuoteVolume: row[6], Confirmed: row[8] == "1",
	}, nil
}

// decodeKline 把 candle 频道推送帧的 data 翻成中立事件；data 是「数组的
// 数组」（目前每次推送只含一根），未收线（confirm != "1"）不产出事件——
// 上层（stream/collector）只关心已定案的 K 线，未收线的值还会变（与
// binance 的 x 字段语义一致）。
func decodeKline(instID, channel string, data json.RawMessage) (exchange.Event, error) {
	var rows [][]string
	if err := json.Unmarshal(data, &rows); err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 解析 candle 帧: %w", err)
	}
	if len(rows) == 0 {
		return exchange.Event{}, nil
	}
	c, err := parseCandleArray(rows[0])
	if err != nil {
		return exchange.Event{}, err
	}
	if !c.Confirmed {
		return exchange.Event{}, nil
	}
	return exchange.Event{Kline: &exchange.Kline{
		Market:       market,
		NativeSymbol: instID,
		Interval:     strings.TrimPrefix(channel, "candle"),
		OpenTime:     c.OpenTime,
		Open:         c.Open,
		High:         c.High,
		Low:          c.Low,
		Close:        c.Close,
		Volume:       c.Volume,
		QuoteVolume:  c.QuoteVolume,
	}}, nil
}

// tickerData 对应 tickers 频道 data 数组里的单个元素，字段含义见 doc.go
// 引用的官方样例。已核对该频道全部字段名，两两之间不存在「仅大小写不同」的
// 字段对，不会触发 binance 包 tickerFrame 注释里说明的那种 encoding/json
// 大小写折叠坑，因此本类型无需像 binance 那样加占位字段（核对结论详见
// doc.go）。
type tickerData struct {
	Last      string `json:"last"`
	BidPx     string `json:"bidPx"`
	AskPx     string `json:"askPx"`
	High24h   string `json:"high24h"`
	Low24h    string `json:"low24h"`
	Vol24h    string `json:"vol24h"`    // 24h 成交量,基础币口径——本包 volume24h 取这个
	VolCcy24h string `json:"volCcy24h"` // 未使用:24h 成交量,计价币口径,占位说明见上方类型注释
	Ts        string `json:"ts"`
}

// normalizedTicker 是 ticker 快照归一化后写进 Snapshot.Payload 的报文主体，
// 字段形状是 binance/okx 两个 adapter 共享的契约，见 exchange.Snapshot 的
// 类型注释。
type normalizedTicker struct {
	Last      string `json:"last"`
	Bid       string `json:"bid"`
	Ask       string `json:"ask"`
	High24h   string `json:"high24h"`
	Low24h    string `json:"low24h"`
	Volume24h string `json:"volume24h"`
}

// decodeTicker 把 tickers 帧的 data 翻成中立快照事件；data 是数组，目前只
// 含一个元素。EventTime 取报文自带的 ts；Volume24h 取 vol24h（基础币口径），
// 不是 volCcy24h（计价币口径）——与 binance 的 v 字段口径一致（已核实，见
// doc.go 与 exchange.Snapshot 的类型注释）。
func decodeTicker(instID string, data json.RawMessage) (exchange.Event, error) {
	var rows []tickerData
	if err := json.Unmarshal(data, &rows); err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 解析 ticker 帧: %w", err)
	}
	if len(rows) == 0 {
		return exchange.Event{}, nil
	}
	r := rows[0]
	eventTime, err := parseEventTime(r.Ts)
	if err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 解析 ticker 时间戳: %w", err)
	}
	payload, err := json.Marshal(normalizedTicker{
		Last: r.Last, Bid: r.BidPx, Ask: r.AskPx,
		High24h: r.High24h, Low24h: r.Low24h, Volume24h: r.Vol24h,
	})
	if err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 序列化归一化 ticker: %w", err)
	}
	return exchange.Event{Snapshot: &exchange.Snapshot{
		Market:       market,
		NativeSymbol: instID,
		StreamType:   exchange.StreamTicker,
		EventTime:    eventTime,
		Payload:      payload,
	}}, nil
}

// booksData 对应 books5 频道 data 数组里的单个元素，字段含义见 doc.go 引用
// 的官方样例；asks/bids 每档是 4 元素数组 [价, 量, 已废弃恒为 "0" 的占位
// 字段, 该价位订单数]。
type booksData struct {
	Asks [][]string `json:"asks"`
	Bids [][]string `json:"bids"`
	Ts   string     `json:"ts"`
}

// normalizedDepth 是 depth 快照归一化后写进 Snapshot.Payload 的报文主体，
// 与 normalizedTicker 同理，共享契约见 exchange.Snapshot 的类型注释。
type normalizedDepth struct {
	Bids [][2]string `json:"bids"`
	Asks [][2]string `json:"asks"`
}

// decodeDepth 把 books5 帧的 data 翻成中立快照事件。每档原生数组的第 3、4
// 位（固定为 "0" 的废弃字段、该价位订单数）不进归一化结果，只保留价与量，
// 与 binance 的深度快照形状对齐（共享契约见 exchange.Snapshot）。EventTime
// 取报文自带的 ts——与 binance 的浅层盘口不同，books5 本身带时间戳，不需要
// 也不允许留 0（已核实，见 doc.go）。
func decodeDepth(instID string, data json.RawMessage) (exchange.Event, error) {
	var rows []booksData
	if err := json.Unmarshal(data, &rows); err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 解析 books5 帧: %w", err)
	}
	if len(rows) == 0 {
		return exchange.Event{}, nil
	}
	r := rows[0]
	eventTime, err := parseEventTime(r.Ts)
	if err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 解析 books5 时间戳: %w", err)
	}

	bids, err := trimDepthLevels(r.Bids)
	if err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 解析 books5 bids: %w", err)
	}
	asks, err := trimDepthLevels(r.Asks)
	if err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 解析 books5 asks: %w", err)
	}

	payload, err := json.Marshal(normalizedDepth{Bids: bids, Asks: asks})
	if err != nil {
		return exchange.Event{}, fmt.Errorf("okx: 序列化归一化 depth: %w", err)
	}
	return exchange.Event{Snapshot: &exchange.Snapshot{
		Market:       market,
		NativeSymbol: instID,
		StreamType:   exchange.StreamDepth,
		EventTime:    eventTime,
		Payload:      payload,
	}}, nil
}

// trimDepthLevels 只保留每档的价与量（原生数组前两项），丢弃第 3、4 位。
func trimDepthLevels(levels [][]string) ([][2]string, error) {
	out := make([][2]string, 0, len(levels))
	for _, lv := range levels {
		if len(lv) < 2 {
			return nil, fmt.Errorf("档位字段不足: 需要至少 2 个,got %d", len(lv))
		}
		out = append(out, [2]string{lv[0], lv[1]})
	}
	return out, nil
}

// parseEventTime 把 OKX 报文里的字符串毫秒时间戳解析成 int64；空字符串视为
// 0（该字段理论上必带，留这条兜底只是防御，不代表业务上允许缺失）。
func parseEventTime(ts string) (int64, error) {
	if ts == "" {
		return 0, nil
	}
	return strconv.ParseInt(ts, 10, 64)
}
