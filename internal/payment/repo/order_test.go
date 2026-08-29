package repo

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/utils/tests"
)

// capturingLogger 手写 mock：只记录最近一次 GORM 生成的（已插值）SQL，配合 DryRun
// 用于断言查询条件的形状，不落地任何真实数据库连接、不引入新依赖
// （tests.DummyDialector 是 gorm.io/gorm 自带的测试替身）。
type capturingLogger struct {
	sql string
}

func (l *capturingLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return l }
func (l *capturingLogger) Info(context.Context, string, ...interface{})     {}
func (l *capturingLogger) Warn(context.Context, string, ...interface{})     {}
func (l *capturingLogger) Error(context.Context, string, ...interface{})    {}
func (l *capturingLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	l.sql, _ = fc()
}

// newDryRunOrder 构造只生成 SQL、不执行、不连接真实数据库的 Order 仓储，用于锚定
// 查询条件的形状（如是否带上 merchant_id）。
func newDryRunOrder(t *testing.T) (*Order, *capturingLogger) {
	t.Helper()
	logger := &capturingLogger{}
	db, err := gorm.Open(tests.DummyDialector{}, &gorm.Config{Logger: logger, DryRun: true})
	if err != nil {
		t.Fatalf("打开 DryRun 连接: %v", err)
	}
	return NewOrder(db), logger
}

// TestOrder_FindForMerchant_ScopesMchOrderNoByMerchant 回归锚点：mch_order_no 分支
// 必须把 merchant_id 并入 WHERE（对齐 uniq_merchant_order 复合唯一键）。历史 bug：
// 只按 mch_order_no 查时，不同商户复用同一 mch_order_no，First()（按主键升序取首行）
// 可能先取到别家的行，即便本商户确有同名订单也会被误判为不存在——service 层据此
// 把本该返回的 50001（商户订单号重复）误判成 order_no 碰撞、换号重试，最终变成 10003。
func TestOrder_FindForMerchant_ScopesMchOrderNoByMerchant(t *testing.T) {
	r, logger := newDryRunOrder(t)

	_, _ = r.FindForMerchant(context.Background(), 7, "", "mch-1")

	if !strings.Contains(logger.sql, "merchant_id = 7") {
		t.Fatalf("mch_order_no 查询未按 merchant_id 限定，SQL = %q", logger.sql)
	}
	if !strings.Contains(logger.sql, `mch_order_no = "mch-1"`) {
		t.Fatalf("mch_order_no 查询条件缺失，SQL = %q", logger.sql)
	}
}

// TestOrder_FindForMerchant_OrderNoBranchUnaffected order_no 全局唯一，查询分支保持
// 原样：不需要也不应该带 merchant_id，归属校验在查到行之后进行。
func TestOrder_FindForMerchant_OrderNoBranchUnaffected(t *testing.T) {
	r, logger := newDryRunOrder(t)

	_, _ = r.FindForMerchant(context.Background(), 7, "PO123", "")

	if strings.Contains(logger.sql, "merchant_id") {
		t.Fatalf("order_no 查询不应带 merchant_id 条件，SQL = %q", logger.sql)
	}
	if !strings.Contains(logger.sql, `order_no = "PO123"`) {
		t.Fatalf("order_no 查询条件缺失，SQL = %q", logger.sql)
	}
}
