package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yanking/go-skeleton/pkg/log"
)

// decodeJSON 把缓冲区里的 JSON 日志逐行解析出来，并丢掉每条都会变的 time 字段。
func decodeJSON(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("解析日志行 %q: %v", line, err)
		}
		delete(record, "time")
		records = append(records, record)
	}
	return records
}

func TestMustNewWritesJSONWithService(t *testing.T) {
	var buf bytes.Buffer
	logger := log.MustNew(log.Config{Service: "user", Writer: &buf})

	logger.Info("已启动", "port", 8080)

	got := decodeJSON(t, &buf)
	want := []map[string]any{{
		"level":   "INFO",
		"msg":     "已启动",
		"service": "user",
		"port":    float64(8080),
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("日志不符 (-want +got):\n%s", diff)
	}
}

func TestMustNewTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := log.MustNew(log.Config{Service: "user", Format: log.FormatText, Writer: &buf})

	logger.Info("已启动")

	got := strings.TrimSpace(buf.String())
	if strings.HasPrefix(got, "{") {
		t.Fatalf("Format=text 时不应输出 JSON, got %q", got)
	}
	for _, want := range []string{"level=INFO", "msg=已启动", "service=user"} {
		if !strings.Contains(got, want) {
			t.Errorf("text 输出缺少 %q, got %q", want, got)
		}
	}
}

func TestMustNewPanicsOnInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  log.Config
	}{
		{name: "Service 为空", cfg: log.Config{}},
		{name: "Format 不认识", cfg: log.Config{Service: "user", Format: "jsno"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("want panic, got 正常返回")
				}
			}()
			tt.cfg.Writer = io.Discard
			log.MustNew(tt.cfg)
		})
	}
}

