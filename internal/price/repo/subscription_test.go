package repo

import (
	"context"
	"strings"
	"testing"
)

// TestSubscriptionListEnabled_FiltersDisabled 回归锚点：ListEnabled 必须按
// enabled 过滤，否则重载器会把已停用的订阅当成活跃订阅重新建连。
//
// 只断言 SQL 含子串 "enabled" 不够：过滤值若反写成 enabled = false（语义完全
// 相反），子串照样命中、测试照样通过。这里断言完整的 "enabled = true"，把过滤值
// 也钉死。
func TestSubscriptionListEnabled_FiltersDisabled(t *testing.T) {
	lg := &capturingLogger{}
	db := newDryRun(t, lg)
	_, _ = NewSubscription(db).ListEnabled(context.Background())
	t.Log(lg.sql)

	if !strings.Contains(lg.sql, "enabled = true") {
		t.Errorf("查询未按 enabled = true 过滤:\n%s", lg.sql)
	}
}
