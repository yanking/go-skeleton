// Package server 是 user 服务的传输装配层：把 service 实现与生成的 gateway 注册器
// 交给 pkg/transport，自身不实现任何传输机制（双端口、环回、拦截器、健康检查都在那边）。
package server

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"

	userv1 "github.com/yanking/go-skeleton/api/user/v1"
	"github.com/yanking/go-skeleton/internal/user/service"
	"github.com/yanking/go-skeleton/pkg/transport"
)

// Config user 服务的传输层配置，由 configs/user.yaml 绑定。
type Config struct {
	// GRPCAddr 纯 gRPC 监听地址。
	GRPCAddr string `yaml:"grpc_addr"`
	// HTTPAddr HTTP/JSON 监听地址。
	HTTPAddr string `yaml:"http_addr"`
}

// New 装配 user 服务的传输层。返回的 Transport 的 Components() 交给 pkg/app 编排。
func New(
	ctx context.Context,
	cfg Config,
	svc *service.UserService,
	tel transport.Telemetry,
	logger *slog.Logger,
) *transport.Transport {
	return transport.MustNew(ctx, transport.Config{
		Service:   "user",
		GRPCAddr:  cfg.GRPCAddr,
		HTTPAddr:  cfg.HTTPAddr,
		Logger:    logger,
		Telemetry: tel,
		// 注册本服务的 pb 实现与生成的 gateway 注册器——这两样是服务独有的，其余全在 pkg/transport。
		RegisterGRPC:    func(s *grpc.Server) { userv1.RegisterUserServiceServer(s, svc) },
		RegisterGateway: []transport.GatewayRegistrar{userv1.RegisterUserServiceHandler},
		// Interceptors 留给鉴权等服务自有拦截器；本服务暂无。
	})
}
