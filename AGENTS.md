# AGENTS.md

> 本文件是给 AI 助手(以及新人)的项目导航与约束入口。
> 编码细则:@.agents/go-style.md

## 宪法
@CONSTITUTION.md

## 项目定位

Go 微服务 monorepo 骨架:业务单 `go.mod`(开发工具链另置 `tools/` 子 module,其依赖不进主依赖图)、proto 契约 + gRPC/grpc-gateway、结构化日志、可观测性基建。
它首先是**模板仓库**:一切改动都要经得起一个问题——"这个模式复制到第 10 个服务时还成立吗?"
新增服务用 `.agents/skills/new-service` 技能渲染入口模板,不要手抄。
拿本仓起新项目用 `.agents/skills/new-project` 技能(`make new-project MODULE=...`):module path 与模板身份散在三十余处,手工改必漏。

分支约定(判断"当前分支该有什么"看这里):

| 分支 | 内容 |
|---|---|
| `main` | 只有模板层,零业务服务(`api/`、`cmd/`、`configs/`、`internal/` 均不存在)——clone 默认拿到的就是它 |
| `example` | 模板层 + 示例服务;模板层与 `main` 逐字一致,改了模板层就要同步过去 |
| `feature/*` | 在建服务,业务层允许是半成品;模板层改动照样受"第 10 个服务"检验 |

模板层同步:改动优先直接落在 `main`,再 `git merge main` 带进 `example`——两支共享历史,同步成本恒定。反方向(模板层改动先落在 `example`,如随 `feature/*` 一并进来)只能把那几个提交 cherry-pick 到 `main`:`merge example` 会把业务层也带过去。

文档分布:

| 位置 | 内容 |
|---|---|
| `README.md` | 人类入口:快速开始与仓库导览 |
| `CONSTITUTION.md` / 本文件 / `.agents/` | 约束与导航;编码细则在 `.agents/go-style.md`,可执行技能在 `.agents/skills/` |
| `docs/<svc>/` | 服务的业务文档:架构、时序、接入指南;`diagrams/` 是 archify 渲染页(channel 首用) |
| `docs/toolbox.md` | 仓库级工具参考(make 目标之外的工具,如 archify 画图) |
| `docs/superpowers/{specs,plans}/` | 按日期归档的技术方案与实施计划 |

## 公共包地图(`pkg/`)

