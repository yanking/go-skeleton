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
| `example` | 模板层 + 示例服务;模板层与 `main` 逐字一致,改了模板层就要同步过去 |
| `feature/*` | 在建服务,业务层允许是半成品;模板层改动照样受"第 10 个服务"检验 |

模板层同步:改动优先直接落在 `main`,再 `git merge main` 带进 `example`——两支共享历史,同步成本恒定。反方向(模板层改动先落在 `example`,如随 `feature/*` 一并进来)只能把那几个提交 cherry-pick 到 `main`:`merge example` 会把业务层也带过去。

文档分布:

| 位置 | 内容 |
|---|---|
| `README.md` | 人类入口:快速开始与仓库导览 |
| `CONSTITUTION.md` / 本文件 / `.agents/` | 约束与导航;编码细则在 `.agents/go-style.md`,可执行技能在 `.agents/skills/` |
| `docs/<svc>/` | 服务的业务文档:架构、时序、接入指南;`diagrams/` 是 archify 渲染页 |
| `docs/deploy.md` | 部署参考:探针语义、宽限期预算、退出码、资源限制、凭证注入 |
| `docs/toolbox.md` | 仓库级工具参考(make 目标之外的工具,如 archify 画图) |
| `docs/superpowers/{specs,plans}/` | 按日期归档的技术方案与实施计划 |

## 公共包地图(`pkg/`)

