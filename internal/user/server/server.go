// Package server 是 user 服务的传输装配层：把 service 实现与生成的 gateway 注册器
// 交给 pkg/transport，自身不实现任何传输机制（双端口、环回、拦截器、健康检查都在那边）。
package server

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"

	userv1 "github.com/yanking/go-skeleton/gen/user/v1"
	"github.com/yanking/go-skeleton/internal/user/service"
	"github.com/yanking/go-skeleton/openapi"
	"github.com/yanking/go-skeleton/pkg/transport"
)

// serviceName 服务名，同时用于定位嵌入的 OpenAPI 文档。
const serviceName = "user"

// Config user 服务的传输层配置，由 configs/user.yaml 绑定。
type Config struct {
	// GRPCAddr 纯 gRPC 监听地址。
	GRPCAddr string `yaml:"grpc_addr"`
	// HTTPAddr HTTP/JSON 监听地址。
	HTTPAddr string `yaml:"http_addr"`
	// ServeDocs 是否对外提供接口文档（GET /openapi.json 与 GET /docs）。
	// 生产环境通常关掉：接口全貌不该对外暴露。关掉即两个端点彻底不存在。
	ServeDocs bool `yaml:"serve_docs"`
}

// New 装配 user 服务的传输层。返回的 Transport 的 Components() 交给 pkg/app 编排。
func New(
	ctx context.Context,
	cfg Config,
	svc *service.UserService,
	tel transport.Telemetry,
	logger *slog.Logger,
) *transport.Transport {
	// 文档由构建链从 proto 生成并嵌进二进制，运行期不读磁盘，容器里不用额外挂文件。
	var spec []byte
	if cfg.ServeDocs {
		spec = openapi.MustSpec(serviceName, "v1")
	}

	return transport.MustNew(ctx, transport.Config{
		Service:   serviceName,
		GRPCAddr:  cfg.GRPCAddr,
		HTTPAddr:  cfg.HTTPAddr,
		Logger:    logger,
		Telemetry: tel,
		OpenAPI:   spec,
		// 注册本服务的 pb 实现与生成的 gateway 注册器——这两样是服务独有的，其余全在 pkg/transport。
		RegisterGRPC:    func(s *grpc.Server) { userv1.RegisterUserServiceServer(s, svc) },
		RegisterGateway: []transport.GatewayRegistrar{userv1.RegisterUserServiceHandler},
		// Interceptors 留给鉴权等服务自有拦截器；本服务暂无。
	})
}
