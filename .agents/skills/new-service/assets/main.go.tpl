// Command __svc__ 是 __svc__ 服务的装配根：读配置、构造 Logger 与根 ctx，
// 组件装配交给 initial，启停编排交给 pkg/app。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/yanking/go-skeleton/cmd/__svc__/initial"
	"github.com/yanking/go-skeleton/internal/__svc__/config"
	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/conf"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

// 退出码分三种，让部署侧的告警规则能区别对待：一次慢停机不该和进程崩溃
// 触发同一条告警，更不该半夜叫人起来。
const (
	// exitRunFailure 运行期出事：组件意外退出，或停机过程中有组件报错。
	exitRunFailure = 1
	// exitStopTimeout 停机未在宽限期内跑完。资源可能没完全释放，但这是
	// 「慢」而不是「崩」。
	exitStopTimeout = 2
	// exitForced 收到第二个信号后放弃排空直接退出（128 + SIGINT）。
	exitForced = 130
)

func main() {
	configFile := flag.String("config", "./configs/__svc__.yaml", "path to config file")
	flag.Parse()

	var c config.Config
	conf.MustLoad(*configFile, &c)

	// trace_id / span_id 靠这个钩子进每条日志，日志与链路由此关联。
	c.Log.Extractors = []log.Extractor{telemetry.TraceAttrs}
	// LevelVar 让级别能在运行期调（生产临时开 debug 不必重启、不丢现场）。
	// 调它的入口由服务自己决定（管理端点、信号等），这里只是把它接上。
	c.Log.LevelVar = new(slog.LevelVar)
	logger := log.New(c.Log)

	// 根 ctx 由 cmd 统一给出并监听信号；pkg/app 只对 ctx 取消做反应，不自己监听。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go forceExitOnSecondSignal(logger)

	if err := initial.App(ctx, c, logger); err != nil {
		stop()
		if errors.Is(err, app.ErrStopTimeout) {
			logger.Warn("停机未在宽限期内完成", "err", err)
			os.Exit(exitStopTimeout)
		}
		logger.Error("服务异常退出", "err", err)
		os.Exit(exitRunFailure)
	}
}

// forceExitOnSecondSignal 给运维一个逃生口：第二个停机信号即放弃排空、立刻退出。
//
// signal.NotifyContext 收到首个信号后只 cancel ctx，它自己的监听随即退出，而
// signal.Notify 的注册仍在、进程的默认信号行为已被禁用——于是后续信号被全部
// 丢弃：Ctrl-C 按第二下没反应，生产上运维在 TERM 与 KILL 之间补发的 TERM 同样
// 无效，只能干等停机预算跑完或 kill -9。
func forceExitOnSecondSignal(logger *slog.Logger) {
	again := make(chan os.Signal, 2)
	signal.Notify(again, syscall.SIGINT, syscall.SIGTERM)
	<-again // 第一个信号已由 NotifyContext 处理，这里只是跳过它
	<-again
	logger.Warn("收到第二个停机信号，放弃排空直接退出")
	os.Exit(exitForced)
}