| 包 | 职责 |
|---|---|
| `pkg/app` | 组件生命周期编排:按注册顺序拉起、逆序停止 |
| `pkg/conf` | YAML 配置加载与绑定校验;`${NAME}` 引用在解析前从环境变量展开,未设置或写法不合文法即 panic |
| `pkg/log` | 构造统一 slog.Logger,支持从 ctx 提取 trace_id 等字段 |
| `pkg/errcode` | 业务错误码与错误的「双通道」封装 |
| `pkg/telemetry` | OTel 链路追踪与指标一键装配,stdout/otlp 双导出,含运行时指标;采样策略可配(生产须显式配比例采样),SDK 内部错误桥到 slog |
| `pkg/mysql` | GORM MySQL 连接池,dbresolver 自动读写分离,内嵌 `*gorm.DB` 直接调用 |
| `pkg/pgsql` | GORM PostgreSQL(pgx stdlib)连接池,与 `pkg/mysql` 同构 |
| `pkg/redis` | go-redis 客户端,单机/集群经 addrs 数量自动推断,内嵌 `UniversalClient` 直接调用 |
| `pkg/httpc` | 出站 HTTP 客户端:按体形态给方法(JSON/表单/原文体)与 Get,超时逐调用可覆盖,响应体有大小上限,连接池逐 Client 独立;注入 TracerProvider 即埋出站 span 并透传 traceparent,注入 Logger 即逐次记出站日志(url 只留 scheme://host/path,报文不记) |
| `pkg/transport` | 对外传输组件,三种形态构造器:`NewRPCServer` 纯 gRPC / `NewRPCRESTServer` gRPC + gateway 环回双端口 / `NewRESTServer` 直连 REST(pb 契约直出 HTTP,无环回,不支持流式方法)。拦截链含 errcode 出口翻译、处理超时、访问日志、panic 兜底与鉴权(otel 经 StatsHandler/otelhttp 挂载),链序见包注释;带请求体与连接数上限、摘流窗口与 liveness/readiness 两个探针 |
| `pkg/queue` | asynq 任务队列薄封装:Client 入队 + Worker 消费(实现 app.Component) |

本表只是索引;使用约束(锁读纪律、停机顺序、全局状态、装配期行为)以各包 Go doc 注释为准,改用法前先读。

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

目录(六件套):`config`(本服务配置结构体,绑定 `configs/<svc>.yaml`)/ `handler`(协议出口,薄壳,只调 service 并返回 errcode;仅 rpc/both 变体有)/ `service`(业务层,声明仓储接口——依赖倒置支点)/ `repo`(service 所声明接口的实现:GORM 仓储,以及 Redis、三方 HTTP 接口、邮件等外部依赖的适配;ORM 与三方 SDK 类型不出本层)/ `model`(表模型,不出所属模块,出口转换由 handler 做)/ `job`(异步任务,实现 app.Component)。

布局按服务承载几个业务域选,起服务时就定(new-service 技能的 module 参数):

| 布局 | 适用 | 目录 |
|---|---|---|
| 平铺 | 只承载一个业务域(微服务的常态) | 各层直接放文件:`internal/<svc>/<层>/` |
| 分模块 | 承载多个业务域(单体,或已知还会加业务域) | **层优先**:模块是层内的一级子目录,`internal/<svc>/<层>/<module>/`;`config` 不分模块,仍在 `internal/<svc>/config`;跨模块共用的件(如鉴权拦截器)放服务级共享包 `internal/<svc>/<pkg>/` |

**层名是封闭集合**(`handler`/`service`/`repo`/`model`/`job`),模块名是业务域名字。所以 `internal/user/service/wallet/` 里 `service` 是层、`wallet` 是模块;`internal/user/auth/` 这种不叫层名的一级目录是服务级共享包,不是层也不是模块。

**包名取层名,不取目录名**:`internal/<svc>/service/<module>/` 里的包声明为 `package service`。这是对 Go「包名随目录名」惯例的刻意例外——按目录名的话同一模块的四层全叫 `<module>`,模块内部 service 引用 model 就得起别名;取层名则模块内零别名(`model.Account`、`service.RegisterIn`),只有 `cmd/<svc>/initial` 里跨模块的同层包撞名,按下面的 `<模块><层>` 起即可。new-service/new-module 渲染的 `doc.go` 已按此声明。

**跨模块共用的东西只能上移到服务级共享包**:层目录本身不放 Go 文件、层内又不另建子包,所以各模块 repo 共用的错误哨兵、共用的鉴权拦截器之类没法待在层里,一律上移为 `internal/<svc>/<pkg>/`。这是层优先的代价,起名时按它提供的能力命名(如 `dberr`、`authn`),不要用层名。

模块内部的职责与错误流向同平铺——平铺服务就是只有一个模块、且各层不单列模块子目录。平铺服务要加第二个业务域时,先把现有代码迁进模块子目录(每层各建一个)再写新模块:越晚迁,要挪的代码越多。已分模块的服务加业务域用 `.agents/skills/new-module` 技能。

模块边界:
- 跨模块只经对方的 `service` 包:调用方在自己的 service 里声明需要的窄接口,`cmd/<svc>/initial` 接线时传入对方的实现;不引用别的模块的 `handler`、`repo`、`model`、`job`,对方 `service` 的子包也不行。
- 服务级共享包不引用任何模块(含它们的 `service` 包):各模块都依赖它,反过来依赖就会成环,业务逻辑也能借道共享包绕过模块边界。共享包确实需要某个模块的能力时,**声明窄接口并用自己的类型表达出入参**,由 initial 注入模块的实现——鉴权拦截器正是这个形状:它要按 token 查用户,却不能依赖 `auth` 模块,于是自己定义 `Verifier` 接口与 `Identity` 类型。
- **层内不另建子包**:层优先布局里 `repo/shared/` 与 `repo/order/` 同形、分不开,所以层目录的一级子目录只能是模块。模块内部要再分包,放进模块里(`repo/<module>/<pkg>/`)。
- 同一个服务要么全平铺、要么全分模块,不混用。

Go 的 `internal/` 只挡本仓库之外的引用,仓库内哪个包能引用哪个包编译器不管,所以这几条由 `test/arch` 的架构测试在 `make check` 里强制(同理,宪法第 4 条「凭证只写 `${ENV_NAME}` 引用」由 `test/secrets` 强制——两者都是普通 go test,因此自动落在提交门槛里)。它按目录识别:服务一级子目录里名字是层名的是层目录,`config` 与其余名字是服务级共享包;**模块集合由 `service` 层的子目录定义**——`service` 是模块唯一的对外入口,别的层可以缺某个模块的目录(不是每个模块都有 job),拿它们当来源会把「某模块还没有 repo」误判成布局问题。层目录里直接放 `.go` 即平铺、只放子目录即分模块;两者并存、某层直接放 `.go` 而服务已分模块、或层内出现不在模块集合里的子目录,都报违规。模块名是小写字母开头的字母数字串(`^[a-z][a-z0-9]*$`),不与层名、`config` 重名(否则渲染出 `handler/handler/`,且 initial 的包别名会成 `handlerhandler`),也不用 Go 工具会特殊对待的 `testdata`、`vendor`、`internal`;initial 里多个模块的同层包同名,按 `<模块><层>` 起别名(如 `orderservice`)。

跳层依赖一律经接口:每一层只依赖**使用方自己声明**的窄接口,不依赖下一层的具体类型(go-style 硬规则 6「接口在使用方声明」)。`handler` 在自己包里声明它需要的用例接口、`service` 声明仓储接口、`job` 声明它需要的用例接口,跨模块与共享包→模块同理(见上「模块边界」);具体实现一律由 `cmd/<svc>/initial` 注入,构造器因此「收接口、返具体类型」。好处是每一层都能脱开下层单测;代价是只有一个实现的地方也要写一份接口声明——这是刻意接受的,不以「现在只有一个实现」为由省掉。

传输装配在 `cmd/<svc>/initial`,不进 internal:基础组件与业务组件分函数构造(createInfra:遥测/DB/Redis;createServer:传输、job),App 组装时基础在前业务在后——顺序即注册顺序,基础组件先起后停。装配时把 `tel.TracerProvider()` 与 `tel.MeterProvider()` 一并传给各组件(见上「遥测 Provider 要显式注入」),有依赖的服务再给传输组件加 `WithReadinessCheck`——装配期不 ping 库这个取舍的另一半就在这里。分模块的服务在 createServer 里逐个模块构造 repo → service → handler(各层的模块子包),再统一交给 transport(`WithService` 可多次调用,每个模块追加一次;双端口形态的服务 `WithGateway` 同样每个模块追加一次)。

传输形态由构造器决定,配置地址须与形态匹配、错配即装配期报错(`pkg/transport` 包注释有全表):

| 构造器 | 形态 | 适用 |
|---|---|---|
| `NewRPCServer` | 纯 gRPC,只配 `grpc_addr` | 东西向内部服务 |
| `NewRPCRESTServer` | gRPC + gateway 双端口,`grpc_addr`/`http_addr` 都必配 | 以 gRPC 为业务协议、再要一个 HTTP 转译出口(环回打回本进程,拦截链两种协议共用,业务代码只写一遍) |
| `NewRESTServer` | 直连 REST,只配 `http_addr` | 对外纯 HTTP、proto 契约直出(pb 的 `RegisterXxxHandlerServer` 直接调 handler,无环回无第二份编解码,适合高 QPS/大报文);不支持流式方法,无 gRPC 端口 |

两个端口各自监听:暴露范围、mesh 协议标注与网络策略可分别配置。形态是服务的结构性决策,同一份二进制不按部署切换——要改形态是代码改动。

new-service 技能还没有直连 REST 变体,入口照 rpc 变体的骨架把 `transport.NewRPCServer` 换成 `transport.NewRESTServer`,并经 `WithDirectGateway` 注册 pb。

接口文档:`make proto` 为带 `google.api.http` 注解的服务生成 OpenAPI v3 spec,HTTP 出口挂 `/docs` 即得阅读页;`openapi` 聚合包由 new-service 技能随首个 both 变体服务落地(模板在其 assets),机制见包注释。

数据库迁移:SQL 迁移按服务放 `migrations/<svc>/`,goose 注解格式(一个文件一对 Up/Down),序号统一用 `make migrate-create SVC=<svc> NAME=<desc>` 生成的时间戳。DSN 默认从 `configs/<svc>.yaml` 的 `pgsql.write` 提取(值须带引号;其中的 `${ENV}` 引用由迁移 recipe 的 shell 在双引号内展开(shell 语义,`$NAME`、`${X:-d}` 也生效),未设置即为空串而非像服务启动那样 panic,靠库端认证失败兜底),部署侧/CI 显式注入 `MIGRATE_DSN` 环境变量覆盖;迁移是独立的部署步骤,与服务启动分离(避免多副本抢跑),多服务共用一库时**当前共用同一张 `goose_db_version`**——Makefile 的 migrate 目标不传 goose `-table`。后果:版本号在全库范围内比较,**给某个服务新建的迁移若时间戳早于另一服务已应用的迁移,会被当作乱序跳过**;用 `make migrate-create` 生成时间戳即可避免。要改成各服务独立版本表,除了改 Makefile 还需给已有部署做一次版本表数据迁移(否则已应用的迁移在新表里显示为待应用、重跑即冲突),不能只改一边。

## 服务开发流程(从零加一个服务的动线)

一个服务的产品落点(以 `<svc>` 代称):`api/<svc>/` proto 契约(`make proto` 生成到 `gen/` 的 pb、stub、gateway 转译与 OpenAPI spec)/ `cmd/<svc>/` 入口(`main.go` + `initial/` 装配)/ `configs/<svc>.yaml` 声明式配置 / `internal/<svc>/` 业务分层(平铺或分模块,见上节)/ `migrations/<svc>/` goose 迁移(有库才有)。

1. **渲染入口**:先按「服务分层」定布局,再用 `.agents/skills/new-service` 技能按变体(none/rpc/both)生成骨架与配置——分模块的服务给出首个模块名;随后按其清单登记错误码分段。已分模块的服务加业务域用 `.agents/skills/new-module`。
2. **契约先行**:在 `api/<svc>/` 写 proto,要 HTTP 出口就加 `google.api.http` 注解;注释格式按 `.agents/go-style.md`「proto 注释」(直接决定文档页的标题与字段说明),`make proto` 生成全部产物。
3. **分层实现**:按 model → repo → service → handler 的顺序写;各层职责见「服务分层」,错误流向见「错误处理约定」。**每层先在自己包里声明它需要的下层接口,再写实现**(「跳层依赖一律经接口」)。分模块的服务逐个模块写,跨模块调用守「服务分层」的模块边界。
4. **建库**:`make migrate-create SVC=<svc> NAME=<desc>` 生成首个迁移,写好 Up/Down 后 `make migrate-up`;后续变更一律新增文件(其余约定见上节)。
5. **装配**:在 `cmd/<svc>/initial` 接线——createInfra 在前、createServer 在后;both 变体在业务 pb 就绪后把构造器换成 `NewRPCRESTServer`,连同 `WithGateway` 与 `http_addr` 在同一次改动里放开(形态与地址错配即装配期报错,见「服务分层」的形态表)。
6. **验证与运行**:`make check` 全绿后 `make run SVC=<svc>`,HTTP 出口 `/docs` 看接口文档。端到端脚本按服务放 `test/e2e/<svc>/run.sh`(自带迁移、测试数据写入、被测服务与下游 mock 的起停),用 `make e2e SVC=<svc>` 跑,前置条件以该脚本的头部注释为准。
7. **上线**:`make docker SVC=<svc>` 出镜像,部署侧的探针、宽限期预算与凭证注入按 `docs/deploy.md`——那几项配错的后果(liveness 指向就绪检查、宽限期装不下停机两段)都是发布时才显形的。
