package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// maxKlinesLimit 是 GET /api/v3/klines 的 limit 上限（已核实，见 doc.go）；
// 每页尽量拉满，减少翻页往返次数。
const maxKlinesLimit = 1000

// Instruments 拉 Binance 现货全量交易对（GET /api/v3/exchangeInfo），
// 按 Config.ImportQuotes 与 status == "TRADING" 过滤——非目标计价币、已下架/
// 停牌的交易对不返回，不是「返回但标记不可交易」。
func (b *Binance) Instruments(ctx context.Context, market string) ([]exchange.Instrument, error) {
	code, body, err := b.cfg.HTTP.Get(ctx, b.cfg.RESTURL+"/api/v3/exchangeInfo", nil, 0)
	if err != nil {
		return nil, fmt.Errorf("binance: 请求 exchangeInfo: %w", err)
	}
	if code != 200 {
		return nil, fmt.Errorf("binance: exchangeInfo 返回非 200: %d %s", code, body)
	}

	var resp struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("binance: 解析 exchangeInfo 响应: %w", err)
	}

	quotes := make(map[string]bool, len(b.cfg.ImportQuotes))
	for _, q := range b.cfg.ImportQuotes {
		quotes[q] = true
	}

	var out []exchange.Instrument
	for _, s := range resp.Symbols {
		if s.Status != "TRADING" || !quotes[s.QuoteAsset] {
			continue
		}
		out = append(out, exchange.Instrument{
			Market:       market,
			NativeSymbol: s.Symbol,
			Symbol:       s.BaseAsset + "-" + s.QuoteAsset,
			Base:         s.BaseAsset,
			Quote:        s.QuoteAsset,
			Trading:      true,
		})
	}
	return out, nil
}

// Klines 拉一段历史 K 线（GET /api/v3/klines），[start, end] 按闭区间处理
// （见 doc.go 对 startTime/endTime 语义的核实结论）。返回已按官方文档确认的
// 时间正序；下一页起点取最后一根的开盘时间 + 一个周期，避免与本页最后一条
// 重复；翻页到达或越过 end 时返回 0，调用方据此判断补洞收尾。
func (b *Binance) Klines(ctx context.Context, s exchange.Sub, start, end int64) ([]exchange.Kline, int64, error) {
	if start > end {
		return nil, 0, nil
	}

	q := url.Values{}
	q.Set("symbol", s.NativeSymbol)
	q.Set("interval", s.Interval)
	q.Set("startTime", strconv.FormatInt(start, 10))
	q.Set("endTime", strconv.FormatInt(end, 10))
	q.Set("limit", strconv.Itoa(maxKlinesLimit))

	code, body, err := b.cfg.HTTP.Get(ctx, b.cfg.RESTURL+"/api/v3/klines?"+q.Encode(), nil, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("binance: 请求 klines: %w", err)
	}
	if code != 200 {
		return nil, 0, fmt.Errorf("binance: klines 返回非 200: %d %s", code, body)
	}

	var rows [][]json.RawMessage
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		return nil, 0, fmt.Errorf("binance: 解析 klines 响应: %w", err)
	}

	// now 只取一次：本函数只处理一批响应，同一批内多行用同一个判定基准，
	// 不必每行各调一次 time.Now()。
	now := time.Now().UnixMilli()
	klines := make([]exchange.Kline, 0, len(rows))
	for _, row := range rows {
		k, closeTime, err := parseKlineRow(s, row)
		if err != nil {
			return nil, 0, err
		}
		if closeTime > now {
			// 尚未收线（该端点没有 x/confirm 一类标志位，只能靠 closeTime
			// 是否已过判断）：补洞的止点 end 恒为调用时刻（见 backfill.go
			// Backfill 对 end 的注释），查询区间必然覆盖当前进行中的那根
			// K 线，[start, end] 的最后一行就可能是它——不丢弃的话会把一根
			// 还会变的行当成定案数据落库，source=2 与已收线的历史行外观
			// 完全一样，没有任何字段能分辨（防御查询区间恰好触达当前进行中
			// 周期的边界情况，与 okx 包 rest.go 对 Confirmed 的过滤同一个
			// 理由）。丢掉这一行后 next 会指向它的开盘时间，下一页请求会
			// 再次拿到同一根、再次被这里过滤成空数组，由下面 len(klines)==0
			// 分支干净终止——多一次请求可以接受，不在这里另做 next=0 的
			// 特殊处理。
			continue
		}
		klines = append(klines, k)
	}
	if len(klines) == 0 {
		// 假设：Binance 对 [start, end] 做的是整段区间过滤（区间内任何位置有
		// 数据都会被查到），不是「起点必须落在某根 K 线上」；空数组即代表这
		// 段区间确实没有数据，可以安全终止翻页，不会因为 start 恰好落在两根
		// K 线之间就漏掉 start 之后仍存在的数据。此假设依据推断（"With
		// startTime, returns oldest items from startTime up to the limit."
		// 一类的翻页规则，见 doc.go 对 klines 语义的核实结论），未见官方文档
		// 就"空数组场景"给出明确保证；如果这个假设不成立，补洞会在没有数据
		// 的区间里悄悄提前收尾——留意后续如果发现补洞有遗漏，先复查这里。
		return nil, 0, nil
	}

	next, err := nextOpenTime(klines[len(klines)-1].OpenTime, s.Interval)
	if err != nil {
		return nil, 0, err
	}
	if next >= end {
		// 已到达或越过 end：按闭区间语义，下一页起点等于 end 时对应的那根
		// K 线本页已经拿到（本页 endTime 传的就是 end，服务端已把它囊括在内），
		// 无需再翻一页去问一个必然拿不到新数据的区间。
		next = 0
	}
	return klines, next, nil
}

