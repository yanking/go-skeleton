package main

import (
	"errors"
	"log/slog"
	"time"

	"github.com/yanking/go-skeleton/internal/user/server"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

// Config user 服务的完整配置，由 configs/user.yaml 绑定。
// 各字段类型直接用对应 pkg 的类型，绑定阶段就能挡住拼写错误。
type Config struct {
	Server      server.Config   `yaml:"server"`
	Log         logConfig       `yaml:"log"`
	Telemetry   telemetryConfig `yaml:"telemetry"`
	MySQL       mysqlConfig     `yaml:"mysql"`
	StopTimeout time.Duration   `yaml:"stop_timeout"`
}

type logConfig struct {
	Level     slog.Level `yaml:"level"`
	Format    log.Format `yaml:"format"`
	AddSource bool       `yaml:"add_source"`
}

type telemetryConfig struct {
	Exporter    telemetry.Exporter `yaml:"exporter"`
	Endpoint    string             `yaml:"endpoint"`
	Insecure    bool               `yaml:"insecure"`
	SampleRatio float64            `yaml:"sample_ratio"`
	Env         string             `yaml:"env"`
}

type mysqlConfig struct {
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	ConnectTimeout  time.Duration `yaml:"connect_timeout"`
}

// Validate 由 conf.MustLoad 在绑定后自动调用，失败即 panic——起不来就死。
func (c *Config) Validate() error {
	switch {
	case c.Server.GRPCAddr == "":
		return errors.New("server.grpc_addr 必填")
	case c.Server.HTTPAddr == "":
		return errors.New("server.http_addr 必填")
	case c.MySQL.DSN == "":
		return errors.New("mysql.dsn 必填")
	}
	return nil
}
