# 项目架构

> 本文档定义脚手架的目标形态，是落地实现时的对照契约；任何偏离走宪法第三条。仓库形态：**多服务 monorepo**，每个服务独立成套、可独立编译与部署。

## 总览（单服务视角）

每个服务一个二进制、双协议、pb-first：

```
HTTP/JSON :8080              gRPC :9090
     │                          │
grpc-gateway ── 环回 gRPC ──▶ gRPC Server ◀── StatsHandler 观测（otelgrpc，链外接入）
                                │  拦截器链：日志 → recovery → 鉴权 → 参数校验
                                ▼
                            service    实现 pb 生成的 Server 接口；pb ↔ 领域类型转换
                                ▼
                            biz        领域逻辑；定义仓储等外设接口
                                ▲ 实现（依赖倒置）
                            data       数据库 / 缓存 / 下游服务客户端
```

- 同一进程开两个端口：`:9090` 纯 gRPC；`:8080` HTTP，由 grpc-gateway 经环回连接转发到 gRPC 端口（端口按服务经配置区分）。
- 为什么走环回而不用 `RegisterXxxHandlerServer` 进程内直连：直连会绕过 gRPC 拦截器链，鉴权、日志就得在 HTTP 侧再实现一遍。环回让横切关注点只写一次。

## 目录契约

```
.
├── .agent/                   # AI 规范文档层：架构 / 编码规范 / 工程规范
├── api/                      # 协议层（对外事实源）
│   └── {service}/v1/         # 按服务 + 版本分目录
│       ├── {service}.proto   # 含 google.api.http 注解与 protovalidate 校验注解
│       └── *.pb.go 等        # 生成物，随 proto 同提交，禁手改
├── buf.yaml                  # buf 模块与 lint / breaking 配置
├── buf.gen.yaml              # 生成插件配置
├── Makefile                  # 命令契约唯一载体（定义见 engineering.md）
├── .golangci.yml             # 静态检查配置（含 generated 排除，见 go-style.md 适用范围）
├── .gitignore                # 忽略 bin/ 与 .claude/（settings.json 除外，报告与备份不入库）
├── go.mod / go.sum           # 单一 module，多服务共用
├── cmd/{service}/            # main：读配置、造根 ctx、构造组件并注入，交由 pkg/app 编排启停
├── configs/{service}.yaml    # 每服务一份配置，经 pkg/conf.MustLoad 绑定到结构体
├── migrations/{service}/     # golang-migrate 纯 SQL 迁移文件（工作流见 engineering.md）
├── docs/                     # 业务与产品文档：人类主编，AI 仅在用户明示时写入；AI 规范勿入
├── internal/
│   └── {service}/            # 每服务一套四层，服务间禁止互相 import
│       ├── server/           # 薄装配层：构造 service 实现、组好注册闭包与自有拦截器，交给 pkg/transport
│       ├── service/          # 实现 pb 生成的 {Service}Server；出入参转换与映射错误码
│       ├── biz/              # 领域逻辑；定义仓储接口；不感知传输与存储
│       └── data/             # 实现 biz 的接口；连接收在未导出字段的 Data 结构里
├── pkg/                      # 跨服务共享的领域无关工具（可被外部仓库引用；谨慎准入，禁止业务逻辑）
│   ├── app/                  # 生命周期编排：Run(ctx) 按序拉起、逆序停止
│   ├── conf/                 # 配置加载：MustLoad(configFile, obj)
│   ├── log/                  # 日志构造：MustNew(Config) 出 *slog.Logger
│   ├── telemetry/            # 可观测性：MustNew(ctx, Config) 出 trace/metric provider
│   ├── transport/            # 传输层：MustNew(ctx, Config) 出双协议组件，Components() 交给 app
│   ├── mysql/                # MySQL 连接池：MustNew(ctx, Config) 出内嵌 *sql.DB 的 Client
│   ├── postgres/             # PostgreSQL 连接池：同上，走 pgx/v5/stdlib
│   └── redis/                # Redis 连接池：MustNew(ctx, Config) 出内嵌 *goredis.Client 的 Client
├── openapi/                  # 生成的 OpenAPI 文档，禁手改
└── bin/                      # make build 产物，不入库
```

## 依赖方向（红线）

