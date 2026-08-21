package biz_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yanking/go-skeleton/internal/user/biz"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// fakeRepo 是 biz 层测试用的手写 fake——biz 测试不碰真实存储，也不引 mock 框架。
type fakeRepo struct {
	created *biz.User
	got     map[int64]*biz.User
	err     error
}

func (f *fakeRepo) Create(_ context.Context, u *biz.User) (*biz.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.created = u
	saved := *u
	saved.ID = 1
	return &saved, nil
}

func (f *fakeRepo) Get(_ context.Context, id int64) (*biz.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.got[id]
	if !ok {
		return nil, biz.ErrUserNotFound
	}
	return u, nil
}

func TestCreateNormalizesEmail(t *testing.T) {
	// 邮箱大小写不敏感是领域规则，不是存储细节，所以规范化归 biz——
	// 放到 data 层的话，换个存储实现就会漏掉。
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "全大写转小写", input: "YAN@Example.COM", want: "yan@example.com"},
		{name: "首尾空白被裁掉", input: "  yan@example.com  ", want: "yan@example.com"},
		{name: "已规范则原样", input: "yan@example.com", want: "yan@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{}
			uc := biz.NewUserUsecase(repo, discardLogger())

			if _, err := uc.Create(context.Background(), &biz.User{Name: "颜", Email: tt.input}); err != nil {
				t.Fatalf("Create 返回错误: %v", err)
			}
			if diff := cmp.Diff(tt.want, repo.created.Email); diff != "" {
				t.Errorf("落库的邮箱不符 (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGetPropagatesNotFound(t *testing.T) {
	uc := biz.NewUserUsecase(&fakeRepo{got: map[int64]*biz.User{}}, discardLogger())

	_, err := uc.Get(context.Background(), 42)

	if !errors.Is(err, biz.ErrUserNotFound) {
		t.Errorf("want ErrUserNotFound, got %v", err)
	}
}
