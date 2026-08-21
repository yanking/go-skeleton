// Package log 构造服务统一的 slog.Logger：结构化输出、级别与格式经配置可调，
// 并支持用 Extractor 从 ctx 提取 trace_id 一类字段自动附加到每条日志。
//
// 本包只造 Logger，不调 slog.SetDefault——是否接管全局默认 Logger 由 cmd 决定。
// 接管的好处是第三方库经 slog 打的日志也走同一 Handler，代价是引入全局可变状态。
package log

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// Format 日志输出格式。
type Format string

const (
	// FormatJSON 一行一条 JSON，Format 零值即此项。
	FormatJSON Format = "json"
	// FormatText key=value 文本，便于本地开发肉眼阅读。
	FormatText Format = "text"
)

// UnmarshalText 实现 encoding.TextUnmarshaler，使配置文件里的格式拼写错误在绑定阶段
// （conf.MustLoad）就暴露，与 slog.Level 的行为一致，而不必等到 MustNew 才 panic。
// 空串归一到 FormatJSON，与 Format 零值的含义保持一致。
func (f *Format) UnmarshalText(text []byte) error {
	switch format := Format(text); format {
	case "", FormatJSON:
		*f = FormatJSON
	case FormatText:
		*f = FormatText
	default:
		return fmt.Errorf("未知日志格式 %q，只支持 %q 与 %q", format, FormatJSON, FormatText)
	}
	return nil
}

// Config 日志构造参数，除 Service 外零值均有合理默认。
type Config struct {
	// Service 服务名，写入每条日志的 service 字段；必填，为空即 panic。
	Service string
	// Level 放行级别，nil 取 slog.LevelInfo。
	// 传 *slog.LevelVar 即可在运行期调级，该变量由调用方持有，本包不代管。
	Level slog.Leveler
	// Format 输出格式，零值取 FormatJSON。
	Format Format
	// AddSource 是否附带调用点的文件与行号，默认关。
	AddSource bool
	// Writer 输出目标，nil 时用 os.Stdout。
	Writer io.Writer
	// Extractors 从 ctx 提取动态字段的钩子，每条日志按序调用；契约见 Extractor。
	Extractors []Extractor
}

// MustNew 按 cfg 构造 Logger。配置非法（Service 为空、Format 不认识）直接 panic——
// 这些都是写错了才会发生的装配期错误，日志起不来的进程不该继续跑。
func MustNew(cfg Config) *slog.Logger {
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
	return slog.New(handler).With(slog.String("service", cfg.Service))
}
