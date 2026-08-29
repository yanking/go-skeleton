package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// maxKlinesLimit 是 GET /api/v5/market/history-candles 的 limit 上限
// （已核实，见 doc.go），每页尽量拉满，减少翻页往返次数。
const maxKlinesLimit = 300

// restEnvelope 是 OKX REST 响应的统一外层包裹。OKX 出错时 HTTP 状态码通常
// 仍是 200，真正的错误信号在 Code 字段（非 "0" 即失败），这与 Binance 用
// HTTP 状态码传递错误不同（已核实，见 doc.go：Error Codes 表格列出的错误码
// 对应的 HTTP status code 均为 200），不检查这个字段会把业务错误响应误当
// 空数据处理。
type restEnvelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// decodeEnvelope 统一处理 REST 响应的两层错误：HTTP 状态码非 200（网络/网关
// 层）与响应体 code 非 "0"（OKX 业务层），两者都要拦下，不能只查其中一层。
func decodeEnvelope(httpCode int, body string) (json.RawMessage, error) {
	if httpCode != 200 {
		return nil, fmt.Errorf("响应非 200: %d %s", httpCode, body)
	}
	var env restEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return nil, fmt.Errorf("解析响应包裹: %w", err)
	}
	if env.Code != "0" {
		return nil, fmt.Errorf("业务错误 code=%s msg=%s", env.Code, env.Msg)
	}
	return env.Data, nil
}

