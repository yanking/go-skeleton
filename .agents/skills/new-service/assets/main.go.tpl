// Command __svc__ 是 __svc__ 服务的装配根：读配置、构造 Logger 与根 ctx，
// 组件装配交给 initial，启停编排交给 pkg/app。
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/yanking/go-skeleton/cmd/__svc__/initial"
	"github.com/yanking/go-skeleton/internal/__svc__/config"
	"github.com/yanking/go-skeleton/pkg/conf"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

func main() {
	configFile := flag.String("config", "./configs/__svc__.yaml", "path to config file")
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	// trace_id / span_id 靠这个钩子进每条日志，日志与链路由此关联。
	c.Log.Extractors = []log.Extractor{telemetry.TraceAttrs}
	logger := log.New(c.Log)

	// 根 ctx 由 cmd 统一给出并监听信号；pkg/app 只对 ctx 取消做反应，不自己监听。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := initial.App(ctx, c, logger); err != nil {
		logger.Error("服务异常退出", "err", err)
		stop()
		os.Exit(1)
	}
}
