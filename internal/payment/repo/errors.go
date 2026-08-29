package repo

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRowNotFound 表示按主键或唯一键查询未命中记录，各仓储共用。
var ErrRowNotFound = errors.New("记录不存在")

// ErrDuplicate 表示写入触发唯一约束冲突，各仓储共用。
var ErrDuplicate = errors.New("唯一键冲突")

// pgUniqueViolation 是 PostgreSQL 唯一约束冲突的 SQLSTATE 错误码。
const pgUniqueViolation = "23505"

// isUniqueViolation 判断底层错误是否为 pgx 报告的唯一约束冲突，供 Create 类方法
// 翻译为 ErrDuplicate；GORM 对 pgx 错误只做了浅层包装，故用 errors.As 取出原始
// *pgconn.PgError 再判 Code，而非依赖字符串匹配。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
