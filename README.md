# go-skeleton

Go 微服务 monorepo **模板仓库**。契约先行（proto + gRPC/grpc-gateway），结构化日志与
可观测性开箱即用；业务单 `go.mod`，开发工具链另置 `tools/` 子 module，其依赖不进主依赖图。

它不是一个应用，而是一份起点：一切改动都要经得起一个问题——「这个模式复制到第 10 个服务时还成立吗?」

## 起一个新项目

```sh
git clone git@github.com:yanking/go-skeleton.git acme-pay
cd acme-pay
make new-project MODULE=github.com/acme/acme-pay
```

一条命令换掉 module path、抹去模板身份、生成 README、重置 git 历史并把模板挂成 `upstream`，
自检 `make check` 全绿才落地。细节与生成后清单见 `.agents/skills/new-project/SKILL.md`。

用 AI 助手的话，**在 clone 出来的目录里**说一句「用这个模板起个新项目 acme-pay」即可触发同一条动线——
`new-project` 技能随仓库分发，先有这份 clone 才看得见它。

## 起一个新服务

```sh
bash .agents/skills/new-service/scripts/new_service.sh <svc> [none|rpc|both]
```

变体决定对外出口：`none` 无传输（纯异步任务型），`rpc` 纯 gRPC，`both` 再加 grpc-gateway
转译的 HTTP 出口（各占一个端口）。随后按 `AGENTS.md`「服务开发流程」补分层实现。

跑起来 `make run SVC=<svc>`；有 HTTP 出口的服务在 `/docs` 看接口文档。

## 分支

| 分支 | 内容 |
|---|---|
| `main` | 只有模板层，零业务服务——**默认分支，起新项目直接 clone 它** |
| `example` | 模板层 + 三个示例服务（channel / payment / price），照着看分层怎么落地 |

## 里面有什么

`pkg/` 十一个公共包：`app` 组件生命周期、`conf` 配置、`log` 结构化日志、`errcode` 错误双通道、
`telemetry` 链路与指标、`mysql`/`pgsql`/`redis` 存储、`httpc` 出站 HTTP、`transport` 传输
（gRPC + gateway 双端口，拦截链含错误码出口翻译、访问日志、鉴权）、`queue` asynq 任务队列。

`make help` 列出全部命令；`make check` 是提交门槛（build / vet / test 全绿 + gofmt 零漂移）。

## 文档

| 位置 | 内容 |
|---|---|
| [AGENTS.md](AGENTS.md) | 项目导航与约束入口（给 AI 助手与新人），含分层约定与开发动线 |
| [CONSTITUTION.md](CONSTITUTION.md) | 仓库最高约束，与其冲突的一律以它为准 |
| `.agents/go-style.md` | 编码规范 |
| `.agents/skills/` | 可执行技能：`new-project` 起项目、`new-service` 起服务 |
| [docs/toolbox.md](docs/toolbox.md) | 仓库级工具参考 |
