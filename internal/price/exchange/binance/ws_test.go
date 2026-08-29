package binance

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/pkg/httpc"
)

// testHTTP 满足 New 对 Config.HTTP 的必填校验（见 binance.go）；本文件的
// 用例都是 Decode/Plan 这类不触网的纯函数测试，不会真正发出请求，共用同一个
// 零值 httpc.Client 实例即可。
var testHTTP = httpc.New(httpc.Config{})

// TestDecode_ClosedKlineOnly 验证 Decode 只在 K 线收线（k.x == true）时产出事件，
// 且开盘时间取 k.t（不是收盘时间 k.T）。报文取自 Binance 官方 web-socket-streams.md
// 的 Kline/Candlestick Streams 样例。
func TestDecode_ClosedKlineOnly(t *testing.T) {
	closed := []byte(`{"stream":"bnbbtc@kline_1m","data":{"e":"kline","E":1672515782136,"s":"BNBBTC","k":{"t":1672515780000,"T":1672515839999,"s":"BNBBTC","i":"1m","o":"0.001","c":"0.002","h":"0.0025","l":"0.0015","v":"1000","q":"1","x":true}}}`)
	open := bytes.Replace(closed, []byte(`"x":true`), []byte(`"x":false`), 1)
	b := New(Config{HTTP: testHTTP})

	ev, err := b.Decode(closed)
	if err != nil {
		t.Fatalf("Decode 已收线帧出错: %v", err)
	}
	if ev.Kline == nil {
		t.Fatal("已收线的 K 线应产出事件")
	}
	if ev.Kline.OpenTime != 1672515780000 {
		t.Errorf("OpenTime = %d, want 1672515780000(取 k.t 开盘时间,不是 k.T)", ev.Kline.OpenTime)
	}
	if ev.Kline.Close != "0.002" {
		t.Errorf("Close = %q, want \"0.002\"", ev.Kline.Close)
	}

	ev, err = b.Decode(open)
	if err != nil {
		t.Fatalf("Decode 未收线帧不该报错: %v", err)
	}
	if ev.Kline != nil {
		t.Error("未收线的 K 线不得产出事件")
	}
}

