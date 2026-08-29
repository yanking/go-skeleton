package redis

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// slowCmdThreshold 慢命令判定阈值，与 pkg/mysql、pkg/pgsql 的慢 SQL 阈值一致。
const slowCmdThreshold = 200 * time.Millisecond

// plumbingCmds 是 go-redis 握手期的内部命令。服务端不支持时（如 Redis < 7.2
// 或嵌入式测试实现）客户端会拿到报错并自行忽略——它们失败属已知噪音，
// 不按业务错误上报，降级进 Debug。
var plumbingCmds = map[string]bool{
	"hello": true, "client": true, "ping": true, "setinfo": true, "echo": true,
}

func isPlumbing(name string) bool { return plumbingCmds[strings.ToLower(name)] }

// cmdLogger 经 go-redis Hook 上报命令日志。只记命令名不记参数：
// 参数可能含敏感值，定位靠 trace_id 关联即可。
type cmdLogger struct {
	logger *slog.Logger
}

func (cmdLogger) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (l cmdLogger) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		begin := time.Now()
		err := next(ctx, cmd)
		l.log(ctx, cmd.Name(), time.Since(begin), err, 1, isPlumbing(cmd.Name()))
		return err
	}
}

func (l cmdLogger) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		begin := time.Now()
		err := next(ctx, cmds)
		allPlumbing := true
		for _, cmd := range cmds {
			if !isPlumbing(cmd.Name()) {
				allPlumbing = false
				break
			}
		}
		l.log(ctx, "pipeline", time.Since(begin), err, len(cmds), allPlumbing)
		return err
	}
}

// log 按结果分级上报一条命令：业务失败进 Error、超阈值进 Warn、其余进 Debug
// （握手期内部命令失败不按业务错误上报）。放行级别统一由 slog 控制。
func (l cmdLogger) log(ctx context.Context, name string, elapsed time.Duration, err error, count int, plumbing bool) {
	switch {
	case err != nil && !errors.Is(err, goredis.Nil) && !plumbing:
		l.logger.ErrorContext(ctx, "命令执行失败", "name", name, "err", err, "elapsed", elapsed.String())
	case elapsed > slowCmdThreshold:
		l.logger.WarnContext(ctx, "慢命令", "name", name, "n", count, "elapsed", elapsed.String())
	default:
		l.logger.DebugContext(ctx, "命令", "name", name, "n", count, "elapsed", elapsed.String())
	}
}
