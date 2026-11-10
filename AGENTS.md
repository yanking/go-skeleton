# AGENTS.md

> 本文件是给 AI 助手(以及新人)的项目导航与约束入口。

## 宪法
@CONSTITUTION.md

## 编码细则
@.agents/go-style.md

## 项目定位

Go 微服务 monorepo 骨架:业务单 `go.mod`(开发工具链另置 `tools/` 子 module,其依赖不进主依赖图)、proto 契约 + gRPC/grpc-gateway、结构化日志、可观测性基建。
它首先是**模板仓库**:一切改动都要经得起一个问题——"这个模式复制到第 10 个服务时还成立吗?"
新增服务用 `.agents/skills/new-service` 技能渲染入口模板,不要手抄。
服务可以只承载一个业务域(微服务),也可以承载多个(单体);两种形态的目录约定见「服务分层」,起服务时就要定。
拿本仓起新项目用 `.agents/skills/new-project` 技能(`make new-project MODULE=...`):module path 与模板身份散在三十余处,手工改必漏。

分支约定(判断"当前分支该有什么"看这里):

| 分支 | 内容 |
|---|---|
| `main` | 只有模板层,零业务服务(`api/`、`cmd/`、`configs/`、`internal/` 均不存在)——clone 默认拿到的就是它 |
| `feature/*` | 在建服务,业务层允许是半成品;模板层改动照样受"第 10 个服务"检验 |

模板层改动一律直接落在 `main`。随 `feature/*` 一并进来的模板层改动,合并前照样受「第 10 个服务」检验——`feature/*` 的业务层可以是半成品,模板层不行。

文档分布:

| 位置 | 内容 |
|---|---|
| `README.md` | 人类入口:快速开始与仓库导览 |
| `CONSTITUTION.md` / 本文件 / `.agents/` | 约束与导航;编码细则在 `.agents/go-style.md`,可执行技能在 `.agents/skills/` |
| `docs/<svc>/` | 服务的业务文档:架构、时序、接入指南;`diagrams/` 是 archify 渲染页 |
| `docs/deploy.md` | 部署参考:探针语义、宽限期预算、退出码、资源限制、凭证注入 |
| `docs/toolbox.md` | 仓库级工具参考(make 目标之外的工具,如 archify 画图) |

## 公共包地图(`pkg/`)

本表只回答「有没有、找谁」,不是使用说明:锁读纪律、停机顺序、各参数的零值语义、
装配期行为一律以包注释为准,按需取(取法见下)。唯一常驻的例外是本节末那条——
不打开该包也会踩的坑,才值得占每次会话的上下文。

| 包 | 领域 |
|---|---|
| `pkg/app` | 组件生命周期编排与停机预算分配 |
| `pkg/conf` | YAML 配置加载、绑定校验、`${NAME}` 环境变量展开 |
| `pkg/log` | 统一 slog.Logger 构造(含从 ctx 提取字段) |
| `pkg/errcode` | 业务错误码与错误的「双通道」封装 |
| `pkg/telemetry` | OTel 链路追踪与指标装配(采样、导出、运行时指标) |
| `pkg/mysql` | GORM MySQL 连接池与 dbresolver 读写分离 |
| `pkg/pgsql` | GORM PostgreSQL(pgx stdlib)连接池,与 `pkg/mysql` 同构 |
| `pkg/redis` | go-redis 客户端(单机/集群) |
| `pkg/httpc` | 出站 HTTP 客户端 |
| `pkg/transport` | 对外传输组件,三形态构造器(形态表见「服务分层」) |
| `pkg/queue` | asynq 任务队列:Client 入队 + Worker 消费(实现 app.Component) |

**包文档用 `go doc` 取,不要读源码文件**:`pkg/transport` 的非测试源码是 8.4 万字符
分在 5 个文件里,而 `go doc` 给出同一份约束不到 8 千——省一个数量级,且它就是那份
代码、不存在漂移。三档粒度:

