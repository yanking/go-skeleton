package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	userv1 "github.com/yanking/go-skeleton/gen/user/v1"
	"github.com/yanking/go-skeleton/internal/user/biz"
	"github.com/yanking/go-skeleton/internal/user/service"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

var createdAt = time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)

// fakeRepo 按需返回固定结果或错误，用于驱动 service 层的转换与错误映射。
type fakeRepo struct {
	user *biz.User
	err  error
}

func (f *fakeRepo) Create(context.Context, *biz.User) (*biz.User, error) { return f.user, f.err }
func (f *fakeRepo) Get(context.Context, int64) (*biz.User, error)        { return f.user, f.err }

func newService(repo biz.UserRepo) *service.UserService {
	return service.NewUserService(biz.NewUserUsecase(repo, discardLogger()), discardLogger())
}

func TestGetUserConvertsToProto(t *testing.T) {
	svc := newService(&fakeRepo{user: &biz.User{
		ID: 7, Name: "颜", Email: "yan@example.com", CreatedAt: createdAt,
	}})

	got, err := svc.GetUser(context.Background(), &userv1.GetUserRequest{Id: 7})
	if err != nil {
		t.Fatalf("GetUser 返回错误: %v", err)
	}

	want := &userv1.GetUserResponse{User: &userv1.User{
		Id: 7, Name: "颜", Email: "yan@example.com",
		CreatedAt: timestamppb.New(createdAt),
	}}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("响应不符 (-want +got):\n%s", diff)
	}
}

func TestErrorsMapToStatusCodes(t *testing.T) {
	// 错误只在 service 这一处翻译，不散落各层——这条用例锁住映射表。
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "用户不存在", err: biz.ErrUserNotFound, want: codes.NotFound},
		{name: "邮箱被占用", err: biz.ErrEmailTaken, want: codes.AlreadyExists},
		{name: "未预期的错误一律 Internal", err: errors.New("数据库炸了"), want: codes.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(&fakeRepo{err: tt.err})

			_, err := svc.GetUser(context.Background(), &userv1.GetUserRequest{Id: 1})

			if got := status.Code(err); got != tt.want {
				t.Errorf("状态码 got %v, want %v (err=%v)", got, tt.want, err)
			}
		})
	}
}

func TestCreateUserLeaksNoInternalDetail(t *testing.T) {
	// 未预期的错误不能把内部信息泄露给客户端。
	svc := newService(&fakeRepo{err: errors.New("dial tcp 10.0.0.1:3306: connection refused")})

	_, err := svc.CreateUser(context.Background(), &userv1.CreateUserRequest{
		Name: "颜", Email: "yan@example.com",
	})

	if msg := status.Convert(err).Message(); msg != "内部错误" {
		t.Errorf("错误消息不该泄露内部细节, got %q", msg)
	}
}
