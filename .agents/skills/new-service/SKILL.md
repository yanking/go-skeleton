---
name: new-service
description: 在 go-skeleton 仓库新增微服务：用脚本按变体（none/rpc/both）渲染入口模板（cmd/<svc>/main.go、initial 装配、internal/<svc>/config、configs/<svc>.yaml，传输变体含 transport.Server 接线），再按 AGENTS.md 六件套约定补业务分层并登记错误码分段。用户说「新增服务 / 新建 xxx 服务 / 初始化一个服务 / 加一个 svc / 加个 rpc（或 http）服务」时使用，无论是否提到模板。
---

# 新增服务

入口文件已固化为模板，由脚本渲染保证服务名替换零差错；结构约定以 AGENTS.md 为准。

## 生成入口

```bash
bash .agents/skills/new-service/scripts/new_service.sh <svc> [none|rpc|both]
```

- `<svc>` 为小写 kebab-case 服务名；同名服务已存在时脚本拒绝覆盖。
- 变体决定 `createServer` 里 `transport.NewServer` 的选项与 yaml 的 `transport` 段：
  - `none`（默认）：无传输段，只有遥测——纯异步任务型或骨架先行；
  - `rpc`：纯 gRPC（内置 health 协议、reflection、errcode 出口拦截器），yaml 带 `transport.grpc_addr`；业务 pb 生成后用 `WithService` 注册。
  - `both`：`rpc` 再加 `WithGateway` 与 `http_addr`——proto 加 `google.api.http` 注解后 `make proto` 生成转译代码，HTTP 端口自带 `/healthz`，`/docs` 由 `WithMount` 挂 `openapi` 聚合包（spec embed + 阅读页）提供，该包仓内缺失时由脚本一并落地。
- **能力在代码、暴露在配置**：`WithGateway` 声明本服务有 HTTP 转译能力，`transport.http_addr` 决定本次部署是否暴露。生成的 `both` 骨架里两者都是注释状态，**须一起放开**——只配端口会在装配期报错。
- **传输开关就是 transport 段**：整段缺失即无对外出口；`grpc_addr` 必填，`http_addr` 只是它的代理出口，单独配置会在装配期报错。
- 生成物：`cmd/<svc>/main.go`、`cmd/<svc>/initial/init_app.go`、`internal/<svc>/config/config.go`、`configs/<svc>.yaml`，以及 internal 分层骨架——所有变体含 `service/repo/model/job` 的 `doc.go`，rpc/both 变体额外含 `handler/doc.go`。

## 生成后必做（顺序即检查清单）

1. **登记错误码分段**：AGENTS.md 错误码分段表新增一行，分配一个未使用的万位段给该服务（每段 10000 宽，如 20000–29999；已用与未分配段见该表，通用码占 10000–19999）。
2. **写 proto 契约并生成产物**（rpc/both 变体）：动线见 AGENTS.md「服务开发流程」第 2 步；注释格式按 `.agents/go-style.md`「proto 注释」——它直接生成文档页的操作标题与字段说明。写完跑 `make proto`。
3. **填业务实体**：骨架目录已生成，各层填什么见其 doc.go 注释；职责、接口位置、错误流向遵守 AGENTS.md「服务分层」与「错误处理约定」两节。
4. **挂基础组件**：用到 db/redis 时在 createInfra 构造注册（基础在前），句柄解嵌后传给 createServer 侧使用；Config 与 yaml 相应加段。有库的服务同时用 `make migrate-create SVC=<svc> NAME=<desc>` 建首个迁移（流程见 AGENTS.md「服务开发流程」）。
5. **验证**：`make check` 全绿后收工（含 gofmt 零漂移）。

> **both 变体的顺序约束**：本清单第 2 步（`make proto`）之前 `make check` 必然编译不过——`openapi` 聚合包用 `//go:embed */v1/*.json` 嵌 spec，而 `go:embed` 的每个 pattern 都必须至少命中一个文件，首个 spec 生成前无文件可嵌。报错形如 `openapi/openapi.go:15:12: pattern */v1/*.json: no matching files found`，属预期，按清单顺序走即消失。

## 约束

- 只动新服务的文件。
- 服务名出现在路径、包注释、flag 默认值、yaml `log.name` 四处，全部由脚本替换生成。
