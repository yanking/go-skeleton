// Package data 实现 biz 定义的仓储接口：把领域类型落到 MySQL。
// SQL 写在 query/*.sql，访问代码由 sqlc 生成到 sqlc/，禁手改。
package data

import (
	"database/sql"

	"github.com/yanking/go-skeleton/internal/user/data/sqlc"
)

// Data 持有本服务全部存储连接。字段全部未导出——包外（含 biz、service）即便拿到
// *Data 也取不出连接，只能经本包的仓储访问。这是「biz 不得绕过仓储直接查库」的编译期保障。
type Data struct {
	queries *sqlc.Queries
}

// NewData 由 cmd 在装配期调用，参数是 pkg/mysql 造好的 *sql.DB。
// 收标准库类型而非 *mysql.Client：后者带着 Start/Stop，data 层不该有关停连接池的能力。
func NewData(db *sql.DB) *Data {
	return &Data{queries: sqlc.New(db)}
}
