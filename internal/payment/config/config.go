// Package config 定义 payment 服务的配置结构，由 configs/payment.yaml 绑定。
package config

import (
	"time"

	"github.com/yanking/go-skeleton/pkg/app"
	"github.com/yanking/go-skeleton/pkg/log"
	"github.com/yanking/go-skeleton/pkg/pgsql"
	"github.com/yanking/go-skeleton/pkg/queue"
	"github.com/yanking/go-skeleton/pkg/telemetry"
	"github.com/yanking/go-skeleton/pkg/transport"
)

// Config payment 服务的全部配置：声明式参数经嵌入 pkg 各包的 Config 绑定，
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
	// Pgsql 订单、商户、渠道实例等六张表所在库。
	Pgsql pgsql.Config `yaml:"pgsql"`
	// Queue 商户异步通知任务队列所用的 Redis。
	Queue queue.Config `yaml:"queue"`
	// Channel 下游 channel 服务的连接与实例同步参数。
	Channel Channel `yaml:"channel"`
	// Notify 商户通知与渠道回调地址相关参数。
	Notify Notify `yaml:"notify"`
}

// Channel 下游 channel 服务参数：payment 的下单、回调验签、实例元数据均经它取得。
type Channel struct {
	// Addr channel 服务 gRPC 地址，必填——缺它整个下单链路不成立，装配期即死。
	Addr string `yaml:"addr"`
	// SyncInterval 渠道实例元数据的同步周期，零值取 job 侧默认（5m）。
	SyncInterval time.Duration `yaml:"sync_interval"`
}

// Notify 通知相关参数。
type Notify struct {
	// CallbackBaseURL 下发给渠道的回调地址前缀，须是渠道侧可达的外网基址
	// （如 https://pay.example.com），拼接为 <base>/callbacks/payment/{instance_id}。
	CallbackBaseURL string `yaml:"callback_base_url"`
}