// TestDecode_UnknownFrameIsIgnorable 与另外两条非业务帧一起，用表驱动的形式覆盖
// Binance 实际会推送的几类无业务含义帧：订阅确认帧、合并流的错误响应帧、以及
// 字段齐全但 e 值未知（本包未实现该流类型）的帧。三者都必须返回零值 Event 且
// err == nil——一旦某类帧被误判成 error，readLoop 会把它当解码失败记日志，
// 交易所持续推送这类帧时日志会被噪音淹没（见 stream.Conn.readLoop）。
func TestDecode_UnknownFrameIsIgnorable(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "订阅确认帧",
			raw:  `{"result":null,"id":1}`,
		},
		{
			// 报文取自官方文档 Error Messages 一节：JSON 语法错误时的响应，
			// 无 stream/data 包裹。
			name: "合并流的错误响应帧",
			raw:  `{"code":3,"msg":"Invalid JSON: expected value at line 1 column 1"}`,
		},
		{
			// aggTrade 是 Binance 真实会推送的流，但本包未实现聚合成交流的翻译；
			// 报文取自官方文档 Aggregate Trade Streams 样例，字段齐全但 data.e
			// 不是本包认识的取值，用它验证「未识别类型」分支不会误报错误。
			name: "字段齐全但 e 值未知的帧",
			raw:  `{"stream":"bnbbtc@aggTrade","data":{"e":"aggTrade","E":1672515782136,"s":"BNBBTC","a":12345,"p":"0.001","q":"100","f":100,"l":105,"T":1672515782136,"m":true,"M":true}}`,
		},
		{
			// bookTicker 与 depth 一样不带 e 字段，但本包未实现该流的翻译（Plan 也
			// 不会生成 bookTicker 的订阅）；报文取自官方文档 Individual Symbol Book
			// Ticker Streams 样例，用来验证「e 为空且流名不含 @depth」这条兜底分支
			// 仍然安全可忽略（depth 本身已经从这张表里挪到 TestDecode_Depth，因为
			// 它现在会产出 Snapshot，不再是「可忽略」）。
			name: "无 e 字段且非 depth 流的帧",
			raw:  `{"stream":"bnbbtc@bookTicker","data":{"u":400900217,"s":"BNBUSDT","b":"25.35190000","B":"31.21000000","a":"25.36520000","A":"40.66000000"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := New(Config{HTTP: testHTTP}).Decode([]byte(tt.raw))
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if ev.Kline != nil || ev.Snapshot != nil {
				t.Error("不该产出事件")
			}
		})
	}
}

// TestPlan_SplitsByStreamLimit 验证 Plan 按 MaxStreamsPerConn 切分连接：
// 5 条订阅、每连接上限 2 条，应切成 3 条连接（2+2+1）。
func TestPlan_SplitsByStreamLimit(t *testing.T) {
	subs := make([]exchange.Sub, 5)
	for i := range subs {
		subs[i] = exchange.Sub{Market: "spot", NativeSymbol: "S" + strconv.Itoa(i), StreamType: exchange.StreamKline, Interval: "1m"}
	}
	plans, err := New(Config{WSURL: "wss://x/stream", MaxStreamsPerConn: 2, HTTP: testHTTP}).Plan(subs)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Errorf("连接数 = %d, want 3(5 条订阅,每连接 2 条)", len(plans))
	}
}

// TestPlan_RejectsInvalidKlineInterval 验证 Interval 大小写错误（如 "1H"
// 而不是 "1h"）在 Plan 阶段就报错，不会被直接拼接进一个无效的流名发给
// Binance——与 okx 包同名测试对称（必修 6）。Binance 把一家交易所的全部
// 订阅打进一条合并流 URL（见 ws.go Plan），一条无效流名会让整条连接的拨号
// 被拒或握手后持续报错，不是只有这一条订阅失效，爆炸半径比 OKX 侧更大。
func TestPlan_RejectsInvalidKlineInterval(t *testing.T) {
	_, err := New(Config{WSURL: "wss://x/stream", HTTP: testHTTP}).Plan(
		[]exchange.Sub{{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1H"}})
	if err == nil {
		t.Fatal("Interval 大小写错误(1H)应报错，实际未报错")
	}
	if !strings.Contains(err.Error(), "1H") {
		t.Errorf("错误信息应包含非法取值 %q, got: %v", "1H", err)
	}
}

// TestDecode_Ticker 验证 24hrTicker 帧翻成 Snapshot：StreamType/NativeSymbol/
// EventTime 与归一化后的 Payload 内容都要对。报文里每一对大小写同名字段
// （c/C、b/B、a/A、l/L）都填成互不相同的值，任何一处退化匹配（见 ws.go
// tickerFrame 的类型注释）都会让某个字段落错位置，从而让下面的断言失败——
// 这是刚踩过的 encoding/json 大小写折叠坑在 ticker 上的第二个入口，专门核对过。
func TestDecode_Ticker(t *testing.T) {
	raw := []byte(`{"stream":"bnbbtc@ticker","data":{` +
		`"e":"24hrTicker","E":1672515782136,"s":"BNBBTC",` +
		`"p":"0.0015","P":"250.00","w":"0.0018","x":"0.0009",` +
		`"c":"111","Q":"999","b":"222","B":"888","a":"333","A":"777",` +
		`"o":"0.0010","h":"555","l":"444","v":"666","q":"123",` +
		`"O":0,"C":999999,"F":0,"L":18150,"n":18151}}`)

	ev, err := New(Config{HTTP: testHTTP}).Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.Snapshot == nil {
		t.Fatal("ticker 帧应产出 Snapshot 事件")
	}
	if ev.Snapshot.StreamType != exchange.StreamTicker {
		t.Errorf("StreamType = %q, want %q", ev.Snapshot.StreamType, exchange.StreamTicker)
	}
	if ev.Snapshot.NativeSymbol != "BNBBTC" {
		t.Errorf("NativeSymbol = %q, want %q", ev.Snapshot.NativeSymbol, "BNBBTC")
	}
	if ev.Snapshot.EventTime != 1672515782136 {
		t.Errorf("EventTime = %d, want 1672515782136", ev.Snapshot.EventTime)
	}

	var got normalizedTicker
	if err := json.Unmarshal(ev.Snapshot.Payload, &got); err != nil {
		t.Fatalf("Payload 不是合法 JSON: %v, payload=%s", err, ev.Snapshot.Payload)
	}
	want := normalizedTicker{Last: "111", Bid: "222", Ask: "333", High24h: "555", Low24h: "444", Volume24h: "666"}
	if got != want {
		t.Errorf("Payload 解出 = %+v, want %+v（任一字段错位都说明撞上了 c/C、b/B、a/A、l/L 某一对的大小写折叠）", got, want)
	}
}

// TestDecode_Depth 验证浅层盘口帧翻成 Snapshot：该报文本身不带 symbol 与
// 时间戳，NativeSymbol 从流名反推、EventTime 必须是 0（不拿本地时间冒充，
// 见 decodeDepth 的注释），Payload 只保留 bids/asks、丢弃 lastUpdateId。
func TestDecode_Depth(t *testing.T) {
	raw := []byte(`{"stream":"bnbbtc@depth20@100ms","data":{"lastUpdateId":160,"bids":[["0.0024","10"]],"asks":[["0.0026","100"]]}}`)

	ev, err := New(Config{HTTP: testHTTP}).Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.Snapshot == nil {
		t.Fatal("depth 帧应产出 Snapshot 事件")
	}
	if ev.Snapshot.StreamType != exchange.StreamDepth {
		t.Errorf("StreamType = %q, want %q", ev.Snapshot.StreamType, exchange.StreamDepth)
	}
	if ev.Snapshot.NativeSymbol != "BNBBTC" {
		t.Errorf("NativeSymbol = %q, want %q（应从流名 bnbbtc@depth20@100ms 反推并转大写）", ev.Snapshot.NativeSymbol, "BNBBTC")
	}
	if ev.Snapshot.EventTime != 0 {
		t.Errorf("EventTime = %d, want 0（该报文不带事件时间，不该拿本地时间冒充）", ev.Snapshot.EventTime)
	}

	var got normalizedDepth
	if err := json.Unmarshal(ev.Snapshot.Payload, &got); err != nil {
		t.Fatalf("Payload 不是合法 JSON: %v, payload=%s", err, ev.Snapshot.Payload)
	}
	want := normalizedDepth{Bids: [][2]string{{"0.0024", "10"}}, Asks: [][2]string{{"0.0026", "100"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Payload 解出 = %+v, want %+v", got, want)
	}
}
