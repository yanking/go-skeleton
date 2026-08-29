package binance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/pkg/httpc"
)

// newTestServer 起一个固定返回给定 JSON 的测试服务器，返回可传给 Config.RESTURL 的地址。
func newTestServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestInstruments_FiltersByQuoteAndStatus 验证 Instruments 只返回
// status=="TRADING" 且计价币在 ImportQuotes 内的交易对；报文形状取自
// 官方文档 exchangeInfo 样例（symbol/status/baseAsset/quoteAsset 四个用得到的字段）。
func TestInstruments_FiltersByQuoteAndStatus(t *testing.T) {
	body := `{
		"symbols": [
			{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT"},
			{"symbol":"ETHBTC","status":"TRADING","baseAsset":"ETH","quoteAsset":"BTC"},
			{"symbol":"BAZUSDT","status":"BREAK","baseAsset":"BAZ","quoteAsset":"USDT"},
			{"symbol":"XRPEUR","status":"TRADING","baseAsset":"XRP","quoteAsset":"EUR"}
		]
	}`
	url := newTestServer(t, body)
	b := New(Config{RESTURL: url, ImportQuotes: []string{"USDT"}, HTTP: httpc.New(httpc.Config{})})

	got, err := b.Instruments(context.Background(), "spot")
	if err != nil {
		t.Fatalf("Instruments: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1(只有 BTCUSDT 同时满足 TRADING 与计价币 USDT)", len(got))
	}
	want := exchange.Instrument{
		Market: "spot", NativeSymbol: "BTCUSDT", Symbol: "BTC-USDT", Base: "BTC", Quote: "USDT", Trading: true,
	}
	if got[0] != want {
		t.Errorf("got[0] = %+v, want %+v", got[0], want)
	}
}

// TestKlines_ParsesRowAndComputesNextPage 验证 Klines 正确解析响应行
// （报文取自官方文档 Kline/Candlestick data 样例），且下一页起点是
// 最后一根开盘时间 + 一个周期。
func TestKlines_ParsesRowAndComputesNextPage(t *testing.T) {
	body := `[
		[1499040000000,"0.01634790","0.80000000","0.01575800","0.01577100","148976.11427815",1499644799999,"2434.19055334",308,"1756.87402397","28.46694368","0"]
	]`
	url := newTestServer(t, body)
	b := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BNBBTC", StreamType: exchange.StreamKline, Interval: "1m"}

	got, next, err := b.Klines(context.Background(), s, 0, 1499040000000+3*60000)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	want := exchange.Kline{
		Market: "spot", NativeSymbol: "BNBBTC", Interval: "1m",
		OpenTime: 1499040000000, Open: "0.01634790", High: "0.80000000", Low: "0.01575800",
		Close: "0.01577100", Volume: "148976.11427815", QuoteVolume: "2434.19055334",
	}
	if got[0] != want {
		t.Errorf("got[0] = %+v, want %+v", got[0], want)
	}
	wantNext := int64(1499040000000 + 60000) // 开盘时间 + 一个 1m 周期
	if next != wantNext {
		t.Errorf("next = %d, want %d", next, wantNext)
	}
}

// TestKlines_ReturnsZeroNextWhenReachedEnd 验证翻页到达/越过 end 时返回 0，
// 供调用方判断补洞已收尾（而不是继续拿 next 当起点再请求一轮）。
func TestKlines_ReturnsZeroNextWhenReachedEnd(t *testing.T) {
	body := `[
		[1499040000000,"1","1","1","1","1",1499040059999,"1",1,"1","1","0"]
	]`
	url := newTestServer(t, body)
	b := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BNBBTC", StreamType: exchange.StreamKline, Interval: "1m"}

	// end 恰好等于「最后一根开盘时间 + 一个周期」：下一页起点会等于 end，
	// 应视为已到终点，返回 0。
	_, next, err := b.Klines(context.Background(), s, 0, 1499040000000+60000)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if next != 0 {
		t.Errorf("next = %d, want 0(已到 end)", next)
	}
}

// TestKlines_EmptyRangeSkipsRequest 验证 start > end 时直接返回，不发 HTTP 请求
// ——RESTURL 指向一个必炸的地址，若真的发了请求测试会因网络错误而失败。
func TestKlines_EmptyRangeSkipsRequest(t *testing.T) {
	b := New(Config{RESTURL: "http://127.0.0.1:0", HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BNBBTC", StreamType: exchange.StreamKline, Interval: "1m"}

	got, next, err := b.Klines(context.Background(), s, 100, 0)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if got != nil || next != 0 {
		t.Errorf("got, next = %+v, %d, want nil, 0", got, next)
	}
}

// TestKlines_FiltersOutUnclosedTrailingKline 锚定必修 1：/api/v3/klines 没有
// x/confirm 一类标志位，endTime 覆盖当前时刻时响应最后一根可能是尚未收线的
// 那根——补洞的止点恒为调用时刻（backfill.go Backfill 的 end），查询区间
// 必然覆盖它。用两根已收线（closeTime 在过去）+ 一根收盘时间在未来的行
// 构造响应，断言未收线那根不出现在结果里，也不会被当成正常一页导致后续
// 分页起点算错。
func TestKlines_FiltersOutUnclosedTrailingKline(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	body := `[
		[1000,"1","1","1","1","1",59999,"1",1,"1","1","0"],
		[60000,"2","2","2","2","2",119999,"2",1,"1","1","0"],
		[120000,"3","3","3","3","3",` + strconv.FormatInt(future, 10) + `,"3",1,"1","1","0"]
	]`
	url := newTestServer(t, body)
	b := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BNBBTC", StreamType: exchange.StreamKline, Interval: "1m"}

	got, next, err := b.Klines(context.Background(), s, 0, future)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2(未收线的第三根应被过滤)", len(got))
	}
	for _, k := range got {
		if k.OpenTime == 120000 {
			t.Errorf("未收线的 K 线(OpenTime=120000)不应出现在结果里")
		}
	}
	wantNext := int64(60000 + 60000) // 续接点取最后一根已收线(OpenTime=60000)+一个周期
	if next != wantNext {
		t.Errorf("next = %d, want %d(应从最后一根已收线续接，不是被过滤掉的那根)", next, wantNext)
	}
}

// TestKlines_EmptyResponseStopsPaging 验证服务端对合法区间（start <= end）
// 真的返回空数组时，Klines 干净终止翻页（返回 nil, 0, nil），不会死循环去
// 拿同一段区间——这条路径与 TestKlines_EmptyRangeSkipsRequest 不同：那条测的
// 是 start > end 时根本不发请求，这条测的是发了请求、服务端确实回了 []。
func TestKlines_EmptyResponseStopsPaging(t *testing.T) {
	url := newTestServer(t, `[]`)
	b := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BNBBTC", StreamType: exchange.StreamKline, Interval: "1m"}

	got, next, err := b.Klines(context.Background(), s, 0, 1499040000000)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if got != nil || next != 0 {
		t.Errorf("got, next = %+v, %d, want nil, 0", got, next)
	}
}
