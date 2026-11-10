# 服务分层与落地细则

> 这是 `AGENTS.md`「服务分层」的细则,**不常驻上下文,按需取**:改业务代码、加服务、
> 加模块、动装配或迁移之前读它。AGENTS.md 只留索引——有哪些层、两种布局怎么选、
> 层名是封闭集合、跳层依赖经接口。
>
> 布局本身由 `test/arch` 在 `make check` 里机械强制,所以读这份的目的是「一次写对」,
> 不是「不被抓到」。

## 包名与目录

**包名取层名,不取目录名**:`internal/<svc>/service/<module>/` 里的包声明为 `package service`。这是对 Go「包名随目录名」惯例的刻意例外——按目录名的话同一模块的四层全叫 `<module>`,模块内部 service 引用 model 就得起别名;取层名则模块内零别名(`model.Account`、`service.RegisterIn`),只有 `cmd/<svc>/initial` 里跨模块的同层包撞名,按下面的 `<模块><层>` 起即可。new-service/new-module 渲染的 `doc.go` 已按此声明。

**跨模块共用的东西只能上移到服务级共享包**:层目录本身不放 Go 文件、层内又不另建子包,所以各模块 repo 共用的错误哨兵、共用的鉴权拦截器之类没法待在层里,一律上移为 `internal/<svc>/<pkg>/`。这是层优先的代价,起名时按它提供的能力命名(如 `dberr`、`authn`),不要用层名。

## 平铺与分模块

模块内部的职责与错误流向同平铺——平铺服务就是只有一个模块、且各层不单列模块子目录。平铺服务要加第二个业务域时,先把现有代码迁进模块子目录(每层各建一个)再写新模块:越晚迁,要挪的代码越多。已分模块的服务加业务域用 `.agents/skills/new-module` 技能。

## 模块边界:
- 跨模块只经对方的 `service` 包:调用方在自己的 service 里声明需要的窄接口,`cmd/<svc>/initial` 接线时传入对方的实现;不引用别的模块的 `handler`、`repo`、`model`、`job`,对方 `service` 的子包也不行。
- 服务级共享包不引用任何模块(含它们的 `service` 包):各模块都依赖它,反过来依赖就会成环,业务逻辑也能借道共享包绕过模块边界。共享包确实需要某个模块的能力时,**声明窄接口并用自己的类型表达出入参**,由 initial 注入模块的实现——鉴权拦截器正是这个形状:它要按 token 查用户,却不能依赖 `auth` 模块,于是自己定义 `Verifier` 接口与 `Identity` 类型。
- **层内不另建子包**:层优先布局里 `repo/shared/` 与 `repo/order/` 同形、分不开,所以层目录的一级子目录只能是模块。模块内部要再分包,放进模块里(`repo/<module>/<pkg>/`)。
- 同一个服务要么全平铺、要么全分模块,不混用。

## test/arch 怎么识别布局

Go 的 `internal/` 只挡本仓库之外的引用,仓库内哪个包能引用哪个包编译器不管,所以这几条由 `test/arch` 的架构测试在 `make check` 里强制(同理,宪法第 4 条「凭证只写 `${ENV_NAME}` 引用」由 `test/secrets` 强制——两者都是普通 go test,因此自动落在提交门槛里)。它按目录识别:服务一级子目录里名字是层名的是层目录,`config` 与其余名字是服务级共享包;**模块集合由 `service` 层的子目录定义**——`service` 是模块唯一的对外入口,别的层可以缺某个模块的目录(不是每个模块都有 job),拿它们当来源会把「某模块还没有 repo」误判成布局问题。层目录里直接放 `.go` 即平铺、只放子目录即分模块;两者并存、某层直接放 `.go` 而服务已分模块、或层内出现不在模块集合里的子目录,都报违规。模块名是小写字母开头的字母数字串(`^[a-z][a-z0-9]*$`),不与层名、`config` 重名(否则渲染出 `handler/handler/`,且 initial 的包别名会成 `handlerhandler`),也不用 Go 工具会特殊对待的 `testdata`、`vendor`、`internal`;initial 里多个模块的同层包同名,按 `<模块><层>` 起别名(如 `orderservice`)。

