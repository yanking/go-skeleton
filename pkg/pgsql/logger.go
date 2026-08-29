package pgsql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// slowSQLThreshold 慢 SQL 判定阈值，对齐 GORM 默认值。
const slowSQLThreshold = 200 * time.Millisecond

// gormLogger 把 GORM 内部日志桥接到 slog：执行失败进 Error（ErrRecordNotFound
// 是查询空结果的正常返回，除外）、慢查询进 Warn、其余进 Debug。
// LogMode 不做级别切换——放行级别统一由 slog 控制，GORM 恒全量上报。
type gormLogger struct {
	logger *slog.Logger
}

func (g gormLogger) LogMode(gormlogger.LogLevel) gormlogger.Interface { return g }

func (g gormLogger) Info(ctx context.Context, format string, args ...any) {
	g.logger.InfoContext(ctx, fmt.Sprintf(format, args...))
}

func (g gormLogger) Warn(ctx context.Context, format string, args ...any) {
	g.logger.WarnContext(ctx, fmt.Sprintf(format, args...))
}

func (g gormLogger) Error(ctx context.Context, format string, args ...any) {
	g.logger.ErrorContext(ctx, fmt.Sprintf(format, args...))
}

func (g gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	elapsed := time.Since(begin)
	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		g.logger.ErrorContext(ctx, "SQL 执行失败", "err", err, "elapsed", elapsed.String())
	case elapsed > slowSQLThreshold:
		sql, _ := fc()
		g.logger.WarnContext(ctx, "慢 SQL", "elapsed", elapsed.String(), "sql", sql)
	default:
		sql, _ := fc()
		g.logger.DebugContext(ctx, "SQL", "elapsed", elapsed.String(), "sql", sql)
	}
}
