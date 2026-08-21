// Command user 是 user 服务的装配根：读配置、构造组件并注入，交由 pkg/app 编排启停。
// 本文件是全服务唯一知道「谁依赖谁」的地方，不引 DI 框架。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/yanking/go-skeleton/internal/user/biz"
	"github.com/yanking/go-skeleton/internal/user/data"
	"github.com/yanking/go-skeleton/internal/user/server"
	"github.com/yanking/go-skeleton/internal/user/service"
	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/conf"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/mysql"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

// serviceName 服务名，写入日志的 service 字段与遥测 resource 的 service.name。
const serviceName = "user"

// version 由 -ldflags "-X main.version=..." 在构建时注入。
var version = "dev"

func main() {
	configFile := flag.String("conf", "configs/user.yaml", "配置文件路径")
	flag.Parse()

	var cfg Config
	conf.MustLoad(*configFile, &cfg)

	// 根 ctx 由 cmd 统一给出并监听信号；pkg/app 只对 ctx 取消做反应，不自己监听信号。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 以下三段填的都是 pkg Config 里标了 yaml:"-" 的字段：它们是装配期注入项，
	// 配置文件里没有也不该有。声明式参数已由 conf.MustLoad 绑好，此处不再逐字段抄写。
	cfg.Log.Service = serviceName
	// trace_id / span_id 靠这个钩子进每条日志，日志与链路由此关联。
	cfg.Log.Extractors = []log.Extractor{telemetry.TraceAttrs}
	logger := log.MustNew(cfg.Log)
	// 接管全局默认 Logger，让第三方库经 slog 打的日志也走同一 Handler。
	slog.SetDefault(logger)

	cfg.Telemetry.Service = serviceName
	cfg.Telemetry.Version = version
	cfg.Telemetry.Logger = logger
	tel := telemetry.MustNew(ctx, cfg.Telemetry)

	cfg.MySQL.Telemetry = tel
	cfg.MySQL.Logger = logger
	db := mysql.MustNew(ctx, cfg.MySQL)

	// 四层手工注入：data → biz → service → server。
	// 往下传的是 db.DB（*sql.DB）而非 *mysql.Client——后者带着 Start/Stop，
	// 仓储不该有关停连接池的能力。
	repo := data.NewUserRepo(data.NewData(db.DB), logger)
	svc := service.NewUserService(biz.NewUserUsecase(repo, logger), logger)
	transport := server.New(ctx, cfg.Server, svc, tel, logger)

	// 注册顺序即拉起顺序、其逆序即停机顺序：telemetry 最先注册 = 最后停，
	// 于是前面所有组件停机期间的 span 与 metric 都还能被记录并 flush。
	components := append([]app.Component{tel, db}, transport.Components()...)

	if err := app.New(app.Config{
		Logger:      logger,
		StopTimeout: cfg.StopTimeout,
	}, components...).Run(ctx); err != nil {
		logger.Error("服务异常退出", "err", err)
		os.Exit(1)
	}
}
