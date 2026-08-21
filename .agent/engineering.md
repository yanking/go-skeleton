# 工程规范

## 工具链

| 工具 | 用途 | 版本管理 |
|---|---|---|
| Go ≥ 1.24 | 语言与构建 | go.mod `go` 指令 |
| buf | proto 的 lint / breaking / 生成统一入口 | Makefile 钉版本并提供安装目标 |
| protoc-gen-go / go-grpc / grpc-gateway / openapiv2 | 代码与接口文档生成 | go.mod `tool` 指令钉死，经 `go tool` 调用 |
| protovalidate | 参数校验：`buf.yaml` 依赖 `buf.build/bufbuild/protovalidate`（注解），Go 侧 `buf.build/go/protovalidate`（拦截器执行；该模块自 v1.x 起已从 `github.com/bufbuild/protovalidate-go` 改名，旧路径 `go get` 会直接报 module path 不匹配） | go.mod 常规依赖钉死 |
| go-sql-driver/mysql ＋ jackc/pgx | 数据库驱动，经 `pkg/mysql`／`pkg/postgres` 统一装配，出口均为 `*sql.DB` | go.mod 常规依赖钉死 |
| redis/go-redis | Redis 客户端，经 `pkg/redis` 装配 | go.mod 常规依赖钉死 |
| XSAM/otelsql ＋ redisotel | 存储埋点。`redisotel` 是 go-redis 官方出品；`otelsql` 是社区库（OTel 无官方 `database/sql` 埋点），用得广但非官方背书 | go.mod 常规依赖钉死；`semconv` 版本须与 otelsql 内部一致 |
| grpc-go ＋ grpc-gateway | 双协议传输，经 `pkg/transport` 统一装配 | go.mod 常规依赖钉死 |
| OpenTelemetry | 可观测性：trace ＋ metrics ＋ runtime 指标，经 `pkg/telemetry` 统一构造 | go.mod 常规依赖钉死；`otel`／`otel/sdk` 系列须同版本，`semconv` 版本必须与 `sdk/resource` 内部使用的一致，否则 `resource.Merge` 会因 schema URL 冲突失败 |
| golangci-lint | 静态检查，配置在 `.golangci.yml` | Makefile 钉版本（与 buf 同策略；官方不推荐模块 / tool 指令方式安装——MVS 会漂移其内部钉死的 linter 版本） |
| golang-migrate | 数据库迁移 CLI | Makefile 钉版本 |

原则：所有开发者与 CI 使用同一钉死版本；任何「装个工具」的需求先进 Makefile 再进文档。

## 命令契约（Makefile）

| 目标 | 语义 |
|---|---|
| `make init` | 安装 / 校验钉死版本的工具链 |
| `make api` | `buf lint` → `buf generate`（产出 pb、gateway、`openapi/`） |
| `make breaking` | `buf breaking --against '.git#branch=main'`。前提：git 仓且 main 基线可解析；尚无基线的首次落库轮次跳过并在提交说明注明（宪法第六条例外） |
| `make build` | 编译全部 `cmd/*` 到 `bin/` |
| `make run SERVICE=<name>` | 起指定服务，读 `configs/<name>.yaml`；SERVICE 必填 |
| `make test` | `go test -race ./...`；需要真实数据库的用例在未配 DSN 时自动跳过 |
| `make test-integration` | 带 `MYSQL_DSN` / `POSTGRES_DSN` 跑全量，用于本地验证存储包 |
| `make lint` | goimports 检查 + `golangci-lint run` |
| `make migrate-up SERVICE=<name>` / `make migrate-down SERVICE=<name>` | 对指定服务执行 / 回滚 `migrations/<name>/` 迁移 |
| `make all` | api → breaking → lint → test → build |

单测单跑：`go test -race -run 'TestUserService_Create' ./internal/user/service/`

## 新建服务清单

依次执行，全程遵守 architecture.md「目录契约」与「依赖方向（红线）」：

1. 建 `api/{service}/v1/{service}.proto`：RPC ＋ `google.api.http` 注解 ＋ protovalidate 注解，先想清错误码；
2. 跑 `make api`（及 `make breaking`，首次落库轮次按上表跳过）；
3. 建 `internal/{service}/{server,service,biz,data}` 四层与 `cmd/{service}/main.go`：Logger 经 `pkg/log.MustNew`、可观测性经 `pkg/telemetry.MustNew`、传输层经 `pkg/transport.MustNew` 构造（双端口、健康检查、拦截器、环回全在里面，服务只填 `RegisterGRPC` / `RegisterGateway` / `Interceptors`）；根 ctx 经 `signal.NotifyContext` 生成，组件按 `telemetry → data → transport.Components()...` 顺序交给 `pkg/app` 编排，`app.Run` 返回非 nil 即以非零码退出；
4. 建 `configs/{service}.yaml`；若碰数据库，用 `pkg/mysql`／`pkg/postgres`／`pkg/redis` 构造连接并注册为 app 组件（排在 telemetry 之后、传输组件之前），建 `migrations/{service}/` 并写首个迁移（up/down 成对）；
5. `make run SERVICE={service}` 实测双协议与 `/healthz`，再全量 `make all`（宪法第五条）。

## 配置

