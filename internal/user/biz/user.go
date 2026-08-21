// Package biz 承载 user 服务的领域逻辑：定义领域类型、仓储接口与业务错误。
// 本包不感知传输与存储——既不 import pb，也不 import 任何存储驱动。
package biz

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// 业务错误在 biz 层定义，由 service 层集中映射为 gRPC 状态码，只在那一处翻译。
var (
	// ErrUserNotFound 用户不存在。
	ErrUserNotFound = errors.New("用户不存在")
	// ErrEmailTaken 邮箱已被其他用户占用。
	ErrEmailTaken = errors.New("邮箱已被占用")
)

// User 用户领域类型。它与 pb 生成的 user.v1.User 是两回事，转换代码写在 service 层。
type User struct {
	ID        int64
	Name      string
	Email     string
	CreatedAt time.Time
}

// UserRepo 用户仓储，由 data 层实现（依赖倒置）。
type UserRepo interface {
	// Create 落库一个新用户，返回带 ID 与创建时间的完整用户。
	// 邮箱冲突须返回 ErrEmailTaken。
	Create(ctx context.Context, u *User) (*User, error)
	// Get 按 ID 取用户，不存在须返回 ErrUserNotFound。
	Get(ctx context.Context, id int64) (*User, error)
}

// UserUsecase 用户领域逻辑。
type UserUsecase struct {
	repo   UserRepo
	logger *slog.Logger
}

// NewUserUsecase 构造用例，依赖经参数显式传入，不引 DI 框架。
func NewUserUsecase(repo UserRepo, logger *slog.Logger) *UserUsecase {
	return &UserUsecase{repo: repo, logger: logger}
}

// Create 创建用户。邮箱在落库前规范化为小写并裁掉首尾空白——
// 「邮箱大小写不敏感」是领域规则而非存储细节，放在这里才不会因为换存储实现而漏掉。
func (uc *UserUsecase) Create(ctx context.Context, u *User) (*User, error) {
	u.Email = strings.ToLower(strings.TrimSpace(u.Email))
	u.Name = strings.TrimSpace(u.Name)
	return uc.repo.Create(ctx, u)
}

// Get 按 ID 取用户。
func (uc *UserUsecase) Get(ctx context.Context, id int64) (*User, error) {
	return uc.repo.Get(ctx, id)
}
