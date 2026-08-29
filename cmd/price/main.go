// Command price 是 price 服务的装配根：读配置、构造 Logger 与根 ctx，
// 组件装配交给 initial，启停编排交给 pkg/app。
//
// 本仓第一个子命令形态的服务：无参数 = 常驻采集（make run SVC=price、容器
// CMD 都不带参数，必须是常驻，不能是打印用法），另有 instruments、backfill
// 两个跑完即退出的一次性子命令，各自解析自己的 flag。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yanking/go-skeleton/cmd/price/initial"
	"github.com/yanking/go-skeleton/internal/price/config"
	"github.com/yanking/go-skeleton/pkg/conf"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

func main() {
	// 无参 = 常驻采集；子命令各自解析自己的 flag。
	// 这是本仓第一个子命令形态的服务：make run SVC=price 不带参数，
	// 因此「无参」必须是常驻模式，不能是打印用法。
	args := os.Args[1:]
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "":
		runDaemon(args)
	case "instruments":
		runInstruments(args)
	case "backfill":
		runBackfill(args)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q；可用：instruments、backfill，或不带参数常驻采集\n", sub)
		os.Exit(2)
	}
}

// loadConfig 读取 -config 指定的配置文件并构造带 trace 钩子的 logger；三个
// 子命令共用同一套装配前置动作，避免各自漏写、悄悄跑偏。
func loadConfig(configFile string) (config.Config, *slog.Logger) {
	var c config.Config
	conf.MustLoad(configFile, &c)

	// trace_id / span_id 靠这个钩子进每条日志，日志与链路由此关联；两个一次性
	// 子命令没有 telemetry 组件（不建 TracerProvider），钩子本身无害地不产出
	// 任何字段。
	c.Log.Extractors = []log.Extractor{telemetry.TraceAttrs}
	return c, log.New(c.Log)
}

// runDaemon 常驻采集，直到收到 SIGINT/SIGTERM 优雅停机。
func runDaemon(args []string) {
	fs := flag.NewFlagSet("price", flag.ExitOnError)
	configFile := fs.String("config", "./configs/price.yaml", "配置文件路径")
	fs.Parse(args)

	c, logger := loadConfig(*configFile)

	// 根 ctx 由 cmd 统一给出并监听信号；pkg/app 只对 ctx 取消做反应，不自己监听。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := initial.App(ctx, c, logger); err != nil {
		logger.Error("服务异常退出", "err", err)
		stop()
		os.Exit(1)
	}
}

// runInstruments 拉取 -exchange 指定交易所的全量交易对，跑完退出。
func runInstruments(args []string) {
	fs := flag.NewFlagSet("instruments", flag.ExitOnError)
	configFile := fs.String("config", "./configs/price.yaml", "配置文件路径")
	exchangeName := fs.String("exchange", "", "交易所名称（binance/okx），必填")
	fs.Parse(args)

	if *exchangeName == "" {
		fmt.Fprintln(os.Stderr, "instruments: -exchange 不能为空")
		os.Exit(2)
	}

	c, logger := loadConfig(*configFile)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := initial.Instruments(ctx, c, logger, *exchangeName); err != nil {
		logger.Error("导入交易对失败", "exchange", *exchangeName, "err", err)
		// os.Exit 跳过全部 defer，deferred 的 stop() 不会执行；显式调用一次，
		// 与 runDaemon 的既有写法一致——尽量不带着仍在注册的信号处理器退出。
		stop()
		os.Exit(1)
	}
}

// runBackfill 按显式区间补一段历史 K 线，跑完退出。
func runBackfill(args []string) {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	configFile := fs.String("config", "./configs/price.yaml", "配置文件路径")
	exchangeName := fs.String("exchange", "", "交易所名称（binance/okx），必填")
	market := fs.String("market", "spot", "市场，本期只做现货")
	symbol := fs.String("symbol", "", "交易所原生交易对符号，如 BTCUSDT，必填")
	interval := fs.String("interval", "", "K 线周期，交易所原生拼写，如 1m，必填")
	from := fs.String("from", "", "补洞起点，RFC3339 格式，如 2026-01-01T00:00:00Z，必填")
	to := fs.String("to", "", "补洞止点，RFC3339 格式，必填")
	fs.Parse(args)

	if *exchangeName == "" {
		fmt.Fprintln(os.Stderr, "backfill: -exchange 不能为空")
		os.Exit(2)
	}
	if *symbol == "" {
		fmt.Fprintln(os.Stderr, "backfill: -symbol 不能为空")
		os.Exit(2)
	}
	if *interval == "" {
		fmt.Fprintln(os.Stderr, "backfill: -interval 不能为空")
		os.Exit(2)
	}
	fromTime, err := time.Parse(time.RFC3339, *from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill: -from 解析失败（须为 RFC3339 格式）: %v\n", err)
		os.Exit(2)
	}
	toTime, err := time.Parse(time.RFC3339, *to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill: -to 解析失败（须为 RFC3339 格式）: %v\n", err)
		os.Exit(2)
	}
	if !toTime.After(fromTime) {
		fmt.Fprintln(os.Stderr, "backfill: -to 必须晚于 -from")
		os.Exit(2)
	}

	c, logger := loadConfig(*configFile)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	bfArgs := initial.BackfillArgs{
		Exchange: *exchangeName,
		Market:   *market,
		Symbol:   *symbol,
		Interval: *interval,
		From:     fromTime,
		To:       toTime,
	}
	if err := initial.Backfill(ctx, c, logger, bfArgs); err != nil {
		logger.Error("补洞失败", "exchange", *exchangeName, "err", err)
		// 理由同 runInstruments：os.Exit 跳过 defer，显式调用一次 stop()。
		stop()
		os.Exit(1)
	}
}
