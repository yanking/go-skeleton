# 项目架构

> 本文档定义脚手架的目标形态，是落地实现时的对照契约；任何偏离走宪法第三条。仓库形态：**多服务 monorepo**，每个服务独立成套、可独立编译与部署。

## 总览（单服务视角）

每个服务一个二进制、双协议、pb-first：

```
HTTP/JSON :8080              gRPC :9090
     │                          │
grpc-gateway ── 环回 gRPC ──▶ gRPC Server ◀── StatsHandler 观测（otelgrpc，链外接入）
                                │  拦截器链：recovery → 日志 → 鉴权 → 参数校验
                                ▼
                            service    实现 pb 生成的 Server 接口；pb ↔ 领域类型转换
                                ▼
                            biz        领域逻辑；定义仓储等外设接口
                                ▲ 实现（依赖倒置）
                            data       数据库 / 缓存 / 下游服务客户端
```

- 同一进程开两个端口：`:9090` 纯 gRPC；`:8080` HTTP，由 grpc-gateway 经环回连接转发到 gRPC 端口（端口按服务经配置区分）。
- 为什么走环回而不用 `RegisterXxxHandlerServer` 进程内直连：直连会绕过 gRPC 拦截器链，鉴权、日志就得在 HTTP 侧再实现一遍。环回让横切关注点只写一次。

## 目录契约

```
.
├── .agent/                   # AI 规范文档层：架构 / 编码规范 / 工程规范
├── api/                      # 协议层（对外事实源）
│   └── {service}/v1/         # 按服务 + 版本分目录
│       ├── {service}.proto   # 含 google.api.http 注解与 protovalidate 校验注解
│       └── *.pb.go 等        # 生成物，随 proto 同提交，禁手改
├── buf.yaml                  # buf 模块与 lint / breaking 配置
├── buf.gen.yaml              # 生成插件配置
├── Makefile                  # 命令契约唯一载体（定义见 engineering.md）
├── .golangci.yml             # 静态检查配置（含 generated 排除，见 go-style.md 适用范围）
├── .gitignore                # 忽略 bin/ 与 .claude/（settings.json 除外，报告与备份不入库）
├── go.mod / go.sum           # 单一 module，多服务共用
├── cmd/{service}/            # main：读配置、构造注入、启动双端口、优雅退出
├── configs/{service}.yaml    # 每服务一份配置，经 pkg/conf.MustLoad 绑定到结构体
├── migrations/{service}/     # golang-migrate 纯 SQL 迁移文件（工作流见 engineering.md）
├── docs/                     # 业务与产品文档：人类主编，AI 仅在用户明示时写入；AI 规范勿入
├── internal/
│   └── {service}/            # 每服务一套四层，服务间禁止互相 import
│       ├── server/           # 传输装配：gRPC server、gateway mux、拦截器注册
│       ├── service/          # 实现 pb 生成的 {Service}Server；出入参转换与映射错误码
│       ├── biz/              # 领域逻辑；定义仓储接口；不感知传输与存储
│       └── data/             # 实现 biz 的接口：数据库、缓存、下游服务客户端
├── pkg/                      # 跨服务共享的领域无关工具（可被外部仓库引用；谨慎准入，禁止业务逻辑）
│   └── conf/                 # 配置加载：MustLoad(configFile, obj)
├── openapi/                  # 生成的 OpenAPI 文档，禁手改
└── bin/                      # make build 产物，不入库
```

## 依赖方向（红线）

服务内：`cmd → server → service → biz ← data`；cmd 是装配根，不被任何层 import。

| 层 | 可以 import | 不可以 import |
|---|---|---|
| cmd | 本服务 server、service、biz、data（仅装配期构造与注入） | 其他服务的任何层 |
| server | 本服务 service、本服务 api(pb) | biz、data |
| service | 本服务 api(pb)、本服务 biz | data |
| biz | 标准库、`pkg/`（领域无关工具） | api(pb)、service、data、任何存储驱动 |
| data | 本服务 biz（为实现其接口）、下游服务的 api(pb 客户端桩) | service、server |

要点：

- pb 类型止步于 service 层，biz 只见领域类型；转换代码写在 service。
- biz 定义接口、data 实现（依赖倒置），装配在 cmd 用构造函数手工注入，不引 DI 框架。
- **跨服务红线**：服务间禁止 import 彼此的 `internal/{service}`；跨服务调用一律走对方 pb 接口，gRPC 客户端归调用方 data 层持有。共享代码只能进 `pkg/`，且必须领域无关、不含业务逻辑。

## 协议层规则

- 目录即版本：`api/{service}/v1/`；破坏性变更开 `v2/`，v1 按弃用周期维护。
- 每个 RPC 必须写 `google.api.http` 注解——HTTP 路由在 proto 里就是文档。
- 路由风格：资源名词复数 + 标准方法（`GET /v1/users/{id}`、`POST /v1/users`）；自定义动作用 `POST /v1/users/{id}:activate` 形式。
- 错误模型：service 层把 biz 错误集中映射为 `google.golang.org/grpc/status`（codes + errdetails），gateway 自动转 HTTP 状态码；错误码映射表随 service 维护，只在这一处翻译。
- 参数校验：proto 层用 protovalidate 注解声明，拦截器统一执行；service 只做注解表达不了的业务校验。
- 健康检查：实现 gRPC Health v1 协议（复用 grpc-go 官方 `grpc_health_v1`，不复制进 `api/`）；HTTP 侧 `GET /healthz` 在 gateway mux 上以代码注册——此为宪法第一条的基础设施例外，不进 `openapi/`。

## 横切关注点

- 拦截器链统一注册在 `internal/{service}/server`，顺序：recovery → 日志 → 鉴权 → 参数校验。
- 观测**不在拦截器链上**：`otelgrpc.NewServerHandler` 是 `stats.Handler`，经 `grpc.StatsHandler` 注册，先于整条拦截器链触发（日志拦截器因此总能拿到已开启的 span）。改为自实现拦截器属选型变更，走宪法第三条。
- 鉴权拦截器带按完整方法名（`info.FullMethod`）的放行清单：`grpc.health.v1.Health/*` 默认在列；`/healthz` 同理不做业务鉴权。
- 元端点清单（宪法第一条例外的全集）：`GET /healthz`。新增须经用户批准并同步更新本清单。
- 日志：`log/slog` 结构化输出。分级：Debug=开发排查细节；Info=正常业务里程碑（默认级）；Warn=可自愈异常或降级；Error=需人介入的失败。公共字段统一 `trace_id`、`service`、`method`；级别经配置可调。请求出入口日志由拦截器统一打，业务代码不重复打「进入 / 退出」。
- 优雅退出：cmd 监听 SIGINT/SIGTERM，顺序：停 HTTP → 关 gateway 环回 ClientConn → `GracefulStop` gRPC（配超时，默认 10s 可配置，超时转 `Stop()` 强制终止）→ 关闭 data 资源。
