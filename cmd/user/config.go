package main

import (
	"errors"
	"time"

	"github.com/yanking/go-skeleton/internal/user/server"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/mysql"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

// Config user 服务的完整配置，由 configs/user.yaml 绑定。
//
// 各段直接就是对应 pkg 的 Config 类型，不另抄副本。抄过的版本漏掉过 mysql 的
// ConnMaxIdleTime——那个旋钮因此从配置文件根本配不到，编译、lint、测试都不会响。
// 每给某个 pkg 加一个参数就要在两处同步，是这类漏字段的根源，现在从结构上消除。
//
// pkg 的 Config 里标 yaml:"-" 的字段是装配期注入项（Logger、Telemetry、Service 等），
// 绑定阶段拿不到值，由 main 在 MustLoad 之后填。
type Config struct {
	Server      server.Config    `yaml:"server"`
	Log         log.Config       `yaml:"log"`
	Telemetry   telemetry.Config `yaml:"telemetry"`
	MySQL       mysql.Config     `yaml:"mysql"`
	StopTimeout time.Duration    `yaml:"stop_timeout"`
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
