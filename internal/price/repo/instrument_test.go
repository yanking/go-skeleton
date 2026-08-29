package repo

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/yanking/go-skeleton/internal/price/model"
)

// TestMarkDelistedExcept_UpdatesStatusNotDeletesRows 回归锚点：交易所本轮不再
// 返回的标的必须走 UPDATE 标记为已下架，绝不能走 DELETE 删行——历史 K 线仍按
// (exchange, market, native_symbol) 引用着这些标的（见 service.ImportInstruments
// 的注释），删行会让已落库的历史数据失去可查的标的元信息，是数据损坏。这条
// 不变量此前只靠 service 层的接口收窄间接守住（InstrumentRepo 只声明了
// UpsertAll/MarkDelistedExcept，没有 Delete 方法，service 物理上调不到删除），
// repo 层本身缺一条测试直接钉住生成的 SQL 形状，这里补上。
func TestMarkDelistedExcept_UpdatesStatusNotDeletesRows(t *testing.T) {
	lg := &capturingLogger{}
	db := newDryRun(t, lg)

	if err := NewInstrument(db).MarkDelistedExcept(context.Background(), "binance", "spot", []string{"BTCUSDT"}); err != nil {
		t.Fatal(err)
	}
	t.Log(lg.sql)

	upper := strings.ToUpper(strings.TrimSpace(lg.sql))
	if !strings.HasPrefix(upper, "UPDATE") {
		t.Fatalf("MarkDelistedExcept 生成的 SQL 不是 UPDATE 语句:\n%s", lg.sql)
	}
	if strings.Contains(upper, "DELETE") {
		t.Fatalf("MarkDelistedExcept 生成的 SQL 包含 DELETE，标下架不能删行:\n%s", lg.sql)
	}

	wantSet := fmt.Sprintf("`status`=%d", model.InstrumentStatusDelisted)
	if !strings.Contains(lg.sql, wantSet) {
		t.Errorf("SQL 未把 status 置为 InstrumentStatusDelisted(%d):\n%s", model.InstrumentStatusDelisted, lg.sql)
	}
}