服务内：`cmd → server → service → biz ← data`；cmd 是装配根，不被任何层 import。

| 层 | 可以 import | 不可以 import |
|---|---|---|
| cmd | 本服务 server、service、biz、data（仅装配期构造与注入） | 其他服务的任何层 |
| server | 本服务 service、本服务 api(pb) | biz、data |
| service | 本服务 api(pb)、本服务 biz | data |
| biz | 本服务内不 import 任何其他层 | api(pb)、service、data、任何存储驱动 |
| data | 本服务 biz（为实现其接口）、下游服务的 api(pb 客户端桩) | service、server |

要点：

- 本表只约束服务内部层与层之间、以及跨服务的可见性；标准库与 `pkg/`（领域无关工具）对所有层开放，不必逐行列举。
- pb 类型止步于 service 层，biz 只见领域类型；转换代码写在 service。
- biz 定义接口、data 实现（依赖倒置），装配在 cmd 用构造函数手工注入，不引 DI 框架。
- data 层用一个**字段全部未导出**的 `Data` 结构持有本服务全部存储连接，由 cmd 在装配期以 `pkg/{mysql,postgres,redis}` 的内嵌句柄（`db.DB`、`rdb.Client`）构造；各 repository 一律只收 `*Data`，于是新增存储组件只需改 `Data` 与 cmd 两个文件，已有 repository 一个字都不用动。
  - 字段必须未导出：这是「biz 不得绕过仓储直接查库」这条红线的**编译期**保障，包外访问会直接得到 `cannot refer to unexported field`。改成导出字段（或跨层的 svcCtx）能再省一处改动，但等于把这条路重新打开，不划算。
  - 往下传的是 `*sql.DB` / `*goredis.Client` 这类标准或第三方类型，**不是** `*mysql.Client`：后者带着 `Start`/`Stop`，传给 repository 等于把关停全服务连接池的能力也给了它；收标准类型还让 data 层对 `pkg/` 零依赖，测试时直接给一个自开的 `*sql.DB` 即可。
  - `NewData` 用位置参数，不用选项模式：加组件时两者改动处数相同（选项模式只是把「改签名」换成「加一个 `WithXxx` 函数」，cmd 那行照样要改），而位置参数多买到一条——漏传直接编译不过，选项模式漏写 `WithXxx` 要到第一个查询才 nil panic。存储真涨到 5 个以上再换 `Deps` 结构体并补 nil 校验 panic，但那时更该先问边界是不是划错了。
  - `Data` **不能**内嵌各存储的 Client：三个包的类型都叫 `Client`，内嵌即 `Client redeclared`；退而内嵌底层句柄则 `d.Close()` 是 `ambiguous selector`，且 `Start`/`Stop`/`Name` 会被提升，让 `Data` 意外满足 `app.Component`。
- **跨服务红线**：服务间禁止 import 彼此的 `internal/{service}`；跨服务调用一律走对方 pb 接口，gRPC 客户端归调用方 data 层持有。共享代码只能进 `pkg/`，且必须领域无关、不含业务逻辑。

## 协议层规则

- 目录即版本：`api/{service}/v1/`；破坏性变更开 `v2/`，v1 按弃用周期维护。
- 每个 RPC 必须写 `google.api.http` 注解——HTTP 路由在 proto 里就是文档。
- 路由风格：资源名词复数 + 标准方法（`GET /v1/users/{id}`、`POST /v1/users`）；自定义动作用 `POST /v1/users/{id}:activate` 形式。
- 错误模型：service 层把 biz 错误集中映射为 `google.golang.org/grpc/status`（codes + errdetails），gateway 自动转 HTTP 状态码；错误码映射表随 service 维护，只在这一处翻译。
- 参数校验：proto 层用 protovalidate 注解声明，拦截器统一执行；service 只做注解表达不了的业务校验。
- 健康检查由 `pkg/transport` 统一提供：gRPC Health v1 复用 grpc-go 官方 `grpc_health_v1`（不复制进 `api/`），整进程报一个总状态；HTTP 侧 `GET /healthz` 在 gateway mux 上以代码注册，只表示「进程活着」（liveness），「是否可接流量」（readiness）看 gRPC Health v1——此为宪法第一条的基础设施例外，不进 `openapi/`。

## 横切关注点

