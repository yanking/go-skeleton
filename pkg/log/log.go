// Package log 构造服务统一的 slog.Logger：结构化输出、级别与格式经配置可调，
// 并支持用 Extractor 从 ctx 提取 trace_id 一类字段自动附加到每条日志。
//
// New 在构造的同时接管全局默认 Logger（slog.SetDefault），第三方库经 slog
// 打的日志也走同一 Handler——服务进程一个日志出口。代价是引入全局可变状态，
// 多 Logger 场景（如测试）需要调用方自行保存/还原 slog.Default()。
package log

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
)

// New 按 cfg 构造 Logger。配置非法（Service 为空、Format 不认识）直接 panic——
// 这些都是写错了才会发生的装配期错误，日志起不来的进程不该继续跑。
func New(cfg Config) *slog.Logger {
	if cfg.Service == "" {
		panic(errors.New("构造 Logger: Service 不能为空"))
	}

	w := cfg.Writer
	if w == nil {
		w = os.Stdout
	}
	opts := &slog.HandlerOptions{Level: cfg.Level, AddSource: cfg.AddSource}

	var handler slog.Handler
	switch cfg.Format {
	case "", FormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	case FormatText:
		handler = slog.NewTextHandler(w, opts)
	default:
		panic(fmt.Errorf("构造 Logger: 未知日志格式 %q，只支持 %q 与 %q", cfg.Format, FormatJSON, FormatText))
	}

	handler = &ctxHandler{Handler: handler, extractors: cfg.Extractors}
	log := slog.New(handler).With(slog.String("service", cfg.Service))
	slog.SetDefault(log)

	return log
}