| 命令 | 给什么 | 何时用 |
|---|---|---|
| `go doc ./pkg/<包>` | 包注释 + 导出项清单 | 默认动作:改用法、排查行为前 |
| `go doc ./pkg/<包>.<符号>` | 该符号的约束与示例 | 已知要动哪个构造器/选项/字段,最省 |
| `go doc -all ./pkg/<包>` | 全部导出项的完整注释 | 要通读整包时才用 |

读源码文件只在「要改这个包」时才需要。

**遥测 Provider 要显式注入**:本仓不设 otel 全局(理由见 `pkg/telemetry` 包注释),
`pkg/transport`、`pkg/mysql`、`pkg/pgsql`、`pkg/redis`、`pkg/httpc` 都从 Config 或选项
收 TracerProvider / MeterProvider。**漏注入不报错**——instrumentation 回落到 no-op 全局,
那部分遥测静默消失,而 RED 指标与连接池饱和度恰恰只能靠它们定位。各包因此在未拿到
Provider 时记一条「未启用」的装配日志,部署后扫一眼启动日志即可确认。

## 错误处理约定(改错误相关代码前必读)

不变量见宪法第 1、2 条;双通道封装(业务字段给客户端、原始错误链只进日志)与用法约定见 `pkg/errcode` 包注释。

逐层翻译的固定流向:repo 把可识别的底层错误翻译为 service 哨兵 → service 统一翻译为业务 errcode(底层错误用 Wrap 挂住 cause)→ 出口由 pkg/transport 统一打包(gRPC 拦截器把业务码放 status details,HTTP 经 gateway 渲染)。出口层按来源分三路:

| 来源 | 出口行为 |
|---|---|
| errcode(含 Wrap 结果) | 翻成 status,业务码进 details 的 ErrorInfo(带本仓归属域) |
| **本仓造出的** status(details 带本仓归属域) | 原样放行——这是有意给客户端看的消息 |
| 其余 status(漏翻译、下游带回)与裸错误 | **只保留状态码**,消息换通用内部错误消息、details 丢弃;ctx 超时/取消无论裸错误还是 status 形态都归一到 10005/10006 |

第三路是**白名单**:黑名单要穷举「哪些码不安全」,白名单只需说明「哪些安全」,后者才兜得住没想到的码。保留状态码是因为码携带重试语义,而泄漏发生在消息与 details 里(实测原样放行会带出 dial tcp 的内网地址、连接串凭证、内部报文上限)。丢 details 同样必要:下游的 ErrorInfo 带着它自己分段的业务码,透出去就是冒充本仓的码。

这是兜底,不是许可——每层仍要翻译相邻下层的错误。非业务路径(路径不存在、方法不允许、请求解析失败)由 gateway 的路由错误处理器翻成 10002/10001,HTTP 状态保住 404/405/400。

一处已知盲区:报文超限、编解码失败发生在 pb 生成代码的 dec() 阶段,比拦截链更早,既没有业务码也不进访问日志——那部分可观测性靠边缘网关或 mesh 的指标补。

错误码分段(新服务在此登记;分模块的服务再在本段内按模块划千位子段,写进归属列):

| 分段 | 归属 |
|---|---|
| 10000–19999 | `pkg/errcode` 通用码(已用:10001 参数错误、10002 资源不存在、10003 内部错误、10004 未认证、10005 请求超时、10006 请求已取消) |

## 服务分层(`internal/<svc>/`)

层(六件套):`config`(本服务配置结构体,绑定 `configs/<svc>.yaml`)/ `handler`(协议出口,薄壳,只调 service 并返回 errcode;仅 rpc/both 变体有)/ `service`(业务层,声明仓储接口——依赖倒置支点)/ `repo`(service 所声明接口的实现:GORM 仓储,以及 Redis、三方 HTTP、邮件等外部依赖的适配;ORM 与三方 SDK 类型不出本层)/ `model`(表模型,不出所属模块,出口转换由 handler 做)/ `job`(异步任务,实现 app.Component)。

布局按服务承载几个业务域选,起服务时就定(new-service 技能的 module 参数):

