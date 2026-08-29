# channel 技术架构

> 配图：[diagrams/architecture.html](diagrams/architecture.html)（archify 生成，浏览器打开可交互；
> 图源 `diagrams/architecture.json`，改图方式见 [../toolbox.md](../toolbox.md)）。

## 总体形态

channel 是一个**纯 gRPC 东西向服务**（`transport.grpc_addr :9092`，无 `http_addr`），
上游是 gateway-backend。核心形态五条：

- **部署**：单服务单 pod，承载全部渠道商户实例。
- **商户配置**：PostgreSQL `channels` 表一行一商户（通用元数据 + `platform` 私有配置），
  改库后至多 60s 生效，无需重启。
- **渠道差异**：每个渠道一个 `adapter/<渠道>` 实现包，service 的 `builders()` 构造表按
  `channel_name` 装配实例。
- **元数据分发**：gateway 直连 channel，调 `ListChannels` 拉全量元数据做路由与限额。
- **补单对账**：`reconcile_enabled` 行级开关，统一 reconcile job 轮询，按渠道需要启用。

## 进程内分层

依赖方向自上而下，依赖倒置支点是 service 声明的 `ChannelRepo` 接口：

| 层 | 职责 | 关键文件 |
|---|---|---|
| handler | 协议出口薄壳：proto ↔ 协议中立类型互转，只调 service 并返回 errcode | `internal/channel/handler/grpc.go` |
| service | 业务层：内存路由表（加载/重载/lookup）、适配哨兵 → 业务 errcode 翻译 | `internal/channel/service/channel.go` |
| repo | `ChannelRepo` 的 GORM 实现，`LoadAll` 全量加载，ORM 不出层 | `internal/channel/repo/channel.go` |
| model | `channels` 表模型，不出服务边界 | `internal/channel/model/channel.go` |
| adapter | 渠道对接层：接口 + 5 个失败哨兵（出站 HTTP 经注入的 `pkg/httpc` 客户端，带链路追踪） | `internal/channel/adapter/` |
| adapter/payapay、adapter/neokred | 每渠道一个实现包，实现 `adapter.Adapter` 八方法 | 对应子包 |
| job | reconcile 补单对账，实现 `app.Component`（Start 阻塞轮询） | `internal/channel/job/reconcile.go` |
| gateway_client | gateway-rpc 直连客户端（insecure，TLS 由 mesh 保障），job 专用 | `internal/channel/gateway_client/client.go` |

## 装配与生命周期

装配在 `cmd/channel/initial/init_app.go`，按「基础在前、业务在后」注册组件，
`pkg/app` 按注册顺序拉起、逆序停止——基础组件先起后停，业务停机期仍有遥测与数据着落：

```
createInfra:  telemetry → pgsql                      （基础）
createServer: service（首载路由表）→ [gatewayclient + gwCloser + reconcile job] → transport.Server
```

装配期的三个「起不来就死」：路由表首次加载失败（DB 不可达）、gateway-rpc 客户端建连失败、
端口监听失败。`configs/channel.yaml` 的 `gateway.addr` 留空则对账 job 整体不装配——
纯回调驱动形态（如 payapay）零开销。

## 路由表机制（service 核心）

- **数据结构**：`instances map[routeKey]*instance`，routeKey 为三元组拼串；instance = General 元数据 + Adapter 实例 + 补单开关。
- **构造表**：`builders()` 维护 `channel_name → NewFunc`，新渠道在此登记一行。放 service 而非 adapter 包，避免 adapter 反向依赖实现包。
- **加载**：启动 `New()` 内全量 `LoadAll`，逐行 `NewFunc(platform JSON)` 构造实例。**单行失败只跳过并告警**——DB 里登记尚未上线的渠道是常态，一行脏数据不拖死服务。
- **重载**：每次请求 `lookup` 前检查 TTL（60s），到期全量重建；重载失败沿用旧表。
- **未命中**：三元组缺字段 → 10001；查无实例 → 40001。

## 适配层设计

`adapter.Adapter` 八方法（Name + 代收/代付 × 下单/查询/回调 + 余额）是渠道的完整抽象。
构造函数收 `platform` JSON 原文，实现自行反序列化——结构因渠道而异，框架不解释。

**失败哨兵**（`adapter.go`）是 adapter → service 的唯一错误语言，service `translate` 固定翻译：

| 哨兵 | 翻译为 |
|---|---|
| `ErrChannelRejected` | 40002 |
| `ErrVerifyFailed` | 40003 |
| `ErrUnknownCallbackStatus` | 40004 |
| `ErrBadResponse` | 40005 |
| `ErrCallbackUnsupported` | 10001 |
| （其余） | 10003 |

错误码语义与典型来源见 [README.md](README.md) 错误码表。

两个存量渠道的形态对比（新渠道接入前先对号入座）：

| | payapay | neokred |
|---|---|---|
| 鉴权 | MD5 签名（key 排序拼 `k=v&…&key=secret`） | 下单：`client_secret`/`program_id` 头；查询/余额：Dashboard 邮箱登录换 Bearer token（401 单飞刷新，`singleflight` 防重复登入） |
| API 面 | 单 base_url + 路径，业务码 `code=200` | 渠道 API + Dashboard API 两套，业务码 `statusCode=200` |
| 金额单位 | 分（直传） | 元（两位小数字符串，出入互转） |
| 回调 | 有，body JSON 重算 MD5 验签 | 无，`ErrCallbackUnsupported`，状态只经查询轮询 |
| 查询状态映射 | 私有 2→成功 3→失败，其余处理中 | 代收仅认成功清单、失败刻意映射为**处理中**（刻意语义）；代付未知状态按失败计 |
| 代付出款 | 固定 BANK/INR_BANK/IFSC | IMPS，超十万（分单位 10000000）自动 RTGS；去 91 区号前缀 |

状态与回调金额字段的完整映射表见 [sequences.md](sequences.md)。

## 数据模型（`channels` 表）

- 唯一约束 `(channel_name, merchant_no, currency)` 即路由三元组。
- 通用列（限额、费率、回调约定、`payout_supports`）+ `platform JSONB`（渠道私有配置原文）+ `reconcile_enabled`。
- 迁移在 `migrations/channel/`（goose，`make migrate-create/up/down SVC=channel`）；
  种子迁移仅本地演示值，密钥管理见 [new-payment-channel.md](new-payment-channel.md) 第 1 步。

## 传输与可观测性（只记 channel 特有决策，机制见 `pkg/transport`、`pkg/telemetry` 包注释）

- 内网东西向服务**不挂鉴权**；收紧时加 `WithAuthenticator`。
- 业务错误打包进 gRPC status details（`ErrorInfo.reason` 放业务码），gateway 侧按码处理。
- 链路 handler → service → adapter → `pkg/httpc` 出站 span 全程贯穿，下游渠道调用可见。
