package mysql

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

const validDSN = "user:pass@tcp(127.0.0.1:3306)/app?parseTime=true"

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "write 缺失", cfg: Config{}, wantErr: true},
		{name: "read 含空 DSN", cfg: Config{Write: validDSN, Read: []string{""}}, wantErr: true},
		{name: "池参数为负", cfg: Config{Write: validDSN, MaxOpenConns: -1}, wantErr: true},
		{name: "合法配置", cfg: Config{Write: validDSN, Read: []string{validDSN}}, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate err got %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewPanics(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "write 为空", cfg: Config{}},
		{name: "write DSN 非法", cfg: Config{Write: "not a dsn"}},
		{name: "read DSN 非法", cfg: Config{Write: validDSN, Read: []string{"bad"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("want panic, got 正常返回")
				}
			}()
			New(tt.cfg)
		})
	}
}

// 连接池惰性建连、GORM 跳过版本探测，New/Start/Stop 全程不触网，可离线验证组件契约。
func TestComponentLifecycle(t *testing.T) {
	db := New(Config{Write: validDSN, Read: []string{validDSN, validDSN}})

	if name := db.Name(); name != "mysql" {
		t.Errorf("Name got %q, want %q", name, "mysql")
	}
	if err := db.Start(context.Background()); err != nil {
		t.Errorf("Start got %v, want nil", err)
	}
	if err := db.Stop(context.Background()); err != nil {
		t.Errorf("Stop got %v, want nil", err)
	}
}

// 读写分离的路由行为由 dbresolver 自身保证（上游有测试），此处只锁装配结构：
// 配了副本才注册 resolver，未配则不注册、全部语句走主库。
func TestResolverWiring(t *testing.T) {
	withReplicas := New(Config{Write: validDSN, Read: []string{validDSN}})
	if _, ok := withReplicas.Plugins["gorm:db_resolver"]; !ok {
		t.Error("配了副本应注册 dbresolver 插件")
	}

	primaryOnly := New(Config{Write: validDSN})
	if _, ok := primaryOnly.Plugins["gorm:db_resolver"]; ok {
		t.Error("未配副本不应注册 dbresolver 插件")
	}
}

func TestGormLoggerTrace(t *testing.T) {
	tests := []struct {
		name      string
		begin     time.Time
		err       error
		wantLevel string
	}{
		{name: "失败进 Error", begin: time.Now(), err: errors.New("boom"), wantLevel: "ERROR"},
		{name: "空结果不算错", begin: time.Now(), err: gorm.ErrRecordNotFound, wantLevel: "DEBUG"},
		{name: "慢查询进 Warn", begin: time.Now().Add(-slowSQLThreshold - time.Second), wantLevel: "WARN"},
		{name: "常规进 Debug", begin: time.Now(), wantLevel: "DEBUG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			gormLogger{logger: logger}.Trace(context.Background(), tt.begin,
				func() (string, int64) { return "SELECT 1", 0 }, tt.err)

			if !strings.Contains(buf.String(), "level="+tt.wantLevel) {
				t.Errorf("want level=%s, got %q", tt.wantLevel, buf.String())
			}
		})
	}
}
