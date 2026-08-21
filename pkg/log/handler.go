package log

import (
	"context"
	"log/slog"
)

// Extractor 从 ctx 提取附加字段的钩子，返回 nil 表示本条无字段可加。
//
// 实现须满足三条：不 panic、不阻塞、不在内部再打日志（会无限递归）。
// 这些由实现方保证，Handler 不做防御——Extractor 跑在日志热路径上，
// 且是装配期自己写的代码，出了问题应当场暴露，而不是静默丢字段。
type Extractor func(ctx context.Context) []slog.Attr

// ctxHandler 包装底层 Handler，为每条记录追加 Extractor 从 ctx 取到的字段。
type ctxHandler struct {
	slog.Handler
	extractors []Extractor
}

// Handle 先追加 ctx 字段再交给底层 Handler。
//
// 注意：若调用方用了 Logger.WithGroup，此处追加的字段会落进该组内而非顶层——
// 这是 slog.Record.AddAttrs 的固有语义。trace_id 等公共字段应在未开组的 Logger 上打。
func (h *ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, extract := range h.extractors {
		if attrs := extract(ctx); len(attrs) > 0 {
			r.AddAttrs(attrs...)
		}
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs 必须覆写：内嵌版本返回的是底层 Handler，包装连同 extractors 一起丢失，
// 而日志照常输出、不报任何错——ctx 字段就此静默消失。WithGroup 同理。
func (h *ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ctxHandler{Handler: h.Handler.WithAttrs(attrs), extractors: h.extractors}
}

func (h *ctxHandler) WithGroup(name string) slog.Handler {
	return &ctxHandler{Handler: h.Handler.WithGroup(name), extractors: h.extractors}
}
