package okx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
// state=="live" 且计价币在 ImportQuotes 内的交易对；报文形状取自官方文档
// public/instruments 样例（instId/baseCcy/quoteCcy/state 四个用得到的字段）。
// 同时验证 OKX 的 Symbol 与 NativeSymbol 相等（instId 本身即「基础币-计价币」
// 形态，不需要像 binance 那样另行拼接）。
func TestInstruments_FiltersByQuoteAndStatus(t *testing.T) {
	body := `{"code":"0","msg":"","data":[
		{"instId":"BTC-USDT","baseCcy":"BTC","quoteCcy":"USDT","state":"live"},
		{"instId":"ETH-BTC","baseCcy":"ETH","quoteCcy":"BTC","state":"live"},
		{"instId":"BAZ-USDT","baseCcy":"BAZ","quoteCcy":"USDT","state":"suspend"},
		{"instId":"XRP-EUR","baseCcy":"XRP","quoteCcy":"EUR","state":"live"}
	]}`
	url := newTestServer(t, body)
	o := New(Config{RESTURL: url, ImportQuotes: []string{"USDT"}, HTTP: httpc.New(httpc.Config{})})

	got, err := o.Instruments(context.Background(), "spot")
	if err != nil {
		t.Fatalf("Instruments: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1(只有 BTC-USDT 同时满足 state=live 与计价币 USDT)", len(got))
	}
	want := exchange.Instrument{
		Market: "spot", NativeSymbol: "BTC-USDT", Symbol: "BTC-USDT", Base: "BTC", Quote: "USDT", Trading: true,
	}
	if got[0] != want {
		t.Errorf("got[0] = %+v, want %+v", got[0], want)
	}
}

// TestInstruments_ErrorsOnBusinessErrorCode 验证响应体 code != "0" 时报错，
// 即使 HTTP 状态码是 200——OKX 出错时 HTTP 状态码通常仍是 200，真正的错误
// 信号在响应体的 code 字段（已核实，见 doc.go：Error Codes 表格列出的错误码
// 对应的 HTTP status code 均为 200），不检查这层会把业务错误响应误当空数据
// 处理，静默漏采集。
func TestInstruments_ErrorsOnBusinessErrorCode(t *testing.T) {
	body := `{"code":"50001","msg":"Service temporarily unavailable","data":[]}`
	url := newTestServer(t, body)
	o := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})

	_, err := o.Instruments(context.Background(), "spot")
	if err == nil {
		t.Fatal("响应体 code != \"0\" 时应报错，实际未报错")
	}
}

// TestKlines_ReversesDescendingResponseToAscending 验证 OKX 历史 K 线端点
// 返回的倒序（最新在前，见 doc.go 核实结论：官方文档未直接写排序方向，但
// 结合 before 参数语义与生态验证——ccxt 源码里捕获的真实响应样例 ts 从大到
// 小排列——确认是倒序）被本包反转成正序：exchange.Exchange.Klines 的返回
// 契约要求正序，编排层看不到这个差异。
func TestKlines_ReversesDescendingResponseToAscending(t *testing.T) {
	body := `{"code":"0","msg":"","data":[
		["1499040120000","3","3","3","3","3","3","3","1"],
		["1499040060000","2","2","2","2","2","2","2","1"],
		["1499040000000","1","1","1","1","1","1","1","1"]
	]}`
	url := newTestServer(t, body)
	o := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"}

	got, _, err := o.Klines(context.Background(), s, 1499040000000, 1499040120000)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	for i, want := range []int64{1499040000000, 1499040060000, 1499040120000} {
		if got[i].OpenTime != want {
			t.Errorf("got[%d].OpenTime = %d, want %d(应已反转成正序)", i, got[i].OpenTime, want)
		}
	}
}

// TestKlines_FiltersUnconfirmedRow 验证 confirm != "1" 的行被过滤，不产出
// K 线——理由与 WS 一侧 decodeKline 一致（未收线的值还会变）。
func TestKlines_FiltersUnconfirmedRow(t *testing.T) {
	body := `{"code":"0","msg":"","data":[
		["1499040060000","2","2","2","2","2","2","2","0"],
		["1499040000000","1","1","1","1","1","1","1","1"]
	]}`
	url := newTestServer(t, body)
	o := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"}

	got, _, err := o.Klines(context.Background(), s, 1499040000000, 1499040120000)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if len(got) != 1 || got[0].OpenTime != 1499040000000 {
		t.Errorf("got = %+v, want 只保留 confirm=1 的一根", got)
	}
}

// TestKlines_ReturnsZeroNextWhenReachedEnd 验证翻页到达/越过 end 时返回 0，
// 供调用方判断补洞已收尾。
func TestKlines_ReturnsZeroNextWhenReachedEnd(t *testing.T) {
	body := `{"code":"0","msg":"","data":[["1499040000000","1","1","1","1","1","1","1","1"]]}`
	url := newTestServer(t, body)
	o := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"}

	// end 恰好等于「最后一根开盘时间 + 一个周期」：下一页起点会等于 end，
	// 应视为已到终点，返回 0。
	_, next, err := o.Klines(context.Background(), s, 1499040000000, 1499040000000+60000)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if next != 0 {
		t.Errorf("next = %d, want 0(已到 end)", next)
	}
}

// TestKlines_EmptyRangeSkipsRequest 验证 start > end 时直接返回，不发 HTTP
// 请求——RESTURL 指向一个必炸的地址，若真的发了请求测试会因网络错误而失败。
func TestKlines_EmptyRangeSkipsRequest(t *testing.T) {
	o := New(Config{RESTURL: "http://127.0.0.1:0", HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"}

	got, next, err := o.Klines(context.Background(), s, 100, 0)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if got != nil || next != 0 {
		t.Errorf("got, next = %+v, %d, want nil, 0", got, next)
	}
}

// TestKlines_EmptyResponseStopsPaging 验证服务端对合法区间（start <= end）
// 真的返回空数组时，Klines 干净终止翻页（返回 nil, 0, nil），不会死循环去
// 拿同一段区间。
func TestKlines_EmptyResponseStopsPaging(t *testing.T) {
	url := newTestServer(t, `{"code":"0","msg":"","data":[]}`)
	o := New(Config{RESTURL: url, HTTP: httpc.New(httpc.Config{})})
	s := exchange.Sub{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"}

	got, next, err := o.Klines(context.Background(), s, 1499040000000, 1499040000000+600000)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if got != nil || next != 0 {
		t.Errorf("got, next = %+v, %d, want nil, 0", got, next)
	}
}