// parseKlineRow 把 klines 响应里的一行（异构数组，字段顺序见官方文档
// Kline/Candlestick data 一节）翻成中立 Kline，并额外带回收盘时间（索引 6）
// 供调用方判断是否已收线——该端点没有 x/confirm 一类标志位，只能靠「收盘
// 时间是否已过」推断，见 Klines 对返回值的过滤。中立 Kline 本身仍只取 6 个
// 字段（开盘时间、开高低收、成交量、成交额），成交笔数等字段官方文档里
// 存在但中立类型未定义，不取。
func parseKlineRow(s exchange.Sub, row []json.RawMessage) (exchange.Kline, int64, error) {
	// 索引含义：0 开盘时间 1 开 2 高 3 低 4 收 5 成交量(基础币) 6 收盘时间
	// 7 成交额(计价币) 8 成交笔数 9 主动买入基础币量 10 主动买入计价币量 11 忽略。
	const minFields = 8
	if len(row) < minFields {
		return exchange.Kline{}, 0, fmt.Errorf("binance: klines 响应行字段不足: 需要至少 %d 个，got %d", minFields, len(row))
	}

	var openTime int64
	if err := json.Unmarshal(row[0], &openTime); err != nil {
		return exchange.Kline{}, 0, fmt.Errorf("binance: 解析 kline 开盘时间: %w", err)
	}
	strs := make([]string, 5)
	for i := range strs {
		if err := json.Unmarshal(row[i+1], &strs[i]); err != nil {
			return exchange.Kline{}, 0, fmt.Errorf("binance: 解析 kline 第 %d 个字段: %w", i+1, err)
		}
	}
	var closeTime int64
	if err := json.Unmarshal(row[6], &closeTime); err != nil {
		return exchange.Kline{}, 0, fmt.Errorf("binance: 解析 kline 收盘时间: %w", err)
	}
	var quoteVolume string
	if err := json.Unmarshal(row[7], &quoteVolume); err != nil {
		return exchange.Kline{}, 0, fmt.Errorf("binance: 解析 kline 成交额: %w", err)
	}

	return exchange.Kline{
		Market:       s.Market,
		NativeSymbol: s.NativeSymbol,
		Interval:     s.Interval,
		OpenTime:     openTime,
		Open:         strs[0],
		High:         strs[1],
		Low:          strs[2],
		Close:        strs[3],
		Volume:       strs[4],
		QuoteVolume:  quoteVolume,
	}, closeTime, nil
}

// nextOpenTime 返回 openTimeMs 所在周期之后下一根 K 线的开盘时间（毫秒）；
// 用官方文档列出的全部周期单位（s/m/h/d/w/M，见 doc.go），月按日历月累加
// （避免固定 30 天造成的漂移），其余按固定时长累加。
func nextOpenTime(openTimeMs int64, interval string) (int64, error) {
	n, unit, err := splitInterval(interval)
	if err != nil {
		return 0, err
	}
	t := time.UnixMilli(openTimeMs).UTC()
	switch unit {
	case "s":
		return t.Add(time.Duration(n) * time.Second).UnixMilli(), nil
	case "m":
		return t.Add(time.Duration(n) * time.Minute).UnixMilli(), nil
	case "h":
		return t.Add(time.Duration(n) * time.Hour).UnixMilli(), nil
	case "d":
		return t.AddDate(0, 0, n).UnixMilli(), nil
	case "w":
		return t.AddDate(0, 0, 7*n).UnixMilli(), nil
	case "M":
		return t.AddDate(0, n, 0).UnixMilli(), nil
	default:
		return 0, fmt.Errorf("binance: 不支持的 K 线周期 %q", interval)
	}
}

// splitInterval 拆出周期数字与单位；单位大小写敏感（m=分钟、M=月，见 doc.go
// 引用的官方周期表），故不能整体转大小写后再比较。
func splitInterval(interval string) (int, string, error) {
	if len(interval) < 2 {
		return 0, "", fmt.Errorf("binance: 无效的 K 线周期 %q", interval)
	}
	unit := interval[len(interval)-1:]
	n, err := strconv.Atoi(interval[:len(interval)-1])
	if err != nil || n <= 0 {
		return 0, "", fmt.Errorf("binance: 无效的 K 线周期 %q", interval)
	}
	return n, unit, nil
}
