package mysql_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/mysql"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// dsnOrSkip 取集成测试用的 DSN，没配就跳过——CI 基线里没有数据库。
// 本地跑法见 .agent/engineering.md「集成测试」。
func dsnOrSkip(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 MYSQL_DSN，跳过需要真实 MySQL 的用例")
	}
	return dsn
}

func TestMustNewPanicsOnInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  mysql.Config
	}{
		{name: "DSN 为空", cfg: mysql.Config{}},
		{name: "DSN 语法不对", cfg: mysql.Config{DSN: "这不是一个 DSN"}},
		{
			// database/sql 的 Ping 完全不重试，连不上必须由本包重试到窗口耗尽再 panic，
			// 而不是「启动成功、第一个请求才 500」。
			name: "连不上时重试到超时",
			cfg: mysql.Config{
				DSN:            "root:secret@tcp(127.0.0.1:1)/skeleton",
				ConnectTimeout: 300 * time.Millisecond,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("want panic, got 正常返回")
				}
			}()
			tt.cfg.Logger = discardLogger()
			mysql.MustNew(context.Background(), tt.cfg)
		})
	}
}

// Client 必须满足 pkg/app.Component，否则 cmd 无法把它注册进 app。
var _ app.Component = (*mysql.Client)(nil)

func TestConnectTimeoutIsRespected(t *testing.T) {
	// database/sql 的 Ping 不重试，连接被拒是立刻返回的，
	// 所以「总耗时接近 ConnectTimeout」直接证明本包在重试。
	const timeout = 800 * time.Millisecond
	start := time.Now()
	func() {
		defer func() { _ = recover() }()
		mysql.MustNew(context.Background(), mysql.Config{
			DSN:            "root:secret@tcp(127.0.0.1:1)/skeleton",
			ConnectTimeout: timeout,
			Logger:         discardLogger(),
		})
	}()

	if elapsed := time.Since(start); elapsed < timeout {
		t.Errorf("应重试满 %v 才放弃, 实际只用了 %v", timeout, elapsed)
	}
}

func TestClientAgainstRealMySQL(t *testing.T) {
	dsn := dsnOrSkip(t)
	ctx := context.Background()
	c := mysql.MustNew(ctx, mysql.Config{DSN: dsn, MaxOpenConns: 7, Logger: discardLogger()})

	if got, want := c.Name(), "mysql"; got != want {
		t.Errorf("Name got %q, want %q", got, want)
	}
	if err := c.Start(ctx); err != nil {
		t.Errorf("Start 返回错误: %v", err)
	}
	if got, want := c.Stats().MaxOpenConnections, 7; got != want {
		t.Errorf("池上限未生效, got %d, want %d", got, want)
	}

	// 查询直接调用，无需先取内嵌字段——这是内嵌 *sql.DB 的目的。
	var one int
	if err := c.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1 失败: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 got %d, want 1", one)
	}

	if err := c.Stop(ctx); err != nil {
		t.Errorf("Stop 返回错误: %v", err)
	}
	if err := c.PingContext(ctx); err == nil {
		t.Error("Stop 之后连接池应已关闭")
	}
}

func TestTelemetryProducesQuerySpan(t *testing.T) {
	dsn := dsnOrSkip(t)
	var spans bytes.Buffer
	ctx := context.Background()
	tel := telemetry.MustNew(ctx, telemetry.Config{
		Service:  "user",
		Exporter: telemetry.ExporterStdout,
		Writer:   &spans,
		Logger:   discardLogger(),
	})

	c := mysql.MustNew(ctx, mysql.Config{DSN: dsn, Telemetry: tel, Logger: discardLogger()})
	var one int
	if err := c.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1 失败: %v", err)
	}

	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := tel.Stop(ctx); err != nil {
		t.Fatalf("关闭 telemetry: %v", err)
	}

	if got := spans.String(); !strings.Contains(got, "sql.conn.query") {
		t.Errorf("未见 SQL 查询 span\n实际输出:\n%s", got)
	}
}
