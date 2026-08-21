package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	driver "github.com/go-sql-driver/mysql"

	"github.com/yanking/go-skeleton/internal/user/biz"
	"github.com/yanking/go-skeleton/internal/user/data/sqlc"
)

// mysqlErrDuplicateEntry 唯一键冲突的 MySQL 错误码。
const mysqlErrDuplicateEntry = 1062

type userRepo struct {
	data   *Data
	logger *slog.Logger
}

// NewUserRepo 构造用户仓储。只收 *Data，新增存储组件时本签名不变。
func NewUserRepo(d *Data, logger *slog.Logger) biz.UserRepo {
	return &userRepo{data: d, logger: logger}
}

// Create 落库新用户。邮箱唯一键冲突翻译成 biz.ErrEmailTaken——
// 存储层的错误码在这里就转成领域错误，不让 MySQL 的 1062 漏到上层去。
func (r *userRepo) Create(ctx context.Context, u *biz.User) (*biz.User, error) {
	res, err := r.data.queries.CreateUser(ctx, sqlc.CreateUserParams{Name: u.Name, Email: u.Email})
	if err != nil {
		if isDuplicateEntry(err) {
			return nil, biz.ErrEmailTaken
		}
		return nil, fmt.Errorf("插入用户: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("取新用户自增 ID: %w", err)
	}
	// 回查一次，让创建时间等服务端生成的字段也带回去。
	return r.Get(ctx, id)
}

// Get 按 ID 取用户，查无此人翻译成 biz.ErrUserNotFound。
func (r *userRepo) Get(ctx context.Context, id int64) (*biz.User, error) {
	row, err := r.data.queries.GetUser(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, biz.ErrUserNotFound
	case err != nil:
		return nil, fmt.Errorf("查询用户 %d: %w", id, err)
	}
	return &biz.User{ID: row.ID, Name: row.Name, Email: row.Email, CreatedAt: row.CreatedAt}, nil
}

// isDuplicateEntry 判断是否唯一键冲突。用驱动的错误类型判定，不匹配错误字符串。
func isDuplicateEntry(err error) bool {
	var mysqlErr *driver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlErrDuplicateEntry
}
