package log

import (
	"fmt"
	"io"
	"log/slog"
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
// （conf.MustLoad）就暴露，与 slog.Level 的行为一致，而不必等到 New 才 panic。
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
// 标了 yaml 键的字段可由配置文件绑定；yaml:"-" 的是装配期注入项，只能在 Go 里给。
type Config struct {
	// Service 服务名，写入每条日志的 service 字段；必填，为空即 panic。
	Service string `yaml:"name"`
	// Level 放行级别，零值即 slog.LevelInfo。slog.Level 实现了 encoding.TextUnmarshaler，
	// 故配置文件里直接写 debug / info / warn / error（大小写不敏感，还支持 info+2 偏移）。
	// 用具体类型而非 slog.Leveler，是为了让本 Config 能直接作 YAML 绑定目标——接口绑不进去。
	// 将来要运行期调级，另加一个 yaml:"-" 的 *slog.LevelVar 字段让它优先即可，不必动这里。
	Level slog.Level `yaml:"level"`
	// Format 输出格式，零值取 FormatJSON。
	Format Format `yaml:"format"`
	// AddSource 是否附带调用点的文件与行号，默认关。
	AddSource bool `yaml:"add_source"`
	// Writer 输出目标，nil 时用 os.Stdout。
	Writer io.Writer `yaml:"-"`
	// Extractors 从 ctx 提取动态字段的钩子，每条日志按序调用；契约见 Extractor。
	Extractors []Extractor `yaml:"-"`
}
