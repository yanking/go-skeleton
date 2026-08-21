package main

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/pkg/conf"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/mysql"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

// TestConfigBindsRealFile 用仓库里真实的 configs/user.yaml 跑一遍绑定。
// 它盯的是「YAML 键与结构体字段漂移」——曾经 mysql 的 ConnMaxIdleTime 只存在于
// pkg/mysql.Config 而不在服务配置里，那个旋钮从 YAML 根本配不到，且无人报错。
func TestConfigBindsRealFile(t *testing.T) {
	var cfg Config
	conf.MustLoad("../../configs/user.yaml", &cfg)

	// 编译期锁住「各段直接就是 pkg 的 Config 类型」——谁重新引入副本，这行过不去。
	_, _, _ = pkgConfigTypes(cfg)

	tests := []struct {
		name string
		got  any
		want any
	}{
		{name: "server.grpc_addr", got: cfg.Server.GRPCAddr, want: ":9090"},
		{name: "server.serve_docs", got: cfg.Server.ServeDocs, want: true},
		{name: "log.level", got: cfg.Log.Level, want: slog.LevelInfo},
		{name: "log.format", got: cfg.Log.Format, want: log.FormatJSON},
		{name: "telemetry.exporter", got: cfg.Telemetry.Exporter, want: telemetry.ExporterNone},
		{name: "mysql.max_open_conns", got: cfg.MySQL.MaxOpenConns, want: 25},
		{name: "mysql.conn_max_lifetime", got: cfg.MySQL.ConnMaxLifetime, want: 30 * time.Minute},
		{name: "stop_timeout", got: cfg.StopTimeout, want: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %v, want %v", tt.got, tt.want)
			}
		})
	}
}

// TestInjectedFieldsAreTaggedNotBindable 注入型字段——接口、指针、函数切片——必须标
// yaml:"-"。漏标不会有任何提示：yaml 会把字段名小写化当成一个合法键，配置文件里恰好
// 写了同名键就得到一条莫名其妙的类型错误，而不是「未知键」。这条用例按类型扫全部
// pkg Config，新增字段时漏标会当场失败。
func TestInjectedFieldsAreTaggedNotBindable(t *testing.T) {
	// injected 判定「只能在 Go 里注入、不可能来自 YAML」的字段类型。
	injected := func(t reflect.Type) bool {
		switch t.Kind() {
		case reflect.Interface, reflect.Pointer, reflect.Func:
			return true
		case reflect.Slice:
			return t.Elem().Kind() == reflect.Func
		default:
			return false
		}
	}

	for _, cfg := range []any{mysql.Config{}, telemetry.Config{}, log.Config{}} {
		typ := reflect.TypeOf(cfg)
		t.Run(typ.String(), func(t *testing.T) {
			for i := range typ.NumField() {
				f := typ.Field(i)
				if !injected(f.Type) {
					continue
				}
				if got := f.Tag.Get("yaml"); got != "-" {
					t.Errorf("%s 是注入型字段（%s），yaml tag got %q, want \"-\"", f.Name, f.Type, got)
				}
			}
		})
	}
}

// pkgConfigTypes 只为编译期类型检查而存在：返回类型写死成各 pkg 的 Config，
// 若哪天服务配置又改回逐字段抄写的副本，这里会编译失败。
func pkgConfigTypes(c Config) (mysql.Config, telemetry.Config, log.Config) {
	return c.MySQL, c.Telemetry, c.Log
}
