// Package initial 是 __svc__ 服务的装配层：基础组件与业务组件分函数构造
// （createInfra / createServer），App 组装并交给 pkg/app 编排启停。
package initial

import (
	"context"
	"errors"
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
	infra, tel := createInfra(ctx, c, logger)
	components := append(infra, createServer(ctx, c, logger, tel)...)
	return app.New(c.App, components...).Run(ctx)
}

// createInfra 构造基础组件：被业务依赖的资源（遥测、DB、Redis）。
// 句柄（如 *gorm.DB）解嵌后传给业务构造侧使用，组件本身留在这里注册，
// 保证仓储层拿不到关停资源的能力。
func createInfra(ctx context.Context, c config.Config, logger *slog.Logger) (components []app.Component, tel *telemetry.Telemetry) {
	// 遥测 Service 与日志同源（配置文件 log.name），避免两处各写一份服务名。
	c.Telemetry.Service = c.Log.Service
	// 遥测自身的日志（装配结果、SDK 导出失败）也走统一出口。
	c.Telemetry.Logger = logger
	tel = telemetry.New(ctx, c.Telemetry)
	components = append(components, tel)

	return components, tel
}

// createServer 构造业务组件。传输开关就是 transport 段：整段缺失即无对外出口。
// HTTP 出口是 gateway 代理，配了 http_addr 才监听——同一份二进制按部署选纯 gRPC 或双协议。
// 只组装不造资源——db、redis 等句柄由 createInfra 侧产出后传参使用。
func createServer(ctx context.Context, c config.Config, logger *slog.Logger, tel *telemetry.Telemetry) (components []app.Component) {
	// 本变体必有对外出口，所以传输段缺失是配置错误，不是「这个服务不开端口」：
	// 放过的话服务会「启动成功」却不监听任何端口，只有探针能发现。
	// 真要做无出口的服务，用 new-service 的 none 变体。
	if c.Transport == (transport.Config{}) {
		panic(errors.New("装配 __svc__: 配置缺 transport 段，而本服务有对外出口"))
	}

	// 注入即自动挂拦截链（出口翻译 → 处理超时 → 访问日志 → panic 兜底 → 鉴权），
	// 顺序由 transport 固定；
	// 需要鉴权时加 transport.WithAuthenticator(...)，内置策略见 transport.FixedBearerAuth。
	// 以纯 gRPC 起步：业务 pb 就绪后，构造器换成 NewRPCRESTServer（签名多一个 ctx），
	// 并把 configs/__svc__.yaml 的 http_addr 与下面三行注释一并放开，双协议形态即成型。
	// 两步必须同时做——NewRPCRESTServer 必配 http_addr，先放开端口而没有路由会在装配期报错。
	srv := transport.NewRPCServer(c.Transport,
		transport.WithTracerProvider(tel.TracerProvider()),
		// 指标 Provider 要显式给：漏了不报错，只是请求量、错误率、延迟分位
		// （RED 三件套）静默消失，而「接口变慢、错误率涨」只能靠它们定位。
		transport.WithMeterProvider(tel.MeterProvider()),
		transport.WithLogger(logger),
		//   transport.WithService(func(s *grpc.Server) {
		//       pb.RegisterXxxServer(s, handler.NewGRPC(svc))
		//   }),
		//   transport.WithGateway(pb.RegisterXxxHandler),
		//   transport.WithMount("/docs", openapi.Handler()), // spec 由 `make proto` 生成并 embed
		// 有数据库或 Redis 依赖时加就绪检查——装配期不 ping 库（DB 比进程晚就绪是
		// 常态）这个取舍的另一半就在这里：不加的话，连不上库的实例照样通过就绪
		// 探针，编排系统继续给它发流量，每个请求都 500。
		//   transport.WithReadinessCheck(func(ctx context.Context) error {
		//       return db.WithContext(ctx).Exec("SELECT 1").Error
		//   }),
	)

	return append(components, srv)
}