// messages 取出每条日志的 msg，用于只关心「哪几条被放行」的用例。
func messages(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var msgs []string
	for _, record := range decodeJSON(t, buf) {
		msg, ok := record["msg"].(string)
		if !ok {
			t.Fatalf("日志缺少 msg 字段: %v", record)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestMustNewLevel(t *testing.T) {
	tests := []struct {
		name  string
		level slog.Leveler
		want  []string
	}{
		{name: "nil 取默认 Info", level: nil, want: []string{"info 级", "warn 级"}},
		{name: "Debug 全放行", level: slog.LevelDebug, want: []string{"debug 级", "info 级", "warn 级"}},
		{name: "Warn 滤掉 Info", level: slog.LevelWarn, want: []string{"warn 级"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.MustNew(log.Config{Service: "user", Level: tt.level, Writer: &buf})

			logger.Debug("debug 级")
			logger.Info("info 级")
			logger.Warn("warn 级")

			if diff := cmp.Diff(tt.want, messages(t, &buf)); diff != "" {
				t.Errorf("放行的日志不符 (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLevelVarAdjustsLevelAtRuntime(t *testing.T) {
	var buf bytes.Buffer
	level := new(slog.LevelVar) // 零值即 Info
	logger := log.MustNew(log.Config{Service: "user", Level: level, Writer: &buf})

	logger.Debug("调级前")
	level.Set(slog.LevelDebug)
	logger.Debug("调级后")

	want := []string{"调级后"}
	if diff := cmp.Diff(want, messages(t, &buf)); diff != "" {
		t.Errorf("放行的日志不符 (-want +got):\n%s", diff)
	}
}

func TestMustNewAddSource(t *testing.T) {
	tests := []struct {
		name       string
		addSource  bool
		wantSource bool
	}{
		{name: "默认不带调用点", addSource: false, wantSource: false},
		{name: "开启后带调用点", addSource: true, wantSource: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.MustNew(log.Config{Service: "user", AddSource: tt.addSource, Writer: &buf})

			logger.Info("已启动")

			source, ok := decodeJSON(t, &buf)[0]["source"].(map[string]any)
			if ok != tt.wantSource {
				t.Fatalf("source 字段存在性 got %v, want %v", ok, tt.wantSource)
			}
			if !tt.wantSource {
				return
			}
			if file, _ := source["file"].(string); !strings.HasSuffix(file, "log_test.go") {
				t.Errorf("source.file 应指向调用点, got %v", source["file"])
			}
		})
	}
}

type traceKey struct{}
type userKey struct{}

// stringAttr 造一个从 ctx 取字符串的 Extractor，取不到就不加字段。
func stringAttr(key any, name string) log.Extractor {
	return func(ctx context.Context) []slog.Attr {
		value, ok := ctx.Value(key).(string)
		if !ok {
			return nil
		}
		return []slog.Attr{slog.String(name, value)}
	}
}

func TestExtractorsAddAttrsFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := log.MustNew(log.Config{
		Service: "user",
		Writer:  &buf,
		Extractors: []log.Extractor{
			stringAttr(traceKey{}, "trace_id"),
			stringAttr(userKey{}, "user_id"),
		},
	})

	ctx := context.WithValue(context.Background(), traceKey{}, "abc123")
	ctx = context.WithValue(ctx, userKey{}, "u-7")
	logger.InfoContext(ctx, "带上下文")
	logger.InfoContext(context.Background(), "空上下文")

	want := []map[string]any{
		{"level": "INFO", "msg": "带上下文", "service": "user", "trace_id": "abc123", "user_id": "u-7"},
		{"level": "INFO", "msg": "空上下文", "service": "user"},
	}
	if diff := cmp.Diff(want, decodeJSON(t, &buf)); diff != "" {
		t.Errorf("日志不符 (-want +got):\n%s", diff)
	}
}

func TestExtractorsSurviveWithAndWithGroup(t *testing.T) {
	tests := []struct {
		name   string
		derive func(*slog.Logger) *slog.Logger
		want   map[string]any
	}{
		{
			name:   "With 派生后仍生效",
			derive: func(l *slog.Logger) *slog.Logger { return l.With("method", "Create") },
			want: map[string]any{
				"level": "INFO", "msg": "处理中", "service": "user",
				"method": "Create", "trace_id": "abc123",
			},
		},
		{
			// WithGroup 之后 ctx 字段落进组内，这是 slog.Record.AddAttrs 的固有语义，
			// 此处锁住该行为以免将来被误当成 bug「修掉」。
			name:   "WithGroup 派生后仍生效，字段落进组内",
			derive: func(l *slog.Logger) *slog.Logger { return l.WithGroup("req") },
			want: map[string]any{
				"level": "INFO", "msg": "处理中", "service": "user",
				"req": map[string]any{"trace_id": "abc123"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.MustNew(log.Config{
				Service:    "user",
				Writer:     &buf,
				Extractors: []log.Extractor{stringAttr(traceKey{}, "trace_id")},
			})

			ctx := context.WithValue(context.Background(), traceKey{}, "abc123")
			tt.derive(logger).InfoContext(ctx, "处理中")

			if diff := cmp.Diff(tt.want, decodeJSON(t, &buf)[0]); diff != "" {
				t.Errorf("日志不符 (-want +got):\n%s", diff)
			}
		})
	}
}

func TestExtractorsSkippedWhenLevelFiltered(t *testing.T) {
	calls := 0
	logger := log.MustNew(log.Config{
		Service: "user",
		Level:   slog.LevelInfo,
		Writer:  io.Discard,
		Extractors: []log.Extractor{func(context.Context) []slog.Attr {
			calls++
			return nil
		}},
	})

	logger.DebugContext(context.Background(), "被级别滤掉")
	if calls != 0 {
		t.Errorf("被滤掉的日志不该调用 Extractor, got calls=%d, want 0", calls)
	}

	logger.InfoContext(context.Background(), "放行")
	if calls != 1 {
		t.Errorf("放行的日志应调用 Extractor 一次, got calls=%d, want 1", calls)
	}
}

func TestFormatUnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    log.Format
		wantErr bool
	}{
		{name: "json", text: "json", want: log.FormatJSON},
		{name: "text", text: "text", want: log.FormatText},
		{name: "空串归一到默认 json", text: "", want: log.FormatJSON},
		{name: "拼错当场报错", text: "jsno", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got log.Format
			err := got.UnmarshalText([]byte(tt.text))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err got %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("Format got %q, want %q", got, tt.want)
			}
		})
	}
}
