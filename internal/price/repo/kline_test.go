package repo

import (
	"context"
	"strings"
	"testing"

	"github.com/yanking/go-skeleton/internal/price/model"
)

// TestKlineUpsert_ConflictTargetIsPrimaryKey 回归锚点：同一根 K 线会从 ws 收线与
// REST 补洞两条路径到达，重复是常态；Upsert 的冲突目标必须精确对齐主键五列
// (exchange, market, native_symbol, interval, open_time)，否则要么产生重复行，
// 要么覆盖到错误的行——且这类问题通常要很久以后才会被发现。
//
// 只断言子串（如 "exchange"、"native_symbol"）不够：普通 INSERT 语句本身就带这些
// 列名，测试守不住"这些列在 ON CONFLICT 目标里"这件事。因此这里精确提取
// ON CONFLICT (...) 括号内的列列表，逐字比对，确保既不多也不少、顺序也对齐
// Upsert 里声明的主键五列。
func TestKlineUpsert_ConflictTargetIsPrimaryKey(t *testing.T) {
	lg := &capturingLogger{}
	db := newDryRun(t, lg)
	r := NewKline(db)

	_ = r.Upsert(context.Background(), []model.Kline{{
		Exchange: "binance", Market: "spot", NativeSymbol: "BTCUSDT",
		Interval: "1m", OpenTime: 1700000000000, Open: "1", High: "2", Low: "1", Close: "2",
		Volume: "10", QuoteVolume: "20", Source: model.KlineSourceStream,
	}})
	t.Log(lg.sql)

	const marker = "ON CONFLICT ("
	start := strings.Index(lg.sql, marker)
	if start == -1 {
		t.Fatalf("生成的 SQL 缺少 ON CONFLICT 子句:\n%s", lg.sql)
	}
	start += len(marker)
	end := strings.Index(lg.sql[start:], ")")
	if end == -1 {
		t.Fatalf("ON CONFLICT 子句未闭合:\n%s", lg.sql)
	}
	conflictCols := lg.sql[start : start+end]

	const wantCols = "`exchange`,`market`,`native_symbol`,`interval`,`open_time`"
	if conflictCols != wantCols {
		t.Errorf("ON CONFLICT 冲突目标列不符:\n got  = %s\n want = %s", conflictCols, wantCols)
	}
	if !strings.Contains(lg.sql, "DO UPDATE") {
		t.Errorf("生成的 SQL 缺少 DO UPDATE:\n%s", lg.sql)
	}
}
