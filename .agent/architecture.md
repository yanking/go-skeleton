# 项目架构

> 本文档定义脚手架的目标形态，是落地实现时的对照契约；任何偏离走宪法第三条。仓库形态：**多服务 monorepo**，每个服务独立成套、可独立编译与部署。

## 总览（单服务视角）

每个服务一个二进制、双协议、pb-first：

```
HTTP/JSON :8080              gRPC :9090
     │                          │
grpc-gateway ── 环回 gRPC ──▶ gRPC Server ◀── StatsHandler 观测（otelgrpc，链外接入）
                                │  拦截器链：recovery → 日志 → 鉴权 → 参数校验
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
│       ├── server/           # 传输装配：gRPC server、gateway mux、拦截器注册
│       ├── service/          # 实现 pb 生成的 {Service}Server；出入参转换与映射错误码
│       ├── biz/              # 领域逻辑；定义仓储接口；不感知传输与存储
│       └── data/             # 实现 biz 的接口：数据库、缓存、下游服务客户端
├── pkg/                      # 跨服务共享的领域无关工具（可被外部仓库引用；谨慎准入，禁止业务逻辑）
│   ├── app/                  # 生命周期编排：Run(ctx) 按序拉起、逆序停止
│   ├── conf/                 # 配置加载：MustLoad(configFile, obj)
│   └── log/                  # 日志构造：MustNew(Config) 出 *slog.Logger
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
- **跨服务红线**：服务间禁止 import 彼此的 `internal/{service}`；跨服务调用一律走对方 pb 接口，gRPC 客户端归调用方 data 层持有。共享代码只能进 `pkg/`，且必须领域无关、不含业务逻辑。

## 协议层规则

- 目录即版本：`api/{service}/v1/`；破坏性变更开 `v2/`，v1 按弃用周期维护。
- 每个 RPC 必须写 `google.api.http` 注解——HTTP 路由在 proto 里就是文档。
- 路由风格：资源名词复数 + 标准方法（`GET /v1/users/{id}`、`POST /v1/users`）；自定义动作用 `POST /v1/users/{id}:activate` 形式。
- 错误模型：service 层把 biz 错误集中映射为 `google.golang.org/grpc/status`（codes + errdetails），gateway 自动转 HTTP 状态码；错误码映射表随 service 维护，只在这一处翻译。
- 参数校验：proto 层用 protovalidate 注解声明，拦截器统一执行；service 只做注解表达不了的业务校验。
- 健康检查：实现 gRPC Health v1 协议（复用 grpc-go 官方 `grpc_health_v1`，不复制进 `api/`）；HTTP 侧 `GET /healthz` 在 gateway mux 上以代码注册——此为宪法第一条的基础设施例外，不进 `openapi/`。

## 横切关注点

- 拦截器链统一注册在 `internal/{service}/server`，顺序：recovery → 日志 → 鉴权 → 参数校验。
- 观测**不在拦截器链上**：`otelgrpc.NewServerHandler` 是 `stats.Handler`，经 `grpc.StatsHandler` 注册，先于整条拦截器链触发（日志拦截器因此总能拿到已开启的 span）。改为自实现拦截器属选型变更，走宪法第三条。
- 鉴权拦截器带按完整方法名（`info.FullMethod`）的放行清单：`grpc.health.v1.Health/*` 默认在列；`/healthz` 同理不做业务鉴权。
- 元端点清单（宪法第一条例外的全集）：`GET /healthz`。新增须经用户批准并同步更新本清单。
- 日志：`log/slog` 结构化输出。分级：Debug=开发排查细节；Info=正常业务里程碑（默认级）；Warn=可自愈异常或降级；Error=需人介入的失败。公共字段统一 `trace_id`、`service`、`method`。请求出入口日志由拦截器统一打，业务代码不重复打「进入 / 退出」。
- Logger 构造唯一入口 `pkg/log.MustNew(Config)`：`service` 由 Config 必填项写入每条日志；格式（json 默认 / text）、级别、是否带调用点均经配置可调。服务配置结构体里的字段直接写 `slog.Level` 与 `log.Format`，二者都实现了 `encoding.TextUnmarshaler`，YAML 写 `level: info`（大小写不敏感，还支持 `info+2` 偏移）、`format: json` 即可绑定，拼错在 `conf.MustLoad` 阶段就报错，不必等到 `MustNew`。`Config.Level` 收的是 `slog.Leveler`（`slog.Level` 天然满足），要运行期调级则由调用方自持 `*slog.LevelVar` 传入，`pkg/log` 不代管该变量、也不提供调级端点（加端点须先扩元端点清单）。构造出的 Logger 由 cmd 注入各组件与 `pkg/app.Config.Logger`；本包不调 `slog.SetDefault`，是否接管全局由 cmd 决定。
- 随请求变化的字段（`trace_id`、`user_id` 等）经 `pkg/log.Extractor`（`func(ctx) []slog.Attr`）注入，在 cmd 装配期注册。因此 `pkg/log` 不依赖 OpenTelemetry：接 OTel 那轮在 `server` 层写一个读 `trace.SpanContextFromContext` 的 Extractor 注册进去即可，`pkg/log` 一行不改。Extractor 契约：不 panic、不阻塞、不在内部再打日志（会无限递归）；Handler 不做防御性 recover——它跑在日志热路径上且是装配期自己写的代码，出问题应当场暴露而非静默丢字段。
- 包装 `slog.Handler` 时若内嵌 `slog.Handler` 而不覆写 `WithAttrs` / `WithGroup`，派生 Logger 会退回底层 Handler、包装静默失效（日志照打、不报错）。`pkg/log` 已覆写并有用例锁住，自行再包 Handler 时须照做。另注意：`WithGroup` 之后 Extractor 追加的字段会落进该组内而非顶层，公共字段应在未开组的 Logger 上打。
- 优雅退出：编排唯一实现在 `pkg/app`。cmd 用 `signal.NotifyContext` 监听 SIGINT/SIGTERM 造出根 ctx 传给 `app.Run(ctx)`；app 只对 ctx 取消做反应，不监听信号、不自造根 ctx。
- 组件契约：`Start(ctx)` 阻塞运行常驻循环（`return srv.Serve(ln)` 即可，不必自起 goroutine、不必自建错误上报 channel），`Stop(ctx)` 让它停下；无常驻循环的资源型组件（data、gateway 环回 ClientConn）`Start` 直接返回 nil。监听端口在装配期建好（cmd 里 `net.Listen`，起不来当场 panic），不放进 `Start`——app 按注册顺序拉起，但不判断也不等待就绪。
- 组件无需过滤 `http.ErrServerClosed` / `grpc.ErrServerStopped`：只有 app 知道停机是不是它自己发起的，故停机期收到的 `Start` 返回值一律按预期处理，只有非停机期的非 nil 返回才算致命错误。这条不能反过来压给组件——没有信息的一方判断不了。
- 停机顺序：注册顺序即拉起顺序、其逆序即停止顺序。约定按 `data → gRPC → gateway 环回 ClientConn → HTTP` 注册，于是停机为「停 HTTP → 关 gateway 环回 ClientConn → `GracefulStop` gRPC → 关闭 data 资源」。`pkg/app` 收的是变长组件切片、不校验顺序，写反了编译期与运行期都不报错，仍须 cmd 遵守本约定。
- 停机总超时由全部组件共享（非每组件），默认 10s 经配置可调。某组件 `Stop` 在宽限期内没返回，app 放弃等待、继续停下一个（被放弃的 `Stop` goroutine 就此漏下，进程正在退出，代价可接受）；剩余组件仍会被调用 `Stop`，只是拿到已过期的 ctx，应立即强制终止（gRPC 即 `GracefulStop` 转 `Stop()`）。跳过等于资源不释放。
- 组件意外退出（非停机期 `Start` 返回非 nil，如监听器被意外关闭）触发同一套停机流程，并由 `Run` 返回该错误；cmd 据此以非零码退出（`if err := app.Run(ctx); err != nil { os.Exit(1) }`），避免「端口已死、进程还活着」。
- Component 适配器归属：由组件所属层自行导出——`server` 出传输组件（gRPC、HTTP、gateway 环回 ClientConn），`data` 出资源组件（连接池等），cmd 只负责按顺序注册；适配器可 import `pkg/app`。
