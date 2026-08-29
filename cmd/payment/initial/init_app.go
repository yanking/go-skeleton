// Package initial 是 payment 服务的装配层：基础组件与业务组件分函数构造
// （createInfra / createServer），App 组装并交给 pkg/app 编排启停。
package initial

import (
	"context"
	"log/slog"

	gatewayv1 "github.com/yanking/go-skeleton/gen/gateway/v1"
	paymentv1 "github.com/yanking/go-skeleton/gen/payment/v1"
	channelclient "github.com/yanking/go-skeleton/internal/payment/channel_client"
	"github.com/yanking/go-skeleton/internal/payment/config"
	"github.com/yanking/go-skeleton/internal/payment/handler"
	"github.com/yanking/go-skeleton/internal/payment/job"
	"github.com/yanking/go-skeleton/internal/payment/repo"
	"github.com/yanking/go-skeleton/internal/payment/service"
	"github.com/yanking/go-skeleton/openapi"
	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"github.com/yanking/go-skeleton/pkg/pgsql"
	"github.com/yanking/go-skeleton/pkg/queue"
	"github.com/yanking/go-skeleton/pkg/telemetry"
	"github.com/yanking/go-skeleton/pkg/transport"
	"google.golang.org/grpc"
)

// App 装配 payment 的全部组件并阻塞运行，直到 ctx 取消（正常停机）或组件意外退出。
// 返回非 nil 时调用方（cmd）应以非零码退出。
//
// 组装顺序即注册顺序：基础组件在前、业务组件在后——pkg/app 按注册顺序拉起、
// 逆序停止，于是基础组件先起后停，业务组件停机期的遥测与数据操作仍有着落。
func App(ctx context.Context, c config.Config, logger *slog.Logger) error {
	// Logger 是装配期注入项，配置文件不出现，在此填入。
	c.App.Logger = logger
	infra, tel, db, qc := createInfra(ctx, c, logger)
	components := append(infra, createServer(ctx, c, logger, tel, db, qc)...)
	return app.New(c.App, components...).Run(ctx)
}

// createInfra 构造基础组件：遥测、数据库与任务队列客户端。组件本身留在这里注册，
// 句柄解嵌后传给业务构造侧，业务层因此拿不到关停资源的能力。
func createInfra(ctx context.Context, c config.Config, logger *slog.Logger) (components []app.Component, tel *telemetry.Telemetry, db *pgsql.DB, qc *queue.Client) {
	// 遥测 Service 与日志同源（配置文件 log.name），避免两处各写一份服务名。
	c.Telemetry.Service = c.Log.Service
	tel = telemetry.New(ctx, c.Telemetry)
	components = append(components, tel)

	c.Pgsql.TracerProvider = tel.TracerProvider()
	c.Pgsql.Logger = logger
	db = pgsql.New(c.Pgsql)
	components = append(components, db)

	// 入队客户端只是资源，没有常驻循环；包装成组件只为把 Close 纳入停机序列。
	// 注册在基础组件段：逆序停止时它晚于 worker 与各 job 关闭，兜底任务停机前仍可入队。
	qc = queue.NewClient(c.Queue)
	components = append(components, closer{name: "queue-client", fn: qc.Close})

	return components, tel, db, qc
}

// createServer 构造业务组件：channel 客户端、用例集、通知消费者、三个定时任务与传输。
// 传输开关就是 transport 段：整段缺失即无对外出口。
// 只组装不造资源——db、queue 客户端等句柄由 createInfra 侧产出后传参使用。
func createServer(ctx context.Context, c config.Config, logger *slog.Logger, tel *telemetry.Telemetry, db *pgsql.DB, qc *queue.Client) (components []app.Component) {
	// channel 服务是下单、验签、实例同步的必经之路，连不上就没有可用形态。
	cc, err := channelclient.New(c.Channel.Addr)
	if err != nil {
		logger.Error("channel 服务客户端构造失败", "addr", c.Channel.Addr, "err", err)
		panic(err)
	}
	components = append(components, closer{name: "channel-client", fn: cc.Close})

	svc := service.New(
		service.Config{CallbackBaseURL: c.Notify.CallbackBaseURL},
		service.Deps{
			Merchants:     repo.NewMerchant(db.DB),
			Instances:     repo.NewInstance(db.DB),
			Bindings:      repo.NewBinding(db.DB),
			Orders:        repo.NewOrder(db.DB),
			Callbacks:     repo.NewCallback(db.DB),
			Notifications: repo.NewNotification(db.DB),
			Channel:       cc,
			Queue:         qc,
			// 出站 HTTP 客户端只用于给商户推通知；接遥测后每次通知都有 client span。
			HTTP: httpc.New(httpc.Config{TracerProvider: tel.TracerProvider()}),
		}, logger)

	// 装配期先全量同步一次渠道实例：本地副本为空时选路无候选，等不到首个 job 周期
	// 就会把所有下单打成「无可用渠道」。同步不通当场死，不带病上线。
	if err := svc.SyncInstances(ctx); err != nil {
		logger.Error("渠道实例首次同步失败", "addr", c.Channel.Addr, "err", err)
		panic(err)
	}

	// 通知消费者：注册的任务类型名与 service 侧入队用的同名，两处不可分头改。
	worker := queue.NewWorker(c.Queue, logger)
	worker.Handle(service.TaskNotify, svc.SendNotify)

	components = append(components, worker,
		job.NewSync(svc, c.Channel.SyncInterval, logger),
		job.NewOrderSweep(svc, logger),
		job.NewNotifySweep(svc, logger))

	if c.Transport == (transport.Config{}) {
		return components
	}

	// 注入即自动挂拦截链（出口翻译 → 访问日志），顺序由 transport 固定；
	// handler 只管返回 errcode，出口翻译由拦截器统一处理。
	// 商户鉴权走的是报文签名（service.Authenticate），不是传输层 Authenticator。
	srv := transport.NewServer(ctx, c.Transport,
		transport.WithTracerProvider(tel.TracerProvider()),
		transport.WithLogger(logger),
		transport.WithService(func(s *grpc.Server) {
			paymentv1.RegisterPaymentServiceServer(s, handler.NewGRPC(svc))
			// 网关对账出口：与 channel 调用上游 gateway-rpc 的是同一份契约镜像。
			gatewayv1.RegisterGatewayServiceServer(s, handler.NewGateway(svc))
		}),
		// 只转译商户面的 payment 契约；gateway 对账出口不开 HTTP。
		transport.WithGateway(paymentv1.RegisterPaymentServiceHandler),
		transport.WithMount("/docs", openapi.Handler()),
		// 渠道回调入口：路径尾部带实例 id，故按前缀挂载。
		transport.WithMount("/callbacks/", handler.NewCallback(svc)),
	)

	return append(components, srv)
}

// closer 把只需在停机时释放的资源（无常驻循环）纳入 app 的停机序列。
type closer struct {
	name string
	fn   func() error
}

// Name 组件名。
func (c closer) Name() string { return c.name }

// Start 资源型组件无常驻循环，直接返回。
func (c closer) Start(context.Context) error { return nil }

// Stop 释放底层连接。
func (c closer) Stop(context.Context) error { return c.fn() }