- 传输层装配唯一入口 `pkg/transport.MustNew(ctx, Config)`：双端口监听、gateway 环回、gRPC Health v1、`GET /healthz`、通用拦截器、otelgrpc 接入、三个 `app.Component` 与优雅停机全在里面。服务只经 Config 提供三样东西：`RegisterGRPC`（注册自己的 pb Server 实现）、`RegisterGateway`（直接填生成的 `RegisterXxxHandler`）、`Interceptors`（自有拦截器，如鉴权）。`internal/{service}/server` 因此退化为薄装配层，不再重复实现传输机制。
- 注册函数收在 `Config` 里而非事后暴露 `*grpc.Server`：gRPC 的 `RegisterService` 在 `Serve` 之后调用会直接 panic，这个顺序约束不该压给使用方——收进 Config 后由 `MustNew` 内部保证，压根没有写错的形状。
- 拦截器链顺序：**日志 → recovery → 鉴权 → 参数校验**。日志必须在 recovery **外层**：反过来的话，handler 的 panic 会从日志拦截器内部穿过去，它 `handler()` 之后的记录代码根本没机会执行——凡是 panic 的 RPC 都不会留下访问日志，错误率被静默低估（此前文档写的是 recovery 在外，落地时被用例抓出，已按实际修正）。代价是日志拦截器自身的 panic 不被兜住，但那是 `pkg/transport` 自己的几行代码、不接触用户输入。
- 访问日志分级按状态码：OK → Info；`Internal`／`Unknown`／`DataLoss` 是服务端自己出问题、需人介入 → Error；其余（多为客户端传参不对）→ Warn，不该拿去告警。`grpc.health.v1.Health/*` 降到 Debug——探针每几秒来一次，打 Info 会把真实日志淹没。
- 观测**不在拦截器链上**：`otelgrpc.NewServerHandler` 是 `stats.Handler`，经 `grpc.StatsHandler` 注册，先于整条拦截器链触发（日志拦截器因此总能拿到已开启的 span）。改为自实现拦截器属选型变更，走宪法第三条。
- 可观测性构造唯一入口 `pkg/telemetry.MustNew(ctx, Config)`，产出 trace 与 metric 两套 provider ＋ W3C 传播器，并注册 Go runtime 指标（goroutine 数、GC、堆）。导出方式经配置三选一：`otlp`（OTLP/gRPC 发往 collector）、`stdout`（本地调试，同步导出立刻可见）、`none`（零值，测试与本地默认，全局保持 noop、零开销）。日志不走 OTLP——容器里经 stdout 交采集侧，靠 `pkg/log` 打进每条日志的 `trace_id` 与链路关联。
- 采样固定用 `ParentBased(TraceIDRatioBased(ratio))`，不用裸 ratio：上游已决定采样的请求必须跟随其决定，否则同一条链路会在服务边界断成两截。`SampleRatio` 零值取 1（全采）；要关闭遥测用 `Exporter=none`，不要把采样率设 0——关闭只有一个开关。
- 即便 `Exporter=none` 也会设置传播器：本服务不采样，但上游传来的 trace context 仍须原样透传给下游（noop tracer 会保留 ctx 中的 SpanContext），否则链路在这个服务这里断开。
- `pkg/telemetry` 是 `pkg/app` 的资源型组件（`Start` 直接返回 nil，`Stop` 逆序 Shutdown 并 flush），靠结构化接口自动满足 `app.Component`，**不 import `pkg/app`**。不 Shutdown 就会丢掉最后一批 span 与 metric，把它做成组件正是为了让 cmd 忘不掉这一步。
- 埋点库须显式注入 provider（`otelgrpc.WithTracerProvider(tel.TracerProvider())`、`WithMeterProvider`、`WithPropagators`），不依赖全局；全局只为第三方库兜底，设置权归 `pkg/telemetry`，见 `go-style.md` 的红线例外。
- 指标命名受 OTel 约束：必须 ASCII 字母开头，中文会被 SDK 当场拒绝（`invalid instrument name`）。span 名无此限制。
- 鉴权拦截器带按完整方法名（`info.FullMethod`）的放行清单：`grpc.health.v1.Health/*` 默认在列；`/healthz` 同理不做业务鉴权。
- 元端点清单（宪法第一条例外的全集）：`GET /healthz`。新增须经用户批准并同步更新本清单。
- 日志：`log/slog` 结构化输出。分级：Debug=开发排查细节；Info=正常业务里程碑（默认级）；Warn=可自愈异常或降级；Error=需人介入的失败。公共字段统一 `trace_id`、`service`、`method`。请求出入口日志由拦截器统一打，业务代码不重复打「进入 / 退出」。
- Logger 构造唯一入口 `pkg/log.MustNew(Config)`：`service` 由 Config 必填项写入每条日志；格式（json 默认 / text）、级别、是否带调用点均经配置可调。服务配置结构体里的字段直接写 `slog.Level` 与 `log.Format`，二者都实现了 `encoding.TextUnmarshaler`，YAML 写 `level: info`（大小写不敏感，还支持 `info+2` 偏移）、`format: json` 即可绑定，拼错在 `conf.MustLoad` 阶段就报错，不必等到 `MustNew`。`Config.Level` 收的是 `slog.Leveler`（`slog.Level` 天然满足），要运行期调级则由调用方自持 `*slog.LevelVar` 传入，`pkg/log` 不代管该变量、也不提供调级端点（加端点须先扩元端点清单）。构造出的 Logger 由 cmd 注入各组件与 `pkg/app.Config.Logger`；本包不调 `slog.SetDefault`，是否接管全局由 cmd 决定。
- 随请求变化的字段（`trace_id`、`user_id` 等）经 `pkg/log.Extractor`（`func(ctx) []slog.Attr`）注入，在 cmd 装配期注册。因此 `pkg/log` 不依赖 OpenTelemetry：接 OTel 那轮在 `server` 层写一个读 `trace.SpanContextFromContext` 的 Extractor 注册进去即可，`pkg/log` 一行不改。Extractor 契约：不 panic、不阻塞、不在内部再打日志（会无限递归）；Handler 不做防御性 recover——它跑在日志热路径上且是装配期自己写的代码，出问题应当场暴露而非静默丢字段。
- 包装 `slog.Handler` 时若内嵌 `slog.Handler` 而不覆写 `WithAttrs` / `WithGroup`，派生 Logger 会退回底层 Handler、包装静默失效（日志照打、不报错）。`pkg/log` 已覆写并有用例锁住，自行再包 Handler 时须照做。另注意：`WithGroup` 之后 Extractor 追加的字段会落进该组内而非顶层，公共字段应在未开组的 Logger 上打。
- 优雅退出：编排唯一实现在 `pkg/app`。cmd 用 `signal.NotifyContext` 监听 SIGINT/SIGTERM 造出根 ctx 传给 `app.Run(ctx)`；app 只对 ctx 取消做反应，不监听信号、不自造根 ctx。
- 组件契约：`Start(ctx)` 阻塞运行常驻循环（`return srv.Serve(ln)` 即可，不必自起 goroutine、不必自建错误上报 channel），`Stop(ctx)` 让它停下；无常驻循环的资源型组件（data、gateway 环回 ClientConn）`Start` 直接返回 nil。监听端口在装配期建好（传输层由 `pkg/transport.MustNew` 内部 `net.Listen`，起不来当场 panic），不放进 `Start`——app 按注册顺序拉起，但不判断也不等待就绪。
- 组件无需过滤 `http.ErrServerClosed` / `grpc.ErrServerStopped`：只有 app 知道停机是不是它自己发起的，故停机期收到的 `Start` 返回值一律按预期处理，只有非停机期的非 nil 返回才算致命错误。这条不能反过来压给组件——没有信息的一方判断不了。
- 停机顺序：注册顺序即拉起顺序、其逆序即停止顺序。约定按 `telemetry → data（mysql / postgres / redis 等连接池）→ gRPC → gateway 环回 ClientConn → HTTP` 注册，于是停机为「停 HTTP → 关 gateway 环回 ClientConn → `GracefulStop` gRPC → 关闭 data 资源 → 最后关 telemetry」。telemetry 排最前（= 最后停）是有意的：前面所有组件停机期间产生的 span 与 metric 都还能被记录，并在最后一步 flush 出去；反过来注册的话，停机过程本身就是黑的，而那恰恰是最需要看清楚的时候。`pkg/app` 收的是变长组件切片、不校验顺序，写反了编译期与运行期都不报错，仍须 cmd 遵守本约定。
- 停机总超时由全部组件共享（非每组件），默认 30s 经配置可调，对齐 K8s `terminationGracePeriodSeconds` 的默认值；**该值必须小于部署侧给进程的宽限期，且要留余量**——两者取等意味着停机刚跑满，收尾日志与进程退出还没做完就被 SIGKILL，所以 Deployment 那边应配成比它大几秒（如 40s）。某组件 `Stop` 在宽限期内没返回，app 放弃等待、继续停下一个（被放弃的 `Stop` goroutine 就此漏下，进程正在退出，代价可接受）；剩余组件仍会被调用 `Stop`，只是拿到已过期的 ctx，应立即强制终止（gRPC 即 `GracefulStop` 转 `Stop()`）。跳过等于资源不释放。
- 组件意外退出（非停机期 `Start` 返回非 nil，如监听器被意外关闭）触发同一套停机流程，并由 `Run` 返回该错误；cmd 据此以非零码退出（`if err := app.Run(ctx); err != nil { os.Exit(1) }`），避免「端口已死、进程还活着」。
- Component 适配器归属：传输组件（gRPC、gateway 环回 ClientConn、HTTP）由 `pkg/transport.Components()` 统一导出并已排好序，cmd 直接 append；`data` 出资源组件（连接池等）；cmd 只负责把 telemetry、data 与传输组件按约定顺序拼起来。
- 存储连接构造在 `pkg/{mysql,postgres,redis}`，**只出连接、不出仓储**：仓储属 data 层，由它实现 biz 定义的接口。三个包各自导出 `MustNew(ctx, Config) *Client`，`Client` 内嵌底层句柄（`*sql.DB` / `*goredis.Client`），故 `QueryContext`、`Get` 等方法可直接调用，需要原始句柄时取内嵌字段；方法集满足 `app.Component`，连接池由 cmd 注册进 app，忘不掉关。
- 三个存储包**刻意不合并**：`database/sql` 驱动靠 blank import 的 `init()` 注册，合成一个包会让只用 MySQL 的服务白白链上 pgx——实测二进制 7.2MB → 13.5MB。代价是 `pkg/mysql` 与 `pkg/postgres` 有约 70 行同形代码，这份重复是为「每个服务只付自己用到的那份」买的单。
- PostgreSQL 出口是 `*sql.DB`（走 `pgx/v5/stdlib`）而非 `*pgxpool.Pool`：与 MySQL 形状一致，data 层写法互通，golang-migrate 与 sqlc 也直接可用。代价是拿不到 `COPY FROM`、`LISTEN/NOTIFY` 等 PG 独有能力——真需要时经 `c.Conn(ctx)` 再 `conn.Raw(...)` 取底层 pgx 连接，不是死路。
- 存储连接在装配期探活并重试：`sql.Open` 不建立任何连接，DSN 或密码配错时服务会「启动成功」，K8s 认为 Pod 已就绪并开始导流，直到第一个请求才报错；而探一次就死又会因为「服务与数据库同时启动」的几秒时间差白白 CrashLoopBackOff。故在 `ConnectTimeout`（默认 5s）窗口内每 200ms 重试一次，耗尽才 panic。注意 `database/sql` 的 `Ping` 自身完全不重试（实测连接被拒 182µs 即返回），而 go-redis 内部自带重试、实测能扛约 1.7s——写相关用例时延迟要设在这个阈值之上，否则测到的是 go-redis 而不是本包。
- 存储埋点经 `otelsql`（SQL）与 `redisotel`（Redis）挂上，provider 显式注入不依赖全局；连接池指标用 `otelsql.RegisterDBStatsMetrics` 注册，`Stop` 必须先注销回调再关连接池——反过来的话连接池已关而回调仍在，下一次采集会读到已关闭的 DB。SQL 查询 span 名为 `sql.conn.query`。
- `pkg/` 内部允许单向依赖，当前只有 `transport → app`（`Components()` 的返回类型）。其余跨包协作一律用结构化接口：`pkg/telemetry` 不 import `pkg/app` 却满足 `app.Component`，`pkg/transport` 不 import `pkg/telemetry` 却能收它——各方只需知道自己需要的方法集，不必知道对方存在。新增 `pkg/` 内部依赖须走宪法第三条。
