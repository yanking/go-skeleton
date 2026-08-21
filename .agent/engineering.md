# 工程规范

## 工具链

| 工具 | 用途 | 版本管理 |
|---|---|---|
| Go ≥ 1.24 | 语言与构建 | go.mod `go` 指令 |
| buf | proto 的 lint / breaking / 生成统一入口 | Makefile 钉版本并提供安装目标 |
| protoc-gen-go / go-grpc / grpc-gateway / openapiv3 | 代码与接口文档生成（OpenAPI 3.1） | go.mod `tool` 指令钉死，经 `go tool` 调用 |
| protovalidate | 参数校验：`buf.yaml` 依赖 `buf.build/bufbuild/protovalidate`（注解），Go 侧 `buf.build/go/protovalidate`（拦截器执行；该模块自 v1.x 起已从 `github.com/bufbuild/protovalidate-go` 改名，旧路径 `go get` 会直接报 module path 不匹配） | go.mod 常规依赖钉死 |
| sqlc | 由 SQL 生成类型安全的 data 层访问代码；schema 直接读 `migrations/{service}/` | Makefile 钉版本 |
| Scalar（`@scalar/api-reference`） | 接口文档阅读器。服务端 `/docs` 从 CDN 取，`make docs` 下载同版本单文件到 `tools/` 供离线用 | 版本唯一事实源是 `pkg/transport/http.go` 的 `scalarVersion`，Makefile 读它；非 Go 生态，升级须手动盯 release |
| go-sql-driver/mysql ＋ jackc/pgx | 数据库驱动，经 `pkg/mysql`／`pkg/postgres` 统一装配，出口均为 `*sql.DB` | go.mod 常规依赖钉死 |
| redis/go-redis | Redis 客户端，经 `pkg/redis` 装配 | go.mod 常规依赖钉死 |
| XSAM/otelsql ＋ redisotel | 存储埋点。`redisotel` 是 go-redis 官方出品；`otelsql` 是社区库（OTel 无官方 `database/sql` 埋点），用得广但非官方背书 | go.mod 常规依赖钉死；`semconv` 版本须与 otelsql 内部一致 |
| grpc-go ＋ grpc-gateway | 双协议传输，经 `pkg/transport` 统一装配 | go.mod 常规依赖钉死 |
| OpenTelemetry | 可观测性：trace ＋ metrics ＋ runtime 指标，经 `pkg/telemetry` 统一构造 | go.mod 常规依赖钉死；`otel`／`otel/sdk` 系列须同版本，`semconv` 版本必须与 `sdk/resource` 内部使用的一致，否则 `resource.Merge` 会因 schema URL 冲突失败 |
| golangci-lint | 静态检查，配置在 `.golangci.yml` | Makefile 钉版本（与 buf 同策略；官方不推荐模块 / tool 指令方式安装——MVS 会漂移其内部钉死的 linter 版本） |
| golang-migrate | 数据库迁移 CLI | Makefile 钉版本 |

原则：所有开发者与 CI 使用同一钉死版本；任何「装个工具」的需求先进 Makefile 再进文档。

钉版本的落地方式：Makefile 顶部的 `*_VERSION` 变量是唯一事实源，安装目标把二进制装成 `bin/<工具>-<版本>`。版本号进文件名意味着改版本即换 make 目标路径，会自动重装——不会出现「用着旧二进制却以为是新版本」。各目标依赖对应的二进制路径，缺了自动装，无需先手动 `make init`。

## 命令契约（Makefile）