// Instruments 拉 OKX 现货全量交易对（GET /api/v5/public/instruments?
// instType=SPOT），按 Config.ImportQuotes 与 state == "live" 过滤——非目标
// 计价币、已下架/停牌/测试的交易对不返回，不是「返回但标记不可交易」（state
// 的全部取值见 doc.go）。OKX 的 instId 本身就是「基础币-计价币」形态（如
// BTC-USDT），故 Symbol 与 NativeSymbol 相等，不需要像 binance 那样另行拼接。
func (o *OKX) Instruments(ctx context.Context, market string) ([]exchange.Instrument, error) {
	code, body, err := o.cfg.HTTP.Get(ctx, o.cfg.RESTURL+"/api/v5/public/instruments?instType=SPOT", nil, 0)
	if err != nil {
		return nil, fmt.Errorf("okx: 请求 instruments: %w", err)
	}
	data, err := decodeEnvelope(code, body)
	if err != nil {
		return nil, fmt.Errorf("okx: instruments: %w", err)
	}

	var rows []struct {
		InstID   string `json:"instId"`
		BaseCcy  string `json:"baseCcy"`
		QuoteCcy string `json:"quoteCcy"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("okx: 解析 instruments 数据: %w", err)
	}

	quotes := make(map[string]bool, len(o.cfg.ImportQuotes))
	for _, q := range o.cfg.ImportQuotes {
		quotes[q] = true
	}

	var out []exchange.Instrument
	for _, r := range rows {
		if r.State != "live" || !quotes[r.QuoteCcy] {
			continue
		}
		out = append(out, exchange.Instrument{
			Market:       market,
			NativeSymbol: r.InstID,
			Symbol:       r.InstID, // OKX instId 本身即「基础币-计价币」，无需另拼
			Base:         r.BaseCcy,
			Quote:        r.QuoteCcy,
			Trading:      true,
		})
	}
	return out, nil
}

// Klines 拉一段历史 K 线（GET /api/v5/market/history-candles），[start, end]
// 按闭区间处理。OKX 的 after/before 都是排他边界（已核实，见 doc.go），用
// before=start-1、after=窗口上界+1 模拟闭区间下界与「本次最多拉 limit 根」
// 的上界（做法与生态内 ccxt 用 since-1 达到同等效果一致，交叉验证过，见
// doc.go）；响应本身是倒序（最新在前），本包内部反转成正序后返回——
// exchange.Exchange.Klines 的返回契约要求正序，编排层看不到这个差异。
//
// 只使用 history-candles 一个端点：/api/v5/market/candles（近期）最多只
// 保留最新 1,440 根（已核实，见 doc.go），对 30 天回溯窗口（collector.
// max_backfill_window）完全不够；history-candles 覆盖「近几年」历史，单一
// 端点已能满足全部补洞场景，不必再按新旧引入端点切换逻辑，保持简单。
func (o *OKX) Klines(ctx context.Context, s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
	if start > end {
		return nil, 0, nil
	}

	windowEnd, err := addPeriods(start, s.Interval, maxKlinesLimit)
	if err != nil {
		return nil, 0, err
	}
	windowEnd--
	if windowEnd > end {
		windowEnd = end
	}

	q := url.Values{}
	q.Set("instId", s.NativeSymbol)
	q.Set("bar", s.Interval)
	q.Set("before", strconv.FormatInt(start-1, 10))
	q.Set("after", strconv.FormatInt(windowEnd+1, 10))
	q.Set("limit", strconv.Itoa(maxKlinesLimit))

	code, body, err := o.cfg.HTTP.Get(ctx, o.cfg.RESTURL+"/api/v5/market/history-candles?"+q.Encode(), nil, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("okx: 请求 history-candles: %w", err)
	}
	data, err := decodeEnvelope(code, body)
	if err != nil {
		return nil, 0, fmt.Errorf("okx: history-candles: %w", err)
	}

	var rows [][]string
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, 0, fmt.Errorf("okx: 解析 history-candles 数据: %w", err)
	}

	klines := make([]exchange.Kline, 0, len(rows))
	for _, row := range rows {
		c, err := parseCandleArray(row)
		if err != nil {
			return nil, 0, err
		}
		if !c.Confirmed {
			// history-candles 理论上只返回已完结的历史数据，这里仍按 confirm
			// 过滤，是为了和 WS 一侧 decodeKline 的语义保持一致、防御查询区间
			// 恰好触达当前进行中周期的边界情况（见 doc.go）。
			continue
		}
		klines = append(klines, exchange.Kline{
			Market:       s.Market,
			NativeSymbol: s.NativeSymbol,
			Interval:     s.Interval,
			OpenTime:     c.OpenTime,
			Open:         c.Open,
			High:         c.High,
			Low:          c.Low,
			Close:        c.Close,
			Volume:       c.Volume,
			QuoteVolume:  c.QuoteVolume,
		})
	}
	if len(klines) == 0 {
		// 走到这里有两种成因，处理方式相同，都当「这段区间没有可用数据，
		// 安全终止翻页」：①OKX 对 [before, after] 做整段区间过滤后确实返回
		// 空数组（假设：区间内没有数据即返回空数组，不是"起点必须落在某根
		// K 线上"才算数——与 binance 包 rest.go 对空数组场景的假设一致，
		// 未见官方文档就该场景给出明确保证）；②响应本身非空，但整页所有行
		// 的 confirm 都不是 "1"，被上面的过滤全部剔除——history-candles
		// 按文档只应返回已完结数据，这种情况理论上不该出现，发生概率极低，
		// 这里仍按同样方式收尾而不是报错，是一个已知的假设边界；后续若发现
		// 补洞有异常遗漏，先复查是不是踩到了这种情况。
		return nil, 0, nil
	}

	// OKX 返回倒序（最新在前，见 doc.go 核实结论），反转成正序。
	for i, j := 0, len(klines)-1; i < j; i, j = i+1, j-1 {
		klines[i], klines[j] = klines[j], klines[i]
	}

	next, err := addPeriods(klines[len(klines)-1].OpenTime, s.Interval, 1)
	if err != nil {
		return nil, 0, err
	}
	if next >= end {
		// 已到达或越过 end：按闭区间语义，本页已经把 end 对应的那根 K 线
		// 囊括在内（本页 after 传的就是 windowEnd+1，windowEnd 已按 end
		// 封顶），无需再翻一页去问一个必然拿不到新数据的区间。
		next = 0
	}
	return klines, next, nil
}

// addPeriods 返回 openTimeMs 之后第 count 个周期的开盘时间（毫秒）；月按
// 日历月累加（避免固定 30/31 天造成漂移，理由同 binance 包 rest.go 的
// nextOpenTime），其余按固定时长累加。utc 后缀（如 6Hutc）只影响开盘对齐的
// 时区基准，不影响周期本身的时长，换算前先去掉。
func addPeriods(openTimeMs int64, bar string, count int) (int64, error) {
	n, unit, err := splitBar(bar)
	if err != nil {
		return 0, err
	}
	t := time.UnixMilli(openTimeMs).UTC()
	switch unit {
	case "s":
		return t.Add(time.Duration(n*count) * time.Second).UnixMilli(), nil
	case "m":
		return t.Add(time.Duration(n*count) * time.Minute).UnixMilli(), nil
	case "H":
		return t.Add(time.Duration(n*count) * time.Hour).UnixMilli(), nil
	case "D":
		return t.AddDate(0, 0, n*count).UnixMilli(), nil
	case "W":
		return t.AddDate(0, 0, 7*n*count).UnixMilli(), nil
	case "M":
		return t.AddDate(0, n*count, 0).UnixMilli(), nil
	default:
		return 0, fmt.Errorf("okx: 不支持的 K 线周期单位 %q", unit)
	}
}

// splitBar 拆出 OKX bar 拼写里的周期数字与单位，先去掉可选的 utc 后缀（如
// 6Hutc→6H）；单位大小写敏感：s=秒、m=分钟、H=小时、D=天、W=周、M=月（已
// 核实，见 doc.go），不能整体转大小写后再比较。
func splitBar(bar string) (int, string, error) {
	b := strings.TrimSuffix(bar, "utc")
	if len(b) < 2 {
		return 0, "", fmt.Errorf("okx: 无效的 K 线周期 %q", bar)
	}
	unit := b[len(b)-1:]
	n, err := strconv.Atoi(b[:len(b)-1])
	if err != nil || n <= 0 {
		return 0, "", fmt.Errorf("okx: 无效的 K 线周期 %q", bar)
	}
	return n, unit, nil
}