| 布局 | 适用 | 目录 |
|---|---|---|
| 平铺 | 只承载一个业务域(微服务的常态) | 各层直接放文件:`internal/<svc>/<层>/` |
| 分模块 | 承载多个业务域(单体,或已知还会加业务域) | **层优先**:模块是层内的一级子目录,`internal/<svc>/<层>/<module>/`;`config` 不分模块;跨模块共用的件放服务级共享包 `internal/<svc>/<pkg>/` |

**层名是封闭集合**(`handler`/`service`/`repo`/`model`/`job`),模块名是业务域名字:`internal/user/service/wallet/` 里 `service` 是层、`wallet` 是模块;`internal/user/auth/` 这种不叫层名的一级目录是服务级共享包。**跳层依赖一律经接口**——每层只依赖使用方自己声明的窄接口,具体实现由 `cmd/<svc>/initial` 注入(go-style 硬规则 6)。

**细则不常驻,按需取**:包名取层名的理由、模块边界四条、共享包上移、平铺↔分模块互转、`test/arch` 的识别规则与模块名正则、装配顺序、传输形态选择、接口文档、数据库迁移——全在 `.agents/service-layout.md`,改业务代码或加服务/模块前读它。布局本身由 `test/arch` 在 `make check` 里强制(同理宪法第 4 条「凭证只写 `${ENV_NAME}`」由 `test/secrets` 强制:两者都是普通 go test,自动落在提交门槛里)。

## 服务开发流程(从零加一个服务的动线)

一个服务的产品落点(以 `<svc>` 代称):`api/<svc>/` proto 契约(`make proto` 生成到 `gen/` 的 pb、stub、gateway 转译与 OpenAPI spec)/ `cmd/<svc>/` 入口(`main.go` + `initial/` 装配)/ `configs/<svc>.yaml` 声明式配置 / `internal/<svc>/` 业务分层(平铺或分模块,见上节)/ `migrations/<svc>/` goose 迁移(有库才有)。

1. **渲染入口**:先按「服务分层」定布局,再用 `.agents/skills/new-service` 技能按变体(none/rpc/both)生成骨架与配置——分模块的服务给出首个模块名;随后按其清单登记错误码分段。已分模块的服务加业务域用 `.agents/skills/new-module`。
2. **契约先行**:在 `api/<svc>/` 写 proto,要 HTTP 出口就加 `google.api.http` 注解;注释格式按 `.agents/go-style.md`「proto 注释」(直接决定文档页的标题与字段说明),`make proto` 生成全部产物。
3. **分层实现**:按 model → repo → service → handler 的顺序写;各层职责见「服务分层」,错误流向见「错误处理约定」。**每层先在自己包里声明它需要的下层接口,再写实现**(「跳层依赖一律经接口」)。分模块的服务逐个模块写,跨模块调用守「服务分层」的模块边界。
4. **建库**:`make migrate-create SVC=<svc> NAME=<desc>` 生成首个迁移,写好 Up/Down 后 `make migrate-up`;后续变更一律新增文件(其余约定见 `.agents/service-layout.md`)。
5. **装配**:在 `cmd/<svc>/initial` 接线——createInfra 在前、createServer 在后;both 变体在业务 pb 就绪后把构造器换成 `NewRPCRESTServer`,连同 `WithGateway` 与 `http_addr` 在同一次改动里放开(形态与地址错配即装配期报错,形态全表见 `go doc ./pkg/transport`)。
6. **验证与运行**:`make check` 全绿后 `make run SVC=<svc>`,HTTP 出口 `/docs` 看接口文档。端到端脚本按服务放 `test/e2e/<svc>/run.sh`(自带迁移、测试数据写入、被测服务与下游 mock 的起停),用 `make e2e SVC=<svc>` 跑,前置条件以该脚本的头部注释为准。
7. **上线**:`make docker SVC=<svc>` 出镜像,部署侧的探针、宽限期预算与凭证注入按 `docs/deploy.md`——那几项配错的后果(liveness 指向就绪检查、宽限期装不下停机两段)都是发布时才显形的。