| 包 | 职责 |
|---|---|
| `pkg/app` | 组件生命周期编排:按注册顺序拉起、逆序停止 |
| `pkg/conf` | YAML 配置加载与绑定校验 |
| `pkg/log` | 构造统一 slog.Logger,支持从 ctx 提取 trace_id 等字段 |
| `pkg/errcode` | 业务错误码与错误的「双通道」封装 |
| `pkg/telemetry` | OTel 链路追踪与指标一键装配,stdout/otlp 双导出,含运行时指标 |
| `pkg/mysql` | GORM MySQL 连接池,dbresolver 自动读写分离,内嵌 `*gorm.DB` 直接调用 |
| `pkg/pgsql` | GORM PostgreSQL(pgx stdlib)连接池,与 `pkg/mysql` 同构 |
| `pkg/redis` | go-redis 客户端,单机/集群经 addrs 数量自动推断,内嵌 `UniversalClient` 直接调用 |
| `pkg/httpc` | 出站 HTTP 客户端:按体形态给方法(JSON/表单/原文体)与 Get,超时逐调用可覆盖,响应体有大小上限,连接池逐 Client 独立;注入 TracerProvider 即埋出站 span 并透传 traceparent,注入 Logger 即逐次记出站日志(url 只留 scheme://host/path,报文不记) |
| `pkg/transport` | 对外传输组件:gRPC 服务 + grpc-gateway 转译的 HTTP 代理出口(各占一个端口),拦截链含 errcode 出口翻译、访问日志与鉴权(otel 另经 StatsHandler 挂载),链序见包注释 |
| `pkg/queue` | asynq 任务队列薄封装:Client 入队 + Worker 消费(实现 app.Component) |

本表只是索引;使用约束(锁读纪律、停机顺序、全局状态、装配期行为)以各包 Go doc 注释为准,改用法前先读。

## 错误处理约定(改错误相关代码前必读)

不变量见宪法第 1、2 条;双通道封装(业务字段给客户端、原始错误链只进日志)与用法约定见 `pkg/errcode` 包注释。

逐层翻译的固定流向:repo 把可识别的底层错误翻译为 service 哨兵 → service 统一翻译为业务 errcode(底层错误用 Wrap 挂住 cause)→ 出口由 pkg/transport 统一打包(gRPC 拦截器把业务码放 status details,HTTP 经 gateway 渲染)。每层只翻译相邻下层的错误。

错误码分段(新服务在此登记):

| 分段 | 归属 |
|---|---|
| 10000–19999 | `pkg/errcode` 通用码(已用:10001 参数错误、10002 资源不存在、10003 内部错误、10004 未认证) |
| 40000–49999 | channel(已用:40001 渠道实例不存在、40002 下游渠道请求失败、40003 回调验签失败、40004 回调状态未知、40005 渠道响应解析失败) |
| 50000–59999 | payment(已用:50001 商户订单号重复、50002 金额超限、50003 指定渠道未绑定或不可用、50004 无可用渠道、50005 订单状态冲突、50006 商户状态受限) |

## 服务分层(`internal/<svc>/`)

目录(六件套):`config`(本服务配置结构体,绑定 `configs/<svc>.yaml`)/ `handler`(协议出口,薄壳,只调 service 并返回 errcode;仅 rpc/both 变体有)/ `service`(业务层,声明仓储接口——依赖倒置支点)/ `repo`(接口的 GORM 实现,ORM 不出本层)/ `model`(表模型,不出服务边界,出口转换由 handler 做)/ `job`(异步任务,实现 app.Component);对接三方渠道/供应商的服务可加 `adapter` 层(签名、报文拼装、响应解析、状态映射不出本层,channel 服务首用)。传输装配在 `cmd/<svc>/initial`,不进 internal:基础组件与业务组件分函数构造(createInfra:遥测/DB/Redis;createServer:传输、job),App 组装时基础在前业务在后——顺序即注册顺序,基础组件先起后停。

gRPC 是唯一的业务协议,HTTP 侧只是 gateway 代理:把 HTTP/JSON 翻成 gRPC 调用、经环回打回本进程,故拦截链对两种协议同样生效,业务代码只写一遍。两者各占一个端口,暴露范围、mesh 协议标注与网络策略可分别配置。

能力与部署分离:代码里给 `WithGateway` 声明「本服务有 HTTP 转译能力」,配置里的 `transport.http_addr` 决定「本次部署要不要暴露它」。只配端口未声明能力即装配期报错;只声明能力未配端口即纯 gRPC 部署,同一份二进制两种形态。

接口文档:`make proto` 为带 `google.api.http` 注解的服务生成 OpenAPI v3 spec,HTTP 出口挂 `/docs` 即得阅读页;`openapi` 聚合包由 new-service 技能随首个 both 变体服务落地(模板在其 assets),机制见包注释。

数据库迁移:SQL 迁移按服务放 `migrations/<svc>/`,goose 注解格式(一个文件一对 Up/Down),序号统一用 `make migrate-create SVC=<svc> NAME=<desc>` 生成的时间戳。DSN 默认从 `configs/<svc>.yaml` 的 `pgsql.write` 提取(值须带引号),部署侧/CI 显式注入 `MIGRATE_DSN` 环境变量覆盖;迁移是独立的部署步骤,与服务启动分离(避免多副本抢跑),多服务共用一库时**当前共用同一张 `goose_db_version`**——Makefile 的 migrate 目标不传 goose `-table`。后果:版本号在全库范围内比较,**给某个服务新建的迁移若时间戳早于另一服务已应用的迁移,会被当作乱序跳过**;用 `make migrate-create` 生成时间戳即可避免。要改成各服务独立版本表,除了改 Makefile 还需给已有部署做一次版本表数据迁移(否则已应用的迁移在新表里显示为待应用、重跑即冲突),不能只改一边。

## 服务开发流程(从零加一个服务的动线)

一个服务的产品落点(以 `<svc>` 代称):`api/<svc>/` proto 契约(`make proto` 生成到 `gen/` 的 pb、stub、gateway 转译与 OpenAPI spec)/ `cmd/<svc>/` 入口(`main.go` + `initial/` 装配)/ `configs/<svc>.yaml` 声明式配置 / `internal/<svc>/` 业务六件套(见上节)/ `migrations/<svc>/` goose 迁移(有库才有)。

`api/` 下还会有**外部契约镜像**(现有 `api/gateway/`:上游 gateway-backend 仓 proto 的镜像,只为生成 client stub 供本仓服务调用)。它不是本仓服务——没有 `cmd/`、`configs/`、`internal/` 落点,也不要给它补;上游契约变更时同步该 proto 文件。

1. **渲染入口**:用 `.agents/skills/new-service` 技能按变体(none/rpc/both)生成骨架与配置,随后按其清单登记错误码分段。
2. **契约先行**:在 `api/<svc>/` 写 proto,要 HTTP 出口就加 `google.api.http` 注解;注释格式按 `.agents/go-style.md`「proto 注释」(直接决定文档页的标题与字段说明),`make proto` 生成全部产物。
3. **分层实现**:按 model → repo → service → handler 的顺序写;各层职责见「服务分层」,错误流向见「错误处理约定」。
4. **建库**:`make migrate-create SVC=<svc> NAME=<desc>` 生成首个迁移,写好 Up/Down 后 `make migrate-up`;后续变更一律新增文件(其余约定见上节)。
5. **装配**:在 `cmd/<svc>/initial` 接线——createInfra 在前、createServer 在后;要 HTTP 出口就把 `WithGateway` 与 `http_addr` 一并放开(理由见「服务分层」的「能力与部署分离」段)。
6. **验证与运行**:`make check` 全绿后 `make run SVC=<svc>`,HTTP 出口 `/docs` 看接口文档。端到端脚本按服务放 `test/e2e/<svc>/run.sh`(自带迁移、测试数据写入、被测服务与下游 mock 的起停),用 `make e2e SVC=<svc>` 跑,前置是本地 Postgres + grpcurl + jq。
