// Package initial 是 __svc__ 服务的装配层：基础组件与业务组件分函数构造
// （createInfra / createServer），App 组装并交给 pkg/app 编排启停。
package initial

import (
	"context"
	"log/slog"

	"github.com/yanking/go-skeleton/internal/__svc__/config"
	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/telemetry"
	"github.com/yanking/go-skeleton/pkg/transport"
)

// App 装配 __svc__ 的全部组件并阻塞运行，直到 ctx 取消（正常停机）或组件意外退出。
// 返回非 nil 时调用方（cmd）应以非零码退出。
//
// 组装顺序即注册顺序：基础组件在前、业务组件在后——pkg/app 按注册顺序拉起、
// 逆序停止，于是基础组件先起后停，业务组件停机期的遥测与数据操作仍有着落。
func App(ctx context.Context, c config.Config, logger *slog.Logger) error {
	// Logger 是装配期注入项，配置文件不出现，在此填入。
	c.App.Logger = logger
	infra, tel := createInfra(ctx, c)
	components := append(infra, createServer(ctx, c, logger, tel)...)
	return app.New(c.App, components...).Run(ctx)
}

// createInfra 构造基础组件：被业务依赖的资源（遥测、DB、Redis）。
// 句柄（如 *gorm.DB）解嵌后传给业务构造侧使用，组件本身留在这里注册，
// 保证仓储层拿不到关停资源的能力。
func createInfra(ctx context.Context, c config.Config) (components []app.Component, tel *telemetry.Telemetry) {
	// 遥测 Service 与日志同源（配置文件 log.name），避免两处各写一份服务名。
	c.Telemetry.Service = c.Log.Service
	tel = telemetry.New(ctx, c.Telemetry)
	components = append(components, tel)

	return components, tel
}

// createServer 构造业务组件。传输开关就是 transport 段：整段缺失即无对外出口。
// 只组装不造资源——db、redis 等句柄由 createInfra 侧产出后传参使用。
func createServer(ctx context.Context, c config.Config, logger *slog.Logger, tel *telemetry.Telemetry) (components []app.Component) {
	if c.Transport == (transport.Config{}) {
		return components
	}

	// 注入即自动挂拦截链（出口翻译 → 访问日志 → 鉴权），顺序由 transport 固定；
	// 需要鉴权时加 transport.WithAuthenticator(...)，内置策略见 transport.FixedBearerAuth。
	srv := transport.NewServer(ctx, c.Transport,
		transport.WithTracerProvider(tel.TracerProvider()),
		transport.WithLogger(logger),
		// 业务 pb 就绪后放开注册，错误出口已由 transport 统一翻译，handler 只管返回 errcode：
		//   transport.WithService(func(s *grpc.Server) {
		//       pb.RegisterXxxServer(s, handler.NewGRPC(svc))
		//   }),
	)

	return append(components, srv)
}
