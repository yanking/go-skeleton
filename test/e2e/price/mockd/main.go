// Command mockd 是 price 服务端对端测试的 Binance 现货下游模拟器：起两个
// HTTP 监听——一个升级为 WebSocket 模拟合并流推送，一个模拟 REST——分别对应
// configs/price.yaml 里 exchanges.binance 的 ws_url/rest_url 两段，与真实
// Binance 域名拆成 stream.binance.com/api.binance.com 两个地址的形状一致。
// 报文外壳与字段名照抄 internal/price/exchange/binance 的 ws.go/rest.go
// 解析结构与其测试文件里的官方样例，不自创字段。
//
// ws 面（GET /ws）：每条新连接固定推四帧——两根已收线 K 线、一根未收线 K 线
// （验证 price 服务按 k.x 丢弃未收线帧）、一帧浅层盘口（验证 Redis 最新值写入）。
// 另有 POST /push-late-klines：不属于 Binance 协议，是 run.sh 的测试控制口子，
// 供其在发 SIGTERM 前按需向全部在线连接追加推送已收线 K 线——用于验证停机
// 时仍攒在内存批次里的"在途" K 线经排空落盘，不因进程退出而丢失（这条不变量
// 由 cmd/price/initial/init_app.go 的组件注册顺序保证，见其 writerComponent
// 类型注释）。
//
// REST 面：GET /api/v3/exchangeInfo 返回两个交易对（供 price instruments
// 断言落库行数）；GET /api/v3/klines 返回三根固定历史 K 线，不看查询参数——
// run.sh 传给 price backfill 的 -from/-to 经过挑选，保证 Klines 的翻页在这
// 三根之后就终止（见 run.sh 对应注释），不会因为忽略查询参数而死循环。
//
// 全部开盘时间与交易对写死成常量，不取 time.Now()：run.sh 的断言 SQL 按这些
// 字面量核对，不必先查库反推：streamOpenTimeBase 两根用于常驻阶段断言，
// lateOpenTimeBase 两根用于 SIGTERM 停机断言，backfillOpenTimeBase 三根用于
// backfill 子命令断言，三组开盘时间互不重叠，断言互不干扰。
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// symbol 是本次 E2E 唯一订阅、也是历史 K 线归属的现货交易对。
const symbol = "BTCUSDT"

// 三组开盘时间与统一的 1 分钟周期，含义见包注释。
const (
	streamOpenTimeBase   = int64(1700000000000) // 常驻阶段：ws 推送的两根已收线
	lateOpenTimeBase     = int64(1700000600000) // SIGTERM 停机断言：/push-late-klines 追加推送
	backfillOpenTimeBase = int64(1600000000000) // backfill 子命令：REST 历史 K 线
	klineIntervalMs      = int64(60000)         // 1m
)

func main() {
	wsAddr := flag.String("ws-addr", "127.0.0.1:18190", "ws 监听地址(对应 exchanges.binance.ws_url)")
	restAddr := flag.String("rest-addr", "127.0.0.1:18191", "REST 监听地址(对应 exchanges.binance.rest_url)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	h := newHub(logger)

	wsMux := http.NewServeMux()
	wsMux.HandleFunc("GET /ws", h.handleWS)
	wsMux.HandleFunc("POST /push-late-klines", h.handlePushLate)

	restMux := http.NewServeMux()
	restMux.HandleFunc("GET /api/v3/exchangeInfo", handleExchangeInfo)
	restMux.HandleFunc("GET /api/v3/klines", handleKlines)

	errCh := make(chan error, 2)
	go func() { errCh <- http.ListenAndServe(*wsAddr, wsMux) }()
	go func() { errCh <- http.ListenAndServe(*restAddr, restMux) }()

	logger.Info("mockd 下游模拟器就绪", "ws_addr", *wsAddr, "rest_addr", *restAddr)
	logger.Error("mockd 退出", "err", <-errCh)
}

// hub 持有全部当前在线的 ws 连接，供 /push-late-klines 按需广播；同一时刻
// price 服务只会建一条连接（本次订阅只有一条，见 run.sh 写入的唯一订阅行），
// 但仍按多连接实现，不假设只有一个客户端。
type hub struct {
	logger *slog.Logger
	mu     sync.Mutex
	conns  map[*websocket.Conn]struct{}
}

func newHub(logger *slog.Logger) *hub {
	return &hub{logger: logger, conns: make(map[*websocket.Conn]struct{})}
}

func (h *hub) register(c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
}

func (h *hub) unregister(c *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
}

func (h *hub) snapshot() []*websocket.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*websocket.Conn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	return out
}

// handleWS 升级为 WebSocket 并推固定脚本，随后一直阻塞到连接断开——写法照抄
// internal/price/stream/conn_test.go 里已验证可用的服务端测试桩模式
// （websocket.Accept + <-r.Context().Done()）。
func (h *hub) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.logger.Error("ws 升级失败", "err", err)
		return
	}
	defer c.CloseNow()

	h.register(c)
	defer h.unregister(c)

	ctx := r.Context()
	script := [][]byte{
		klineMsg(streamOpenTimeBase, true),
		klineMsg(streamOpenTimeBase+klineIntervalMs, true),
		klineMsg(streamOpenTimeBase+2*klineIntervalMs, false), // 未收线：price 服务应丢弃，不得落库
		depthMsg(),
	}
	for _, frame := range script {
		if err := c.Write(ctx, websocket.MessageText, frame); err != nil {
			h.logger.Warn("推送初始脚本失败", "err", err)
			return
		}
	}
	h.logger.Info("ws 连接就绪，已推送初始脚本", "remote", r.RemoteAddr)

	<-ctx.Done()
}