## 跳层依赖

跳层依赖一律经接口:每一层只依赖**使用方自己声明**的窄接口,不依赖下一层的具体类型(go-style 硬规则 6「接口在使用方声明」)。`handler` 在自己包里声明它需要的用例接口、`service` 声明仓储接口、`job` 声明它需要的用例接口,跨模块与共享包→模块同理(见上「模块边界」);具体实现一律由 `cmd/<svc>/initial` 注入,构造器因此「收接口、返具体类型」。好处是每一层都能脱开下层单测;代价是只有一个实现的地方也要写一份接口声明——这是刻意接受的,不以「现在只有一个实现」为由省掉。

## 装配(`cmd/<svc>/initial`)

传输装配在 `cmd/<svc>/initial`,不进 internal:基础组件与业务组件分函数构造(createInfra:遥测/DB/Redis;createServer:传输、job),App 组装时基础在前业务在后——顺序即注册顺序,基础组件先起后停。装配时把 `tel.TracerProvider()` 与 `tel.MeterProvider()` 一并传给各组件(见上「遥测 Provider 要显式注入」),有依赖的服务再给传输组件加 `WithReadinessCheck`——装配期不 ping 库这个取舍的另一半就在这里。分模块的服务在 createServer 里逐个模块构造 repo → service → handler(各层的模块子包),再统一交给 transport(`WithService` 可多次调用,每个模块追加一次;双端口形态的服务 `WithGateway` 同样每个模块追加一次)。

## 传输形态

三个构造器(`NewRPCServer` 纯 gRPC / `NewRPCRESTServer` gRPC + gateway 双端口 /
`NewRESTServer` 直连 REST)的适用场景、各自必配哪些地址、错配时的装配期报错,
**全表在包注释里**:`go doc ./pkg/transport`。这里不复制一份——第二处真源不随
签名变(同 go-style「包的使用文档只有包注释这一处」)。

new-service 技能还没有直连 REST 变体,入口照 rpc 变体的骨架把 `transport.NewRPCServer` 换成 `transport.NewRESTServer`,并经 `WithDirectGateway` 注册 pb。

## 接口文档

接口文档:`make proto` 为带 `google.api.http` 注解的服务生成 OpenAPI v3 spec,HTTP 出口挂 `/docs` 即得阅读页;`openapi` 聚合包由 new-service 技能随首个 both 变体服务落地(模板在其 assets),机制见包注释。

## 数据库迁移

数据库迁移:SQL 迁移按服务放 `migrations/<svc>/`,goose 注解格式(一个文件一对 Up/Down),序号统一用 `make migrate-create SVC=<svc> NAME=<desc>` 生成的时间戳。DSN 默认从 `configs/<svc>.yaml` 的 `pgsql.write` 提取(值须带引号;其中的 `${ENV}` 引用由迁移 recipe 的 shell 在双引号内展开(shell 语义,`$NAME`、`${X:-d}` 也生效),未设置即为空串而非像服务启动那样 panic,靠库端认证失败兜底;提取时会剥掉 `statement_timeout`——服务配置里它是 10s 量级,而大表的 `CREATE INDEX`、回填动辄几分钟,带着它跑会被库端中断在半途),部署侧/CI 显式注入 `MIGRATE_DSN` 环境变量覆盖;迁移是独立的部署步骤,与服务启动分离(避免多副本抢跑),多服务共用一库时**当前共用同一张 `goose_db_version`**——Makefile 的 migrate 目标不传 goose `-table`。后果:版本号在全库范围内比较,**给某个服务新建的迁移若时间戳早于另一服务已应用的迁移,会被当作乱序跳过**;用 `make migrate-create` 生成时间戳即可避免。要改成各服务独立版本表,除了改 Makefile 还需给已有部署做一次版本表数据迁移(否则已应用的迁移在新表里显示为待应用、重跑即冲突),不能只改一边。
