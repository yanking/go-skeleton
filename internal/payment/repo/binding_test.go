package repo

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/utils/tests"
)

// newDryRunBinding 构造只生成 SQL、不执行、不连接真实数据库的 Binding 仓储，
// 用于锚定查询条件的形状（如 currency 是否参与过滤）。
func newDryRunBinding(t *testing.T) (*Binding, *capturingLogger) {
	t.Helper()
	logger := &capturingLogger{}
	db, err := gorm.Open(tests.DummyDialector{}, &gorm.Config{Logger: logger, DryRun: true})
	if err != nil {
		t.Fatalf("打开 DryRun 连接: %v", err)
	}
	return NewBinding(db), logger
}

// TestBinding_ListCandidates_CurrencyFilter 非空币种应按 currency 过滤。
func TestBinding_ListCandidates_CurrencyFilter(t *testing.T) {
	r, logger := newDryRunBinding(t)

	_, _ = r.ListCandidates(context.Background(), 7, "USD")

	if !strings.Contains(logger.sql, `channel_instances.currency = "USD"`) {
		t.Fatalf("指定币种时应带 currency 过滤，SQL = %q", logger.sql)
	}
}

// TestBinding_ListCandidates_EmptyCurrencyMeansAll 空币种对应「全币种」契约
// （ListAvailableChannelsRequest.currency 为空即返回全部币种），不应带 currency 条件。
func TestBinding_ListCandidates_EmptyCurrencyMeansAll(t *testing.T) {
	r, logger := newDryRunBinding(t)

	_, _ = r.ListCandidates(context.Background(), 7, "")

	if strings.Contains(logger.sql, "currency") {
		t.Fatalf("空币种应不限币种，SQL 不应含 currency 条件，SQL = %q", logger.sql)
	}
	if !strings.Contains(logger.sql, "merchant_channels.merchant_id = 7") {
		t.Fatalf("应保留商户与启用状态过滤，SQL = %q", logger.sql)
	}
}