| 目标 | 语义 |
|---|---|
| `make init` | 安装钉死版本的工具链到 `tools/`（buf / golangci-lint / golang-migrate / sqlc）。二进制名带版本号（`tools/buf-v1.72.0`），改了 Makefile 里的版本号即换目标路径，make 自动重装。`bin/` 只放 `make build` 出的服务二进制，两者不混 |
| `make proto-deps` | `buf dep update`，拉取 / 更新 `buf.yaml` 声明的 proto 依赖并写 `buf.lock` |
| `make api` | `buf lint` → `buf generate`（Go 生成物落 `gen/`，接口文档落 `openapi/`） |
| `make breaking` | `buf breaking --against '.git#branch=main'`。前提：git 仓且 main 基线可解析；尚无基线的首次落库轮次跳过并在提交说明注明（宪法第六条例外） |
| `make sql` | `sqlc vet` → `sqlc generate`（产出 `internal/{service}/data/sqlc/`） |
| `make build` | 编译全部 `cmd/*` 到 `bin/`（只放服务二进制）；`cmd/` 为空时跳过并提示（首个服务落地前的过渡状态） |
| `make run SERVICE=<name>` | 起指定服务，读 `configs/<name>.yaml`；SERVICE 必填 |
| `make docs SERVICE=<name> [API_VERSION=v1]` | 生成可离线打开的接口文档页到 `tools/docs/<name>-<ver>.html`：spec 内联、阅读器取自 `tools/` 的本地缓存，`file://` 打开零外网请求。不起服务、不连数据库。首次执行需联网下载阅读器（3.8MB），之后离线可用 |
| `make test` | `go test -race ./...`；需要真实数据库的用例在未配 DSN 时自动跳过 |
| `make test-integration` | 带 `MYSQL_DSN` / `POSTGRES_DSN` 跑全量，用于本地验证存储包 |
| `make lint` | `golangci-lint fmt --diff`（gofmt + goimports 检查，有 diff 即失败）＋ `golangci-lint run` |
| `make fmt` | `golangci-lint fmt`，就地格式化 |
| `make migrate-up SERVICE=<name> DATABASE_URL=<url>` / `make migrate-down ...` | 对指定服务执行 / 回滚一步 `migrations/<name>/` 迁移；两个参数都必填，缺了会带用法提示直接失败 |
| `make all` | api → sql → breaking → lint → test → build；默认目标 |
| `make help` | 列出全部目标 |

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

## SQL 工作流

1. 改 `migrations/{service}/NNNN_描述.up.sql`（up/down 成对）——迁移即 schema，sqlc 直接读它，不另维护一份；
2. 在 `internal/{service}/data/query/*.sql` 写查询，用 `-- name: Xxx :one|:many|:exec|:execresult` 标注；
3. 跑 `make sql`。SQL 写错、字段不存在会在 `sqlc vet` / `generate` 阶段就报错，不必等到运行期；
4. 生成物随 SQL 同提交，禁手改；仓储在 `data/*.go` 里调生成的 `Queries`，并把存储错误翻译成领域错误。

## Proto 工作流

1. 修改 `api/{service}/v{n}/*.proto`；新接口先想清 HTTP 注解、错误码与校验注解。
2. 跑 `make api` 与 `make breaking`。lint 或 breaking 不过就地解决：确需破坏兼容时走宪法第六条（显式列明 + 用户确认 + 评估开新版本目录）。
3. 生成物与 proto 同一提交（宪法第七条）：Go 代码在 `gen/`、文档在 `openapi/`，两者都禁手改；`api/` 下只应有 `.proto`。

## 提交规范

- Conventional Commits：`feat|fix|refactor|docs|test|chore(scope): 中文描述`；scope 用服务或模块名（如 `user`、`proto`、`makefile`）。
- 提交前 `make lint && make test` 必须全绿（宪法第五条的落地点）。
- ~~Bootstrap 期例外~~：Makefile 已落地，该例外自此失效，一律跑 `make lint && make test`。
- 一次提交一件事；proto 变更（含生成物）与对应业务实现可同提交，但不得混入无关重构。

## CI 基线（落地时按此搭）

前提：checkout 用 `fetch-depth: 0`，并确保 `main` 分支引用可解析——否则 `make breaking` 解析不到基线。

PR 必过五关，顺序执行：

1. `make api` 与 `make sql` 后 `git diff --exit-code`——校验已提交生成物与 proto / SQL 一致；
2. `make breaking`；
3. `make lint`；
4. `make test`；
5. `make build`。

## 容器化与发布（占位，有意暂缓）

首个服务跑通 `make all` 后落地，届时补：每服务一镜像的多阶段 Dockerfile（构建参数 SERVICE）、版本号经 `-ldflags` 注入、镜像 tag 对齐 git tag、CI 追加镜像构建关。在此之前不做部署侧约定。

落地时必须一并处理：K8s Deployment 的 `terminationGracePeriodSeconds` 要比服务配置的停机总超时大几秒（`pkg/app` 默认 30s，该字段默认也是 30s，取等没有余量），否则优雅退出的收尾会被 SIGKILL 打断——详见 `architecture.md`「横切关注点」停机条目。
