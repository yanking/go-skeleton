// Package initial 是 channel 服务的装配层：基础组件与业务组件分函数构造
// （createInfra / createServer），App 组装并交给 pkg/app 编排启停。
package initial

import (
	"context"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"log/slog"

	channelv1 "github.com/yanking/go-skeleton/gen/channel/v1"
	"github.com/yanking/go-skeleton/internal/channel/config"
	"github.com/yanking/go-skeleton/internal/channel/gateway_client"
	"github.com/yanking/go-skeleton/internal/channel/handler"
	"github.com/yanking/go-skeleton/internal/channel/job"
	"github.com/yanking/go-skeleton/internal/channel/repo"
	"github.com/yanking/go-skeleton/internal/channel/service"
	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/pgsql"
	"github.com/yanking/go-skeleton/pkg/telemetry"
	"github.com/yanking/go-skeleton/pkg/transport"
	"google.golang.org/grpc"
)

// App 装配 channel 的全部组件并阻塞运行，直到 ctx 取消（正常停机）或组件意外退出。
// 返回非 nil 时调用方（cmd）应以非零码退出。
//
// 组装顺序即注册顺序：基础组件在前、业务组件在后——pkg/app 按注册顺序拉起、
// 逆序停止，于是基础组件先起后停，业务组件停机期的遥测与数据操作仍有着落。
func App(ctx context.Context, c config.Config, logger *slog.Logger) error {
	// Logger 是装配期注入项，配置文件不出现，在此填入。
	c.App.Logger = logger
	infra, tel, db := createInfra(ctx, c, logger)
	components := append(infra, createServer(ctx, c, logger, tel, db)...)
	return app.New(c.App, components...).Run(ctx)
}

// createInfra 构造基础组件：遥测与数据库。组件本身留在这里注册；
// gorm 句柄解嵌后传给业务构造侧，仓储层因此拿不到关停资源的能力。
func createInfra(ctx context.Context, c config.Config, logger *slog.Logger) (components []app.Component, tel *telemetry.Telemetry, db *pgsql.DB) {
	// 遥测 Service 与日志同源（配置文件 log.name），避免两处各写一份服务名。
	c.Telemetry.Service = c.Log.Service
	tel = telemetry.New(ctx, c.Telemetry)
	components = append(components, tel)

	c.Pgsql.TracerProvider = tel.TracerProvider()
	c.Pgsql.Logger = logger
	db = pgsql.New(c.Pgsql)
	components = append(components, db)

	return components, tel, db
}

// createServer 构造业务组件：gRPC 传输与补单对账 job。
// 内网东西向服务，与原实现一致不挂鉴权；如需收紧加 WithAuthenticator。
func createServer(ctx context.Context, c config.Config, logger *slog.Logger, tel *telemetry.Telemetry, db *pgsql.DB) (components []app.Component) {
	// 出站 HTTP 客户端：渠道适配器共用，接遥测后下游调用埋 client span。
	hc := httpc.New(httpc.Config{TracerProvider: tel.TracerProvider()})

	// 路由表在构造期完成首次加载：DB 不可达当场死，不带病上线。
	svc, err := service.New(ctx, repo.New(db.DB), hc, logger)
	if err != nil {
		logger.Error("渠道路由表初始化失败", "err", err)
		panic(err)
	}

	// 补单 job 依赖 gateway-rpc；未配地址即不装配（纯回调驱动形态）。
	if c.Gateway.Addr != "" {
		gw, err := gatewayclient.New(c.Gateway.Addr)
		if err != nil {
			logger.Error("gateway-rpc 客户端构造失败", "addr", c.Gateway.Addr, "err", err)
			panic(err)
		}
		components = append(components, gwCloser{gw}, job.New(svc, gw, logger))
	}

	if c.Transport == (transport.Config{}) {
		return components
	}

	// 注入即自动挂拦截链（出口翻译 → 访问日志），顺序由 transport 固定；
	// handler 只管返回 errcode，出口翻译由拦截器统一处理。
	srv := transport.NewServer(ctx, c.Transport,
		transport.WithTracerProvider(tel.TracerProvider()),
		transport.WithLogger(logger),
		transport.WithService(func(s *grpc.Server) {
			channelv1.RegisterChannelServiceServer(s, handler.NewGRPC(svc))
		}),
	)

	return append(components, srv)
}

// gwCloser 把 gateway 连接的关闭纳停机序列：job 停止后、遥测停止前断连。
type gwCloser struct{ c *gatewayclient.Client }

// Name 组件名。
func (g gwCloser) Name() string { return "gateway-client" }

// Start 连接资源型组件，无阻塞循环。
func (g gwCloser) Start(context.Context) error { return nil }

// Stop 停机时关闭底层 gRPC 连接。
func (g gwCloser) Stop(context.Context) error { return g.c.Close() }