// handlePushLate 向全部在线连接追加推送两根已收线 K 线（开盘时间见
// lateOpenTimeBase）；不是 Binance 协议的一部分，是 run.sh 在发 SIGTERM 前
// 用于制造"在途未落盘" K 线的测试控制口子，见包注释。
func (h *hub) handlePushLate(w http.ResponseWriter, r *http.Request) {
	targets := h.snapshot()
	frames := [][]byte{
		klineMsg(lateOpenTimeBase, true),
		klineMsg(lateOpenTimeBase+klineIntervalMs, true),
	}
	sent := 0
	for _, c := range targets {
		for _, frame := range frames {
			if err := c.Write(r.Context(), websocket.MessageText, frame); err != nil {
				h.logger.Warn("推送在途 K 线失败", "err", err)
				continue
			}
			sent++
		}
	}
	h.logger.Info("已推送在途 K 线", "conns", len(targets), "frames_sent", sent)
	w.WriteHeader(http.StatusNoContent)
}

// wsFrame 是合并流的外层包裹，字段名与 internal/price/exchange/binance/ws.go
// 的 frame 类型对齐。
type wsFrame struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// klineData/klinePayload 字段名与 binance/ws.go 的 klineFrame 对齐（只填该包
// 实际读取的字段，未用到的大小写同名字段——如 T/L/V/Q——不必占位，mockd 不解
// 自己产出的报文，不存在退化匹配风险）。
type klineData struct {
	E string       `json:"e"`
	S string       `json:"s"`
	K klinePayload `json:"k"`
}

type klinePayload struct {
	OpenTime    int64  `json:"t"`
	Symbol      string `json:"s"`
	Interval    string `json:"i"`
	Open        string `json:"o"`
	Close       string `json:"c"`
	High        string `json:"h"`
	Low         string `json:"l"`
	Volume      string `json:"v"`
	QuoteVolume string `json:"q"`
	Closed      bool   `json:"x"`
}

// klineMsg 拼一帧 Kline/Candlestick Streams 消息；closed 对应官方字段 k.x。
func klineMsg(openTime int64, closed bool) []byte {
	data := mustMarshal(klineData{
		E: "kline",
		S: symbol,
		K: klinePayload{
			OpenTime: openTime, Symbol: symbol, Interval: "1m",
			Open: "50000.00", High: "50100.00", Low: "49900.00", Close: "50050.00",
			Volume: "10.5", QuoteVolume: "525000.00",
			Closed: closed,
		},
	})
	stream := strings.ToLower(symbol) + "@kline_1m"
	return mustMarshal(wsFrame{Stream: stream, Data: data})
}

// depthData 字段名与 binance/ws.go 的 depthFrame 对齐。
type depthData struct {
	LastUpdateID int64       `json:"lastUpdateId"`
	Bids         [][2]string `json:"bids"`
	Asks         [][2]string `json:"asks"`
}

// depthMsg 拼一帧 Partial Book Depth Streams 消息；该报文不带 symbol，price
// 服务从流名 <symbol>@depth20@100ms 反推，故流名必须与 symbol 对应。
func depthMsg() []byte {
	data := mustMarshal(depthData{
		LastUpdateID: 1,
		Bids:         [][2]string{{"49999.50", "1.2"}},
		Asks:         [][2]string{{"50000.50", "0.8"}},
	})
	stream := strings.ToLower(symbol) + "@depth20@100ms"
	return mustMarshal(wsFrame{Stream: stream, Data: data})
}

// mustMarshal 序列化 mockd 自己拼的固定样例；入参均为本文件内写死的字面量
// 结构体，不会失败，panic 只用于在这类"不可能失败"的场景下让编译器不必处理
// 一个实际不会发生的 error 返回值。
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// handleExchangeInfo 模拟 GET /api/v3/exchangeInfo：两个 USDT 现货交易对，
// 报文形状（symbol/status/baseAsset/quoteAsset）取自
// internal/price/exchange/binance/rest_test.go 的官方样例。
func handleExchangeInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"symbols": []map[string]any{
			{"symbol": "BTCUSDT", "status": "TRADING", "baseAsset": "BTC", "quoteAsset": "USDT"},
			{"symbol": "ETHUSDT", "status": "TRADING", "baseAsset": "ETH", "quoteAsset": "USDT"},
		},
	})
}

// handleKlines 模拟 GET /api/v3/klines：固定返回三根历史 K 线，不看查询
// 参数——run.sh 传给 price backfill 的 -from/-to 经过挑选，翻页在这三根之后
// 即终止（下一页起点 backfillOpenTimeBase+3*klineIntervalMs 会 >= -to 对应的
// endTime，见 internal/price/exchange/binance/rest.go Klines 的终止判断），
// 不会因为这里忽略查询参数而死循环再请求下一页。行字段顺序取自
// rest_test.go 的官方样例（开盘时间/开/高/低/收/量/收盘时间/成交额/成交笔数/
// 主动买入基础币量/主动买入计价币量/忽略字段）。
func handleKlines(w http.ResponseWriter, r *http.Request) {
	rows := [][]any{
		klineRow(backfillOpenTimeBase, "40000.00", "40100.00", "39900.00", "40050.00", "5.0", "200000.00"),
		klineRow(backfillOpenTimeBase+klineIntervalMs, "40050.00", "40150.00", "39950.00", "40100.00", "5.1", "204255.00"),
		klineRow(backfillOpenTimeBase+2*klineIntervalMs, "40100.00", "40200.00", "40000.00", "40150.00", "5.2", "208780.00"),
	}
	writeJSON(w, rows)
}

func klineRow(openTime int64, open, high, low, close, volume, quoteVolume string) []any {
	return []any{
		openTime, open, high, low, close, volume,
		openTime + klineIntervalMs - 1, quoteVolume, 100, "0", "0", "0",
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
