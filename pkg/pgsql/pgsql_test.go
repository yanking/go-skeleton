package pgsql

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"gorm.io/gorm"
)

// validDSN 合规 DSN：两个超时都是 Config.Validate 的硬要求。
const validDSN = "postgres://user:pass@127.0.0.1:5432/app?sslmode=disable&connect_timeout=5&statement_timeout=10000"

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "write 缺失", cfg: Config{}, wantErr: true},
		{name: "read 含空 DSN", cfg: Config{Write: validDSN, Read: []string{""}}, wantErr: true},
		{name: "池参数为负", cfg: Config{Write: validDSN, ConnMaxLifetime: -1}, wantErr: true},
		{name: "合法配置", cfg: Config{Write: validDSN, Read: []string{validDSN}}, wantErr: false},
		// 以下三条守住「不配就出事、而且出事很晚」的 DSN 形态要求。
		{name: "缺建连超时", cfg: Config{Write: "postgres://u:p@h:5432/app?sslmode=disable&statement_timeout=10000"}, wantErr: true},
		{name: "缺语句超时", cfg: Config{Write: "postgres://u:p@h:5432/app?sslmode=disable&connect_timeout=5"}, wantErr: true},
		{name: "副本 DSN 形态同样要查", cfg: Config{Write: validDSN, Read: []string{"postgres://u:p@h:5432/app?sslmode=disable"}}, wantErr: true},
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
		{name: "write DSN 非法", cfg: Config{Write: "postgres://user:pass@127.0.0.1:5432/app?sslmode=unknown"}},
		{name: "read DSN 非法", cfg: Config{Write: validDSN, Read: []string{"postgres://u:p@h:5432/app?sslmode=unknown"}}},
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

// 连接池惰性建连、postgres dialector 的 Conn 路径不查询版本，全程不触网。
func TestComponentLifecycle(t *testing.T) {
	db := New(Config{Write: validDSN, Read: []string{validDSN}})

	if name := db.Name(); name != "pgsql" {
		t.Errorf("Name got %q, want %q", name, "pgsql")
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
			// 三条分支都要记 SQL：失败分支尤其需要——死锁、约束冲突、语句超时时
			// 只说「有条 SQL 失败了」等于没说。SQL 已由 ParamsFilter 脱敏。
			if !strings.Contains(buf.String(), "sql=") {
				t.Errorf("该分支应记 SQL，got %q", buf.String())
			}
		})
	}
}

// TestPoolLimitsDefaults 钉住零值取保守缺省，而不是把 0 原样传给 database/sql 的
// Setter——SetMaxIdleConns(0) 的真实语义是「一条空闲连接都不留」（标准库里 n > 0
// 才采用，否则置 -1），于是每条 SQL 都要重新握手、认证、用完即关。
func TestPoolLimitsDefaults(t *testing.T) {
	tests := []struct {
		name         string
		cfg          Config
		wantOpen     int
		wantIdle     int
		wantLifetime time.Duration
		wantIdleTime time.Duration
	}{
		{
			name: "全零值取缺省", cfg: Config{},
			wantOpen: defaultMaxOpenConns, wantIdle: defaultMaxIdleConns,
			wantLifetime: defaultConnMaxLifetime, wantIdleTime: defaultConnMaxIdleTime,
		},
		{
			name:     "显式值优先",
			cfg:      Config{MaxOpenConns: 50, MaxIdleConns: 7, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Second},
			wantOpen: 50, wantIdle: 7, wantLifetime: time.Minute, wantIdleTime: time.Second,
		},
		{
			name: "空闲上限被夹进打开上限", cfg: Config{MaxOpenConns: 3, MaxIdleConns: 99},
			wantOpen: 3, wantIdle: 3,
			wantLifetime: defaultConnMaxLifetime, wantIdleTime: defaultConnMaxIdleTime,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			maxOpen, maxIdle, lifetime, idleTime := poolLimits(tt.cfg)
			if maxOpen != tt.wantOpen || maxIdle != tt.wantIdle {
				t.Errorf("连接数 got (open=%d idle=%d), want (open=%d idle=%d)",
					maxOpen, maxIdle, tt.wantOpen, tt.wantIdle)
			}
			if lifetime != tt.wantLifetime || idleTime != tt.wantIdleTime {
				t.Errorf("时长 got (lifetime=%v idle=%v), want (lifetime=%v idle=%v)",
					lifetime, idleTime, tt.wantLifetime, tt.wantIdleTime)
			}
			if maxIdle == 0 {
				t.Error("空闲上限不能是 0：那等于不保留空闲连接，每条 SQL 都要重新建连")
			}
		})
	}
}

// TestParamsFilterDropsValues 钉住 SQL 日志的脱敏：参数必须被丢掉，GORM 才会把
// 参数化原文交给 Trace。放它过去的话，慢 SQL（Warn 级，生产一定放行）会把
// 手机号、token、散列前的口令一并写进日志平台，而库一变慢时这类日志量最大。
func TestParamsFilterDropsValues(t *testing.T) {
	const query = "SELECT * FROM users WHERE email = ? AND token = ?"
	got, vars := gormLogger{}.ParamsFilter(context.Background(), query, "a@example.com", "sk-live-SECRET")
	if got != query {
		t.Errorf("SQL 原文不应被改动，got %q want %q", got, query)
	}
	if vars != nil {
		t.Errorf("参数必须丢弃，got %v", vars)
	}
}

// TestGormLoggerSkipsFcWhenDebugFiltered 钉住热路径开销：Debug 被级别滤掉时不调 fc，
// 它要拼一遍 SQL 字符串而结果没人要，而这是每条语句都会走的路径。
func TestGormLoggerSkipsFcWhenDebugFiltered(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	called := false
	gormLogger{logger: logger}.Trace(context.Background(), time.Now(),
		func() (string, int64) { called = true; return "SELECT 1", 0 }, nil)

	if called {
		t.Error("Debug 被滤掉时不应调用 fc")
	}
	if buf.Len() != 0 {
		t.Errorf("Debug 被滤掉时不应有输出，got %q", buf.String())
	}
}

// TestPoolMetricsRegistration 钉住两件事：注入 MeterProvider 后每个池都注册了池指标
// （饱和度是 DB 变慢时最先要看的那组数），没注入时记一条装配日志——「指标静默消失」
// 必须在启动日志里看得见，否则等排障时才发现没数据就太晚了。
func TestPoolMetricsRegistration(t *testing.T) {
	t.Run("注入后每池一个注册", func(t *testing.T) {
		db := New(Config{Write: validDSN, Read: []string{validDSN}, MeterProvider: sdkmetric.NewMeterProvider()})
		t.Cleanup(func() { _ = db.Stop(context.Background()) })

		if got := len(db.metricRegs); got != 2 {
			t.Errorf("注册数 got %d, want 2（主库 + 1 副本）", got)
		}
	})

	t.Run("未注入时记装配日志", func(t *testing.T) {
		var buf bytes.Buffer
		db := New(Config{Write: validDSN, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
		t.Cleanup(func() { _ = db.Stop(context.Background()) })

		if got := len(db.metricRegs); got != 0 {
			t.Errorf("未注入 MeterProvider 不应有注册，got %d", got)
		}
		if !strings.Contains(buf.String(), "指标未启用") {
			t.Errorf("应记一条「指标未启用」装配日志，got %q", buf.String())
		}
	})
}
