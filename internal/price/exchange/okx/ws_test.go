package okx

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

// testHTTP 满足 New 对 Config.HTTP 的必填校验（见 okx.go）；本文件的用例都
// 是 Decode/Plan 这类不触网的纯函数测试，共用同一个零值 httpc.Client 实例
// 即可（约定与 binance 包一致，见 binance/ws_test.go）。
var testHTTP = httpc.New(httpc.Config{})

// TestDecode_ConfirmFlagGatesKline 验证 candle 频道推送帧只在 confirm=="1"
// （已收线）时产出 Kline 事件，且开盘时间取数组第 0 位（任务简报 Step 2 给出
// 的用例，报文取自 OKX 官方 candle 频道 Push Data Example）。candle 是数组
// 按下标解析，天生免疫 encoding/json 的大小写折叠坑（不同于 tickers，见
// doc.go 与 TestDecode_Ticker）。
func TestDecode_ConfirmFlagGatesKline(t *testing.T) {
	// candle 数组:[ts,o,h,l,c,vol,volCcy,volCcyQuote,confirm],confirm 在索引 8
	confirmed := []byte(`{"arg":{"channel":"candle1m","instId":"BTC-USDT"},"data":[["1597026383085","3.721","3.743","3.677","3.708","8422410","22698348.04","12698348.04","1"]]}`)
	unconfirmed := bytes.Replace(confirmed, []byte(`,"1"]`), []byte(`,"0"]`), 1)
	o := New(Config{HTTP: testHTTP})

	ev, err := o.Decode(confirmed)
	if err != nil {
		t.Fatalf("Decode 出错: %v", err)
	}
	if ev.Kline == nil {
		t.Fatal("confirm=1 应产出事件")
	}
	if ev.Kline.OpenTime != 1597026383085 {
		t.Errorf("OpenTime = %d", ev.Kline.OpenTime)
	}

	ev, err = o.Decode(unconfirmed)
	if err != nil {
		t.Fatalf("未收线帧不该报错: %v", err)
	}
	if ev.Kline != nil {
		t.Error("confirm=0 不得产出事件")
	}
}

