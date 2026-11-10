// Package config 定义 __svc__ 服务的配置结构，由 configs/__svc__.yaml 绑定。
package config

import (
	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/telemetry"
	"github.com/yanking/go-skeleton/pkg/transport"
)

// Config __svc__ 服务的全部配置：声明式参数经嵌入 pkg 各包的 Config 绑定，
// 装配期注入项（如 App 的 Logger、Telemetry 的 Service）仍由装配代码在 Go 里填。
type Config struct {
	// Log 日志配置，name 即服务名，写入每条日志的 service 字段。
	Log log.Config `yaml:"log"`
	// App 启停编排配置，stop_timeout 省略时取 pkg/app 的 30s 默认。
	App app.Config `yaml:"app"`
	// Telemetry 遥测配置，exporter 省略时打 stdout；Service 由装配期注入。
	Telemetry telemetry.Config `yaml:"telemetry"`
	// Transport 传输配置：grpc_addr 是业务协议出口，http_addr 是 gateway 转译出口
	// （留空即纯 gRPC）；无对外服务的服务整段留零值即可（不构造）。
	Transport transport.Config `yaml:"transport"`
}
