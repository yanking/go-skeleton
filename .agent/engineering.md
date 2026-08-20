# 工程规范

## 工具链

| 工具 | 用途 | 版本管理 |
|---|---|---|
| Go ≥ 1.24 | 语言与构建 | go.mod `go` 指令 |
| buf | proto 的 lint / breaking / 生成统一入口 | Makefile 钉版本并提供安装目标 |
| protoc-gen-go / go-grpc / grpc-gateway / openapiv2 | 代码与接口文档生成 | go.mod `tool` 指令钉死，经 `go tool` 调用 |
| protovalidate | 参数校验：`buf.yaml` 依赖 `buf.build/bufbuild/protovalidate`（注解），Go 侧 `github.com/bufbuild/protovalidate-go`（拦截器执行） | go.mod 常规依赖钉死 |
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
| `make test` | `go test -race ./...` |
| `make lint` | goimports 检查 + `golangci-lint run` |
| `make migrate-up SERVICE=<name>` / `make migrate-down SERVICE=<name>` | 对指定服务执行 / 回滚 `migrations/<name>/` 迁移 |
| `make all` | api → breaking → lint → test → build |

单测单跑：`go test -race -run 'TestUserService_Create' ./internal/user/service/`

## 新建服务清单

依次执行，全程遵守 architecture.md「目录契约」与「依赖方向（红线）」：

1. 建 `api/{service}/v1/{service}.proto`：RPC ＋ `google.api.http` 注解 ＋ protovalidate 注解，先想清错误码；
2. 跑 `make api`（及 `make breaking`，首次落库轮次按上表跳过）；
3. 建 `internal/{service}/{server,service,biz,data}` 四层与 `cmd/{service}/main.go`（装配注入、双端口、健康检查、优雅退出）；
4. 建 `configs/{service}.yaml`；若碰数据库，建 `migrations/{service}/` 并写首个迁移（up/down 成对）；
5. `make run SERVICE={service}` 实测双协议与 `/healthz`，再全量 `make all`（宪法第五条）。

## 配置

- 每服务一份 `configs/{service}.yaml`；解析用 `gopkg.in/yaml.v3`，不引重型配置框架。
- 加载唯一入口：`pkg/conf` 的 `MustLoad(configFile string, obj any)`——读文件并严格绑定到配置结构体（未知键报错），不做环境变量覆盖，配置以文件为准。
- 校验必填项：配置结构体实现 `Validate() error`，`MustLoad` 绑定后自动调用，失败即 panic 退出（go-style 允许的装配期 panic 场景）。
- 多环境：部署侧提供不同的配置文件（挂载或以启动参数指定路径），不在代码里搭环境变量映射层。

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
