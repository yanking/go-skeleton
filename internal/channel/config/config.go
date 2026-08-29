// Package config 定义 channel 服务的配置结构，由 configs/channel.yaml 绑定。
package config

import (
	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/pgsql"
	"github.com/yanking/go-skeleton/pkg/telemetry"
	"github.com/yanking/go-skeleton/pkg/transport"
)

// Config channel 服务的全部配置：声明式参数经嵌入 pkg 各包的 Config 绑定，
// 装配期注入项（如 App 的 Logger、Telemetry 的 Service）仍由装配代码在 Go 里填。
type Config struct {
	// Log 日志配置，name 即服务名，写入每条日志的 service 字段。
	Log log.Config `yaml:"log"`
	// App 启停编排配置，stop_timeout 省略时取 pkg/app 的 30s 默认。
	App app.Config `yaml:"app"`
	// Telemetry 遥测配置，exporter 省略时打 stdout；Service 由装配期注入。
	Telemetry telemetry.Config `yaml:"telemetry"`
	// Transport 传输配置：grpc_addr 是业务协议出口（纯 gRPC 服务，无 http_addr）。
	Transport transport.Config `yaml:"transport"`
	// Pgsql 渠道商户配置表所在库。
	Pgsql pgsql.Config `yaml:"pgsql"`
	// Gateway 上游 gateway-rpc 地址；留空不装配补单对账 job。
	Gateway Gateway `yaml:"gateway"`
}

// Gateway gateway-rpc 连接配置。
type Gateway struct {
	// Addr gateway-rpc 的 host:port；空 = 无对账（纯回调驱动的渠道用不着）。
	Addr string `yaml:"addr"`
}
