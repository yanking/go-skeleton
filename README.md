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
bash .agents/skills/new-service/scripts/new_service.sh <svc> [none|rpc|both] [module]
```

变体决定对外出口：`none` 无传输（纯异步任务型），`rpc` 纯 gRPC，`both` 再加 grpc-gateway
转译的 HTTP 出口（各占一个端口）。随后按 `AGENTS.md`「服务开发流程」补分层实现。
一个服务承载多个业务域（单体）时给出首个模块名，按模块分目录；已分模块的服务加业务域用
`new-module` 技能。布局怎么选见 `AGENTS.md`「服务分层」。

跑起来 `make run SVC=<svc>`；有 HTTP 出口的服务在 `/docs` 看接口文档。

## 分支

只有 `main`:纯模板层、零业务服务,也是默认分支——起新项目直接 clone 它。
在建服务走 `feature/*`,约定见 `AGENTS.md`。

## 里面有什么

`pkg/` 十一个公共包：`app` 组件生命周期、`conf` 配置、`log` 结构化日志、`errcode` 错误双通道、
`telemetry` 链路与指标、`mysql`/`pgsql`/`redis` 存储、`httpc` 出站 HTTP、`transport` 传输
（三种形态：`NewRPCServer` 纯 gRPC、`NewRPCRESTServer` gRPC + gateway 双端口、`NewRESTServer` 直连 REST；
拦截链含错误码出口翻译、处理超时、访问日志、panic 兜底、鉴权；带请求体与连接上限、
摘流窗口与 liveness/readiness 两个探针）、`queue` asynq 任务队列。

`make help` 列出全部命令；`make check` 是提交门槛（build / vet / test 全绿 + gofmt 零漂移）。
`make lint`（golangci-lint，配置见 `.golangci.yml`）、`make vuln`（govulncheck 漏洞扫描）与
`make race`（竞态检测）不并进 check——前两者会改动宪法第 5 条对门槛的定义，race 则是因为
它让测试慢几倍而 check 每次改动都要跑。宪法第 4 条（凭证只写 `${ENV_NAME}` 引用）的判据在
`test/secrets/`，它是普通 go test，所以已经在 check 里。
`.github/workflows/ci.yml` 在 push、PR 与每周定时上跑 check / race / lint / vuln 四个 job——
门槛由机器执行，不靠记得跑；定时触发是因为漏洞是时间的函数，仓库安静一个月不等于安全。

`make docker SVC=<svc>` 出镜像（配置不打进镜像，由部署侧挂载）；探针、宽限期预算与
凭证注入见 [docs/deploy.md](docs/deploy.md)。

## 许可

[MIT](LICENSE)。拿它起项目不必开源你的业务代码，保留 `LICENSE` 文件即可——
`new-project` 会保留它，并在清单里说明要换成自己的许可证时怎么处理。

## 文档

| 位置 | 内容 |
|---|---|
| [AGENTS.md](AGENTS.md) | 项目导航与约束入口（给 AI 助手与新人），含分层约定与开发动线 |
| [CONSTITUTION.md](CONSTITUTION.md) | 仓库最高约束，与其冲突的一律以它为准 |
| `.agents/go-style.md` | 编码规范 |
| `.agents/skills/` | 可执行技能：`new-project` 起项目、`new-service` 起服务、`new-module` 加模块 |
| [docs/deploy.md](docs/deploy.md) | 部署参考：探针语义、宽限期预算、退出码、资源限制 |
| [docs/toolbox.md](docs/toolbox.md) | 仓库级工具参考 |