// TestPlan_EmitsSubscribeFramesAndClientPing 验证 Plan 产出订阅帧与客户端
// 心跳声明（任务简报 Step 2 给出的用例）：OKX 要求客户端每 <30s 主动发
// "ping" 文本帧（已核实，见 doc.go），与 Binance 的服务端 ping 相反。
func TestPlan_EmitsSubscribeFramesAndClientPing(t *testing.T) {
	plans, err := New(Config{WSURL: "wss://x/ws/v5/public", MaxStreamsPerConn: 10, HTTP: testHTTP}).Plan(
		[]exchange.Sub{{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].Subscribe) != 1 {
		t.Fatalf("OKX 需要订阅帧, got %+v", plans)
	}
	if !bytes.Contains(plans[0].Subscribe[0], []byte(`"candle1m"`)) {
		t.Errorf("订阅帧未带频道名: %s", plans[0].Subscribe[0])
	}
	if string(plans[0].ClientPing) != "ping" || plans[0].PingEvery == 0 {
		t.Error("OKX 要求客户端主动心跳,ConnPlan 必须声明")
	}
}

// TestPlan_RoutesKlineToBusinessAndTickerDepthToPublic 验证 candle 类订阅与
// ticker/depth 类订阅被分流到两个不同的 WebSocket 端点：candle 频道只在
// /ws/v5/business 上可用，tickers/books5 在 /ws/v5/public 上——这是简报未
// 提及、核实阶段才发现的协议事实（见 doc.go）。如果不分流，candle 订阅发到
// public 端点大概率不会被推送。
func TestPlan_RoutesKlineToBusinessAndTickerDepthToPublic(t *testing.T) {
	subs := []exchange.Sub{
		{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"},
		{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamTicker},
		{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamDepth},
	}
	plans, err := New(Config{WSURL: "wss://ws.okx.com:8443/ws/v5/public", HTTP: testHTTP}).Plan(subs)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("应分成 2 条连接(business + public), got %d: %+v", len(plans), plans)
	}

	var sawBusiness, sawPublic bool
	for _, p := range plans {
		switch p.URL {
		case "wss://ws.okx.com:8443/ws/v5/business":
			sawBusiness = true
			if !bytes.Contains(p.Subscribe[0], []byte(`"candle1m"`)) {
				t.Errorf("business 连接应带 candle 订阅: %s", p.Subscribe[0])
			}
			if bytes.Contains(p.Subscribe[0], []byte(`"tickers"`)) || bytes.Contains(p.Subscribe[0], []byte(`"books5"`)) {
				t.Errorf("business 连接不该带 ticker/depth 订阅: %s", p.Subscribe[0])
			}
		case "wss://ws.okx.com:8443/ws/v5/public":
			sawPublic = true
			if bytes.Contains(p.Subscribe[0], []byte(`"candle`)) {
				t.Errorf("public 连接不该带 candle 订阅: %s", p.Subscribe[0])
			}
			if !bytes.Contains(p.Subscribe[0], []byte(`"tickers"`)) || !bytes.Contains(p.Subscribe[0], []byte(`"books5"`)) {
				t.Errorf("public 连接应同时带 tickers 与 books5 订阅: %s", p.Subscribe[0])
			}
		default:
			t.Errorf("未预期的 URL: %s", p.URL)
		}
	}
	if !sawBusiness || !sawPublic {
		t.Fatalf("business/public 两条连接都应出现, got %+v", plans)
	}
}

// TestPlan_ErrorsWhenWSURLNotPublicSuffixed 验证 Config.WSURL 不以 /public
// 结尾时，含 kline 订阅的 Plan 调用返回 error——这是本包新增、Binance 没有
// 的错误分支（businessWSURL 派生 business 端点依赖这条后缀约定，见
// ws.go businessWSURL），理应有测试钉住，防止将来重构悄悄改坏这条防线
// （评审 Important 2）。
func TestPlan_ErrorsWhenWSURLNotPublicSuffixed(t *testing.T) {
	_, err := New(Config{WSURL: "wss://x/ws/v5/business", HTTP: testHTTP}).Plan(
		[]exchange.Sub{{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"}})
	if err == nil {
		t.Fatal("WSURL 不以 /public 结尾时应报错，实际未报错")
	}
}

// TestPlan_RejectsInvalidKlineInterval 验证 Interval 大小写错误（如 "1h"
// 而不是 "1H"）在 Plan 阶段就报错，不会被直接拼接进一个无效的频道名发给
// OKX。放任这种情况会造成一条静默失败链：OKX 收到 "candle1h" 这类无效频道名
// 会回一个 event:"error" 控制帧；按 Decode 的既定契约，这类帧要当零值事件
// 忽略（见 Decode 注释）；而 stream.Conn.readLoop 只在 Decode 返回非 nil
// error 时才记日志——两层叠加，这条订阅会永远静默不工作、零日志痕迹（协调者
// 裁决 R21）。校验放在 Plan 阶段，错误在装配/重载订阅集时就暴露成一条可见
// 的 error，而不是留到运行时变成一条沉默订阅。
func TestPlan_RejectsInvalidKlineInterval(t *testing.T) {
	_, err := New(Config{WSURL: "wss://x/ws/v5/public", HTTP: testHTTP}).Plan(
		[]exchange.Sub{{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1h"}})
	if err == nil {
		t.Fatal("Interval 大小写错误(1h)应报错，实际未报错")
	}
	if !strings.Contains(err.Error(), "1h") {
		t.Errorf("错误信息应包含非法取值 %q, got: %v", "1h", err)
	}
}

// TestPlan_SplitsByStreamLimit 验证 Plan 按 MaxStreamsPerConn 切分连接：
// 5 条订阅、每连接上限 2 条，应切成 3 条连接（2+2+1），与 binance 包同名测试
// 的覆盖意图一致。
func TestPlan_SplitsByStreamLimit(t *testing.T) {
	subs := make([]exchange.Sub, 5)
	for i := range subs {
		subs[i] = exchange.Sub{Market: "spot", NativeSymbol: "S" + strconv.Itoa(i) + "-USDT", StreamType: exchange.StreamTicker}
	}
	plans, err := New(Config{WSURL: "wss://x/ws/v5/public", MaxStreamsPerConn: 2, HTTP: testHTTP}).Plan(subs)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 {
		t.Errorf("连接数 = %d, want 3(5 条订阅,每连接 2 条)", len(plans))
	}
}

// TestDecode_NonBusinessFramesAreIgnorable 覆盖 OKX 实际会持续推送、与业务
// 无关的几类帧：客户端主动 ping 后的服务端 pong 应答（纯文本，不是 JSON）、
// 订阅确认帧、错误响应帧（任务简报明确要求至少覆盖这三类），另加连接数提示
// 帧与未实现的频道两类补充。理由同 exchange.Exchange.Decode 的接口注释：
// 一旦某类帧被误判成 error，stream.Conn.readLoop 会把它当解码失败刷成日志
// 噪音。
func TestDecode_NonBusinessFramesAreIgnorable(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "pong 文本帧(非 JSON)",
			raw:  "pong",
		},
		{
			// 报文取自官方文档 candle 频道 Successful Response Example。
			name: "订阅确认帧",
			raw:  `{"id":"1512","event":"subscribe","arg":{"channel":"candle1D","instId":"BTC-USDT"},"connId":"a4d3ae55"}`,
		},
		{
			// 报文取自官方文档 candle 频道 Failure Response Example。
			name: "错误响应帧",
			raw:  `{"id":"1512","event":"error","code":"60012","msg":"Invalid request","connId":"a4d3ae55"}`,
		},
		{
			// channel-conn-count 是私有频道限流提示帧（本包不订阅私有频道，
			// 但 Event 字段的判定分支对它同样安全），报文取自官方文档样例。
			name: "连接数提示帧",
			raw:  `{"event":"channel-conn-count","channel":"orders","connCount":"2","connId":"abcd1234"}`,
		},
		{
			// trades 是 OKX 真实会推送的频道，但本包未实现该流类型的翻译；
			// 用它验证「未识别 channel」分支不会误判成错误。
			name: "未实现的频道",
			raw:  `{"arg":{"channel":"trades","instId":"BTC-USDT"},"data":[{"instId":"BTC-USDT","tradeId":"1","px":"1","sz":"1","side":"buy","ts":"1"}]}`,
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

// TestDecode_Ticker 验证 tickers 帧翻成 Snapshot：StreamType/NativeSymbol/
// EventTime 与归一化后的 Payload 内容都要对，volume24h 必须取 vol24h（基础
// 币口径）而不是 volCcy24h（计价币口径）。报文取自官方文档 tickers 频道
// Push Data Example。已核对 tickers 频道全部字段名（instType/instId/last/
// lastSz/askPx/askSz/bidPx/bidSz/open24h/high24h/low24h/volCcy24h/vol24h/
// sodUtc0/sodUtc8/ts）——两两不存在「仅大小写不同」的字段对，不会触发
// encoding/json 的大小写折叠坑（该坑详见 binance 包 ws.go 的 tickerFrame
// 注释），故本类型无需像 binance 那样加占位字段。
func TestDecode_Ticker(t *testing.T) {
	raw := []byte(`{"arg":{"channel":"tickers","instId":"BTC-USDT"},"data":[` +
		`{"instType":"SPOT","instId":"BTC-USDT","last":"9999.99","lastSz":"0.1",` +
		`"askPx":"9999.98","askSz":"11","bidPx":"8888.88","bidSz":"5",` +
		`"open24h":"9000","high24h":"10000","low24h":"8888.88",` +
		`"volCcy24h":"2222","vol24h":"3333","sodUtc0":"2222","sodUtc8":"2222",` +
		`"ts":"1597026383085"}]}`)

	ev, err := New(Config{HTTP: testHTTP}).Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.Snapshot == nil {
		t.Fatal("tickers 帧应产出 Snapshot 事件")
	}
	if ev.Snapshot.StreamType != exchange.StreamTicker {
		t.Errorf("StreamType = %q, want %q", ev.Snapshot.StreamType, exchange.StreamTicker)
	}
	if ev.Snapshot.NativeSymbol != "BTC-USDT" {
		t.Errorf("NativeSymbol = %q, want %q", ev.Snapshot.NativeSymbol, "BTC-USDT")
	}
	if ev.Snapshot.EventTime != 1597026383085 {
		t.Errorf("EventTime = %d, want 1597026383085", ev.Snapshot.EventTime)
	}

	var got normalizedTicker
	if err := json.Unmarshal(ev.Snapshot.Payload, &got); err != nil {
		t.Fatalf("Payload 不是合法 JSON: %v, payload=%s", err, ev.Snapshot.Payload)
	}
	want := normalizedTicker{Last: "9999.99", Bid: "8888.88", Ask: "9999.98", High24h: "10000", Low24h: "8888.88", Volume24h: "3333"}
	if got != want {
		t.Errorf("Payload 解出 = %+v, want %+v(volume24h 应取 vol24h 基础币口径,不是 volCcy24h)", got, want)
	}
}

// TestDecode_Depth 验证 books5 帧翻成 Snapshot：Payload 只保留每档的价与
// 量，丢弃 OKX 原始数组第 3、4 位（废弃字段固定 0、该价位订单数）；EventTime
// 取报文自带的 ts（与 binance 的浅层盘口不同——books5 本身带时间戳，不需要
// 也不允许留 0）。报文取自官方文档 books5 频道 Push Data Example。
func TestDecode_Depth(t *testing.T) {
	raw := []byte(`{"arg":{"channel":"books5","instId":"BCH-USDT"},"data":[` +
		`{"asks":[["111.06","55154","0","2"],["111.07","53276","0","2"]],` +
		`"bids":[["111.05","57745","0","2"],["111.04","57109","0","2"]],` +
		`"instId":"BCH-USDT","ts":"1670324386802","seqId":363996337}]}`)

	ev, err := New(Config{HTTP: testHTTP}).Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.Snapshot == nil {
		t.Fatal("books5 帧应产出 Snapshot 事件")
	}
	if ev.Snapshot.StreamType != exchange.StreamDepth {
		t.Errorf("StreamType = %q, want %q", ev.Snapshot.StreamType, exchange.StreamDepth)
	}
	if ev.Snapshot.NativeSymbol != "BCH-USDT" {
		t.Errorf("NativeSymbol = %q, want %q", ev.Snapshot.NativeSymbol, "BCH-USDT")
	}
	if ev.Snapshot.EventTime != 1670324386802 {
		t.Errorf("EventTime = %d, want 1670324386802(books5 自带 ts,不该是 0)", ev.Snapshot.EventTime)
	}

	var got normalizedDepth
	if err := json.Unmarshal(ev.Snapshot.Payload, &got); err != nil {
		t.Fatalf("Payload 不是合法 JSON: %v, payload=%s", err, ev.Snapshot.Payload)
	}
	want := normalizedDepth{
		Bids: [][2]string{{"111.05", "57745"}, {"111.04", "57109"}},
		Asks: [][2]string{{"111.06", "55154"}, {"111.07", "53276"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Payload 解出 = %+v, want %+v", got, want)
	}
}
