package repo

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/utils/tests"
)

// capturingLogger 手写 mock：只记录最近一次 GORM 生成的（已插值）SQL，配合 DryRun
// 用于断言查询/写入语句的形状，不落地任何真实数据库连接、不引入新依赖
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

// newDryRun 构造只生成 SQL、不执行、不连接真实数据库的 GORM 句柄，用于锚定
// 各仓储生成语句的形状（如 upsert 的冲突目标、查询的过滤条件）。
func newDryRun(t *testing.T, logger *capturingLogger) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(tests.DummyDialector{}, &gorm.Config{Logger: logger, DryRun: true})
	if err != nil {
		t.Fatalf("打开 DryRun 连接: %v", err)
	}
	return db
}
