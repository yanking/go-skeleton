package mysql

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
//
// SQL 一律以参数化原文入日志（WHERE email = ? 而非插值后的明文），见 ParamsFilter。
type gormLogger struct {
	logger *slog.Logger
}

// ParamsFilter 实现 gorm.ParamsFilter：丢掉参数，只让 Explain 拿到参数化原文。
// GORM 默认把参数插值进 SQL 再交给 Trace，于是手机号、token、散列前的口令会随
// WHERE / INSERT 的值进日志；而慢 SQL 走 Warn 级，生产一定放行，故库一变慢
// 泄漏量最大。放弃参数值是刻意的取舍：要定位具体某一行，用 trace 的 span
// 或业务日志显式记必要的标识，不靠全量 SQL 日志。
//
// 与 otelsql 的 span 行为一致（db.query.text 也是参数化原文），两条通道同一尺度。
func (g gormLogger) ParamsFilter(_ context.Context, sql string, _ ...any) (string, []any) {
	return sql, nil
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

// Trace 记一条 SQL 的执行结果。三条分支都记 SQL 与影响行数——失败分支尤其需要
// （死锁、约束冲突、语句超时时只说「有条 SQL 失败了」等于没说），而 SQL 已由
// ParamsFilter 脱敏，记它不再有泄漏代价。Debug 被滤掉时不调 fc：它要拼一遍
// SQL 字符串，而这是每条语句都会走的热路径。
func (g gormLogger) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	elapsed := time.Since(begin)
	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		sql, rows := fc()
		g.logger.ErrorContext(ctx, "SQL 执行失败",
			"err", err, "elapsed", elapsed.String(), "sql", sql, "rows", rows)
	case elapsed > slowSQLThreshold:
		sql, rows := fc()
		g.logger.WarnContext(ctx, "慢 SQL",
			"elapsed", elapsed.String(), "sql", sql, "rows", rows)
	case g.logger.Enabled(ctx, slog.LevelDebug):
		sql, rows := fc()
		g.logger.DebugContext(ctx, "SQL",
			"elapsed", elapsed.String(), "sql", sql, "rows", rows)
	}
}
