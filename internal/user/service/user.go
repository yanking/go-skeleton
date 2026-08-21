// Package service 实现 pb 生成的 UserServiceServer：负责 pb ↔ 领域类型的转换，
// 以及把 biz 的业务错误集中映射为 gRPC 状态码。领域逻辑不在这里。
package service

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	userv1 "github.com/yanking/go-skeleton/gen/user/v1"
	"github.com/yanking/go-skeleton/internal/user/biz"
)

// UserService 实现 userv1.UserServiceServer。
type UserService struct {
	userv1.UnimplementedUserServiceServer

	uc     *biz.UserUsecase
	logger *slog.Logger
}

// NewUserService 构造 service，依赖经参数显式传入。
func NewUserService(uc *biz.UserUsecase, logger *slog.Logger) *UserService {
	return &UserService{uc: uc, logger: logger}
}

// CreateUser 创建用户。入参校验由 protovalidate 拦截器统一执行，此处只做转换。
func (s *UserService) CreateUser(ctx context.Context, req *userv1.CreateUserRequest) (*userv1.CreateUserResponse, error) {
	u, err := s.uc.Create(ctx, &biz.User{Name: req.GetName(), Email: req.GetEmail()})
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return &userv1.CreateUserResponse{User: toProto(u)}, nil
}

// GetUser 按 ID 取用户。
func (s *UserService) GetUser(ctx context.Context, req *userv1.GetUserRequest) (*userv1.GetUserResponse, error) {
	u, err := s.uc.Get(ctx, req.GetId())
	if err != nil {
		return nil, s.mapError(ctx, err)
	}
	return &userv1.GetUserResponse{User: toProto(u)}, nil
}

// mapError 把业务错误翻译成 gRPC 状态码。全服务只有这一处做翻译，映射表随本文件维护。
func (s *UserService) mapError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, biz.ErrUserNotFound):
		return status.Error(codes.NotFound, "用户不存在")
	case errors.Is(err, biz.ErrEmailTaken):
		return status.Error(codes.AlreadyExists, "邮箱已被占用")
	default:
		// 未预期的错误不把内部信息泄露给客户端，但必须就地留下原因——
		// 否则线上只剩一个光秃秃的 Internal，无从查起。
		s.logger.ErrorContext(ctx, "未预期的业务错误", "err", err)
		return status.Error(codes.Internal, "内部错误")
	}
}

// toProto 领域类型转 pb。pb 类型止步于本层，biz 只见领域类型。
func toProto(u *biz.User) *userv1.User {
	return &userv1.User{
		Id:        u.ID,
		Name:      u.Name,
		Email:     u.Email,
		CreatedAt: timestamppb.New(u.CreatedAt),
	}
}
