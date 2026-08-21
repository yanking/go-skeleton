package data_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql" // 注册 database/sql 驱动

	"github.com/yanking/go-skeleton/internal/user/biz"
	"github.com/yanking/go-skeleton/internal/user/data"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newRepo 连上测试库并清空 users 表；未配 MYSQL_DSN 即跳过（CI 基线里没有数据库）。
func newRepo(t *testing.T) biz.UserRepo {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 MYSQL_DSN，跳过需要真实 MySQL 的用例")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开连接: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(context.Background(), "DELETE FROM users"); err != nil {
		t.Fatalf("清空 users 表（迁移跑了吗？）: %v", err)
	}
	return data.NewUserRepo(data.NewData(db), discardLogger())
}

func TestCreateThenGet(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, &biz.User{Name: "颜", Email: "yan@example.com"})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if created.ID == 0 {
		t.Error("Create 应回填自增 ID")
	}
	if created.CreatedAt.IsZero() {
		t.Error("Create 应回填服务端生成的创建时间")
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.Email != "yan@example.com" || got.Name != "颜" {
		t.Errorf("取回的用户不符: %+v", got)
	}
}

func TestDuplicateEmailMapsToDomainError(t *testing.T) {
	// MySQL 的 1062 必须在 data 层就翻译成领域错误，不能漏到 biz/service 去。
	repo := newRepo(t)
	ctx := context.Background()

	if _, err := repo.Create(ctx, &biz.User{Name: "颜", Email: "dup@example.com"}); err != nil {
		t.Fatalf("首次 Create 失败: %v", err)
	}

	_, err := repo.Create(ctx, &biz.User{Name: "另一个颜", Email: "dup@example.com"})

	if !errors.Is(err, biz.ErrEmailTaken) {
		t.Errorf("want ErrEmailTaken, got %v", err)
	}
}

func TestGetMissingMapsToDomainError(t *testing.T) {
	_, err := newRepo(t).Get(context.Background(), 999999)

	if !errors.Is(err, biz.ErrUserNotFound) {
		t.Errorf("want ErrUserNotFound, got %v", err)
	}
}