- 每服务一份 `configs/{service}.yaml`；解析用 `gopkg.in/yaml.v3`，不引重型配置框架。
- 加载唯一入口：`pkg/conf` 的 `MustLoad(configFile string, obj any)`——读文件并严格绑定到配置结构体（未知键报错），不做环境变量覆盖，配置以文件为准。
- 校验必填项：配置结构体实现 `Validate() error`，`MustLoad` 绑定后自动调用，失败即 panic 退出（go-style 允许的装配期 panic 场景）。
- 存储参数（DSN、连接池上下限、连接寿命、探活窗口）同样由服务自己的配置结构体承载，cmd 装配时填进 `pkg/mysql.Config`／`pkg/postgres.Config`／`pkg/redis.Config`。`ConnMaxLifetime` 须小于 MySQL 的 `wait_timeout`，否则会拿到已被服务端单方面关闭的连接。
- 传输层参数（gRPC / HTTP 监听地址）同样由服务自己的配置结构体承载，cmd 装配时填进 `pkg/transport.Config`。
- 可观测性参数（导出方式、collector 地址、采样率）同样由服务自己的配置结构体承载，导出方式字段用 `telemetry.Exporter` 类型（YAML 直接写 `otlp`／`stdout`／`none`，拼错在 `conf.MustLoad` 阶段就报错），cmd 装配时填进 `pkg/telemetry.Config`。
- 日志参数（级别、格式、是否带调用点）同样由服务自己的配置结构体承载，级别字段用 `slog.Level` 类型（YAML 直接写 `debug`／`info`／`warn`／`error`），cmd 装配时填进 `pkg/log.Config` 并 `MustNew` 出 Logger 注入各组件。
- 运行参数（如停机总超时）由服务自己的配置结构体承载，cmd 装配时把加载好的值显式填进 `pkg/app.Config`——`pkg/app` 只认 Go 结构体，不碰 YAML；字段名不强制统一，但须在该服务的 `configs/{service}.yaml` 里注释说明。
- 多环境：部署侧提供不同的配置文件（挂载或以启动参数指定路径），不在代码里搭环境变量映射层。

## 集成测试

需要真实中间件的用例一律经环境变量开关，未配即 `t.Skip`——CI 基线里没有数据库，`make test` 必须在裸环境下全绿。

- Redis 不需要外部服务：`pkg/redis` 用 `miniredis`（进程内的真 Redis 实现，走真实协议）作被测服务端，始终参与 `make test`。
- MySQL / PostgreSQL 没有等价的进程内实现，用例读 `MYSQL_DSN` / `POSTGRES_DSN`。本地起容器：

```
docker run -d --name skel-mysql -e MYSQL_ROOT_PASSWORD=secret -e MYSQL_DATABASE=skeleton -p 53306:3306 mysql:8
docker run -d --name skel-pg -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=skeleton -p 55432:5432 postgres:17-alpine

MYSQL_DSN='root:secret@tcp(127.0.0.1:53306)/skeleton?parseTime=true' \
POSTGRES_DSN='postgres://postgres:secret@127.0.0.1:55432/skeleton?sslmode=disable' \
  go test -race ./...
```

## 数据库迁移

- 工具 golang-migrate；纯 SQL，放 `migrations/{service}/`，命名 `NNNN_描述.up.sql` / `NNNN_描述.down.sql`，up/down 必须成对。
- 执行独立于服务启动（进程不自动跑迁移）：本地与部署流程显式执行 `make migrate-up SERVICE=x`；CI 不自动执行迁移。

## Proto 工作流

1. 修改 `api/{service}/v{n}/*.proto`；新接口先想清 HTTP 注解、错误码与校验注解。
2. 跑 `make api` 与 `make breaking`。lint 或 breaking 不过就地解决：确需破坏兼容时走宪法第六条（显式列明 + 用户确认 + 评估开新版本目录）。
3. 生成物与 proto 同一提交（宪法第七条）；`openapi/` 即接口文档，禁止另行手写接口文档。

## 提交规范

- Conventional Commits：`feat|fix|refactor|docs|test|chore(scope): 中文描述`；scope 用服务或模块名（如 `user`、`proto`、`makefile`）。
- 提交前 `make lint && make test` 必须全绿（宪法第五条的落地点）。
- Bootstrap 期例外（与 `make breaking` 同策略）：Makefile 尚未落地前，改跑 `gofmt -l . && go vet ./... && go test -race ./... && go build ./...`，并在提交说明注明所用命令与结果；Makefile 落地后本例外即失效。
- 一次提交一件事；proto 变更（含生成物）与对应业务实现可同提交，但不得混入无关重构。

## CI 基线（落地时按此搭）

前提：checkout 用 `fetch-depth: 0`，并确保 `main` 分支引用可解析——否则 `make breaking` 解析不到基线。

PR 必过五关，顺序执行：

1. `make api` 后 `git diff --exit-code`——校验已提交生成物与 proto 一致；
2. `make breaking`；
3. `make lint`；
4. `make test`；
5. `make build`。

## 容器化与发布（占位，有意暂缓）

首个服务跑通 `make all` 后落地，届时补：每服务一镜像的多阶段 Dockerfile（构建参数 SERVICE）、版本号经 `-ldflags` 注入、镜像 tag 对齐 git tag、CI 追加镜像构建关。在此之前不做部署侧约定。

落地时必须一并处理：K8s Deployment 的 `terminationGracePeriodSeconds` 要比服务配置的停机总超时大几秒（`pkg/app` 默认 30s，该字段默认也是 30s，取等没有余量），否则优雅退出的收尾会被 SIGKILL 打断——详见 `architecture.md`「横切关注点」停机条目。
