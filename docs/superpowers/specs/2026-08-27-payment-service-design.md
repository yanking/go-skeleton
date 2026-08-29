# payment 服务设计（代收闭环）

> 2026-08-27 定稿。在 go-skeleton 骨架上新建 payment 微服务，贯通「商户 → payment → channel → 三方渠道」
> 的代收闭环。本文是实现的唯一依据；与代码冲突时，实现期以本文修订为准（修订须同步本文）。

## 0. 已拍板决策（不可在实现期重开）

| 决策点 | 结论 |
|---|---|
| 本轮范围 | 代收闭环：下单 → 选路 → 调 channel → 回调/补单终态 → 商户异步通知；实现 `gateway.v1.GatewayService` 两个 RPC 供 channel 补单对账 |
| 商户契约 | proto 先行的全新设计 |
| 异步通知机制 | asynq（Redis）队列，新增 `pkg/queue` 公共包 |
| 选路 | 静态绑定 + 顺序 failover；商户指定 `channel_name` 则在其绑定内按指定渠道走，未指定按 priority 全量候选 |
| 存储 | PostgreSQL（沿用 `pkg/pgsql`） |
| 流程 | superpowers（本 spec → writing-plans → 实现） |

## 1. 定位与边界

```
商户 ──HTTP/JSON(验签)──▶ payment ──gRPC──▶ channel ──HTTP──▶ 三方渠道
三方 ──HTTP 回调──▶ payment /callbacks/*（原生 HTTP 面）──转发──▶ channel 验签
channel 补单 job ──gRPC(gateway.v1)──▶ payment（拉未完结单 / 回推终态）
payment ──asynq worker──▶ 商户 notify_url（终态异步通知）
```

- payment 持有：商户、订单、选路、状态机、通知。channel 持有：渠道差异（签名/报文/状态映射）。
- channel 零改动：payment 实现仓内既有的 `api/gateway/v1/gateway.proto` 契约，
  `configs/channel.yaml` 的 `gateway.addr` 指向 payment 的 gRPC 地址即接通补单对账。

### 非目标（本轮不做）

代付、商户五科目账务与结算、余额查询、收银台 H5、管理台、黑名单风控、UTR 尾数/附言匹配、
通道分组与统计类路由策略。

### 已知简化（显式假设，非遗漏）

- 商户限额与费率不分币种（单币种运营前提；多币种时再拆商户-币种维度配置）。
- `channel_level` / `deeplink` / `timeout` 本轮不对商户暴露，调 channel 时按零值透传；
  渠道要求的付款人信息（如 INR 的姓名/手机号/邮箱）缺失时表现为该实例下单失败进入 failover。

## 2. 服务形态

| 项 | 决定 |
|---|---|
| 变体 | `both`：gRPC `:9093`（东西向，含 gateway.v1）+ HTTP `:8093`（北向：商户 API 转译、`/callbacks/*`、`/docs`） |
| 三方回调入口 | `transport.WithMount("/callbacks/", …)` 原生 HTTP——回调报文格式由三方决定，必须保留原始 header+body，不经 gateway 转译 |
| 错误码段 | 50000–59999，登记进 AGENTS.md 错误码分段表 |
| 新公共包 | `pkg/queue`：asynq 薄封装，零业务概念（宪法第 3 条） |
| 文档同步 | AGENTS.md（错误码表、pkg 地图加 `pkg/queue`）与 `docs/payment/` 服务文档随实现落地（宪法第 6 条） |

## 3. 商户契约（`api/payment/v1/payment.proto`）

### RPC 与 HTTP 映射（全 POST，签名统一）

| RPC | HTTP | 用途 |
|---|---|---|
| `CreatePaymentOrder` | `POST /v1/payment/orders` | 代收下单，返回 `order_no + pay_url` |
| `QueryPaymentOrder` | `POST /v1/payment/orders/query` | 查单，`mch_order_no` / `order_no` 至少其一 |
| `ListAvailableChannels` | `POST /v1/payment/channels/query` | 商户可指定的渠道清单（channel_name 去重 + 限额区间）——指定通道能力的配套 |

### 下单字段（proto 定稿以实现为准，语义不变）

必填：`app_id, mch_order_no, amount(分,int64), currency, timestamp(毫秒), sign`；
可选：`channel_name`（指定通道）、`notify_url`（空则不通知）、`payer_name / payer_phone / payer_email`（是否必填由渠道决定，透传 channel）。
出参：`order_no, pay_url`。查单出参：`order_no, mch_order_no, status(1 处理中 2 成功 3 失败), amount, fee, reference_no, completed_at`。

### 签名规范

1. 参与字段 = 请求消息全部标量字段（含**空值**——空值不参与会留下参数剥离重放的攻击面），`sign` 自身除外。
2. 字段名 ASCII 升序，拼 `k=v&k=v…`（末尾无 `&`）；值的规范形式 = protojson 线上形态
   （int64 十进制字面量、bool `true/false`、未设置字段取 proto3 零值形态 `0/false/空串`）——
   商户按 JSON 报文字面拼即得同一结果，服务端从 proto 字段重算。
3. `sign = hex_lower(HMAC-SHA256(app_secret, 拼串))`——不用 MD5、不用密钥后缀拼接。
4. `timestamp` 与服务器时间偏差 > 5 分钟拒绝。
5. 验签位置：service 层统一入口（每个 RPC 字段集不同且需查商户密钥，不适合 transport 拦截器）；
   验签前置于一切业务逻辑，未验签请求不产生 DB 写与下游调用。

商户侧鉴权顺序：查商户（app_id）→ IP 白名单（**逐项精确匹配**，禁用 `strings.Contains`）→
时间戳窗口 → 验签 → 商户状态。三个查询 RPC 同走此链；查单必须校验订单归属请求商户，
`order_no` 命中他人订单按 10002 处理（不泄露存在性）。客户端 IP 取 HTTP 入口经 gateway
透传 metadata 的 `x-forwarded-for` 首跳，信任边界为前置 LB（部署要求：`http_addr` 仅可信代理可达）。

### 错误码分配（50000–59999）

| 码 | 含义 | gRPC 状态 |
|---|---|---|
| 50001 | 商户订单号重复 | AlreadyExists |
| 50002 | 金额超出商户或渠道限额 | InvalidArgument |
| 50003 | 指定渠道未绑定或不可用 | FailedPrecondition |
| 50004 | 无可用渠道（候选全部失败） | Unavailable |
| 50005 | 订单状态冲突（回推/回调与当前状态矛盾且不可收敛） | FailedPrecondition |
| 50006 | 商户状态受限（封禁/停用） | PermissionDenied |

复用通用码：10001 参数错误、10002 资源不存在（查单未命中）、10003 内部错误、
10004 未认证（app_id 未命中 / 验签失败 / IP 不在白名单 / 时间戳过期——对外统一不区分失败原因，防探测）。

## 4. 数据模型（PostgreSQL，`migrations/payment/`）

| 表 | 关键列 | 约束/说明 |
|---|---|---|
| `merchants` | `app_id, app_secret, name, status(1 正常 2 封禁), ip_whitelist(text[]), limit_min/limit_max(分), fee_rate(千分位), fee_extra(分)` | `app_id` 唯一；无管理台，数据运维直接入库，种子迁移仅本地演示值（同 channel 惯例，密钥不进仓库） |
| `channel_instances` | `channel_name, merchant_no, currency, enabled, limit_payment_min/max, callback_headers(text[]), callback_data_source, callback_return, callback_ip_whitelist` | 三元组唯一；由 `ListChannels` 同步 upsert，同步中消失的行置 `enabled=false` 不删除 |
| `merchant_channels` | `merchant_id, channel_instance_id, priority, enabled` | `(merchant_id, channel_instance_id)` 唯一；priority 越小越优先 |
| `payment_orders` | `order_no, merchant_id, mch_order_no, amount, currency, fee, status, channel_instance_id, out_order_no, reference_no, pay_url, notify_url, response(渠道原文), completed_at, notify_status, notified_at` | `order_no` 唯一；`(merchant_id, mch_order_no)` 唯一；普通索引 `(channel_instance_id, status, created_at)` 服务补单拉单与 sweep |
| `callbacks` | `channel_instance_id, source(1 HTTP 回调 2 补单回推), headers(jsonb), query, body, ip, status(1 已收到 2 已验证 3 无效), order_no, note` | 原文无条件落库，HTTP 回调与补单回推同表留痕——状态变更来源全程可审计 |
| `order_notifications` | `order_no, attempt, response_code, response_body(截断 500)` | 每次通知尝试一行留痕 |

- `order_no` 生成：`P + UnixMilli + 6 位随机数字`，唯一索引兜底，冲突重试一次；仅标准库，不引 ID 生成依赖。
- 费用快照：下单时按 `merchants.fee_rate/fee_extra` 计算 `fee = round(amount*rate/1000) + extra` 存单，后改费率不影响已有单。
- 渠道实例同步：启动全量拉取（失败则起不来，同 channel 路由表纪律）+ 定时同步（周期 `channel.sync_interval` 配置，默认 5m）。

## 5. 订单状态机

```
created ──候选实例下单成功──▶ sent ──成功回调/查询/回推──▶ success（终态）
   │                          └──失败回调/回推──▶ failed ──后到成功且金额一致──▶ success
   └──候选全部失败──▶ failed
```

转移表（事件 × 当前状态 → 动作；未列出的组合 = 不迁移，落 `callbacks` 表标无效 + Warn 告警）：

| 事件 \ 当前 | created | sent | failed | success |
|---|---|---|---|---|
| 派单成功 | → sent（**条件更新** `WHERE status=created`，已被并发回调推进则保持回调结果） | — | — | — |
| 派单全败 | → failed（商户已同步收到 50004，`notify_status=4` 不再异步通知） | — | — | — |
| 成功回调/回推（金额相等） | → success + 触发通知（派单中途宕机后三方仍可能实付） | → success + 触发通知 | → success + 触发通知（先失败后成功，真实渠道行为） | 重复，忽略 |
| 成功回调/回推（金额不符） | 不迁移，标无效 + 告警留人工 | 同左 | 同左 | 同左 |
| 失败回调/回推 | → failed + 触发通知 | → failed + 触发通知 | 重复，忽略 | **不反转**：标无效 + 告警留人工 |
| created 滞留超 30 分钟（order-sweep job） | → failed + 触发通知（宕机残留，商户未收到同步应答） | — | — | — |

三个关键设计决策：

1. **先落单再调渠道**：`created` 落库成功后才逐实例派单——任何时刻三方侧有单则我方必有记录，不存在孤儿订单。
2. **success 不可回退**：本轮无账务，自动反转只会向商户发出矛盾通知；矛盾回调留痕告警走人工。
3. **金额严格相等**：渠道确认金额与订单金额不符即挂告警留人工，不设任何容差。

并发与幂等：回调/回推处理在事务内 `SELECT … FOR UPDATE` 锁订单行，按转移表收敛；
重复报文照常落 `callbacks` 表（审计）但不重复触发终态与通知；下单幂等由 `(merchant_id, mch_order_no)`
唯一约束保证，重复直接 50001（不做「同参返回原单」的宽松语义）。

created 滞留的成因与收敛：派单中途宕机（channel 已受理但订单未更到 sent）会留下 created 单——
回调可直接推进它（见转移表首列）；order-sweep 30 分钟兜底置 failed；被 sweep 误杀的实付单
仍经 failed→success 收敛（回调或补单回推）。三条路互为兜底，不存在永久「处理中」。

## 6. 选路与派单

```
候选 = merchant_channels(merchant_id, enabled) JOIN channel_instances(enabled)
if 请求带 channel_name:
    候选 = 候选中该渠道的实例；空 → 50003
候选按 priority 升序:
    跳过 currency 不符 / 金额超实例限额
剩余为空 → 指定渠道时 50003，否则 50002（限额筛空）
逐实例调 channel.PaymentOrder(route三元组, order_no, amount, payer信息, notify_url):
    成功（拿到 pay_url）→ 订单 → sent（条件更新，见 §5）→ 记录实例快照，返回
    失败 → Warn 日志，试下一个
全部失败 → 订单 → failed，返回 50004
```

- `notify_url`（给三方的回调地址）= `callback_base_url + /callbacks/payment/ + channel_instance_id`（`callback_base_url` 为配置项）。
- 商户限额（`merchants.limit_*`）在下单入口校验，早于选路。
- 不做错误率统计、摩除、权重——行为完全由绑定表决定，可预测可解释；统计类策略等真实流量后按需叠加。

## 7. 三方回调链路（`/callbacks/payment/{channel_instance_id}`，POST 与 GET）

1. 原生 HTTP handler 收请求：读全部 header、query、body、client IP。
2. 原文落 `callbacks` 表（`status=已收到`）——落库失败即 5xx（三方会重发，宁重不丢）；
   **先落库再做任何校验**，被拒的攻击流量同样留痕可查。
3. 查 `channel_instances` 行；按行上快照做**回调 IP 白名单**（精确匹配，空即不校验）；
   不符 → 标无效 + 告警，应答 `403`（真实三方变更出口 IP 时靠告警发现）。
4. 按实例快照抽取 `callback_headers` 指定的头、按 `callback_data_source` 取 body 或 query 为 data，
   调 `channel.PaymentCallback(route, header, data)` 验签并映射。
5. channel 返回 `{order_no, out_order_no, callback_type, amount, reference_no}` → 进状态机（§5）。
6. 应答三方：处理成功回 `200 + callback_return`（实例快照里的渠道约定应答串）；
   验签失败/状态机标无效回 `200 + callback_return`（防三方无限重发，异常已留痕告警）；
   我方内部错误（DB 不可达等）回 `500`，等三方重发。
7. 全程一条 trace：mount handler 起 span → channel gRPC → 状态机 DB 事务。

## 8. 补单对账（实现 `gateway.v1.GatewayService`）

| RPC | 实现语义 |
|---|---|
| `TripartiteUnfinishedOrders` | 按三元组反查 `channel_instances` → 返回该实例窗口期（`payment_period` 分钟）内 `status IN (created, sent)` 的代收单（`order_no, out_order_no, amount`；created 是宕机残留，`out_order_no` 为空，channel 按 `order_no` 查）；`payouts` 恒空 |
| `TripartiteOrderCallback` | 落 `callbacks` 表（`source=补单回推`）→ 走 §5 同一状态机。返回值语义：状态机收敛（含幂等重复）→ 成功；不可收敛（金额不符/矛盾状态）→ 落库告警后**也返回成功**（防 channel 无限重推，异常留人工）；仅基础设施错误（DB 不可达等）→ 返回 gRPC error，channel 下一轮重试 |

## 9. 商户异步通知（asynq）

- **触发**：订单进入终态的同一事务置 `notify_status=1 待通知`（`notify_url` 为空则置 4 无需通知）；
  事务提交后入队 asynq 任务（payload 只带 `order_no`，内容发送时现查——防旧数据）。
- **发送**：POST `notify_url`，30s 超时；通知体 `{order_no(商户单号), sys_order_no, status(枚举同查单出参), amount, fee, reference_no, timestamp, sign}`，签名同 §3 规范（HMAC-SHA256）。
- **成功判定**：HTTP 200 且 body 忽略大小写等于 `success` → `notify_status=3 已送达` + `notified_at`。
- **重试**：asynq 默认指数退避，`MaxRetry=15`；每次尝试写 `order_notifications` 一行。
- **幂等**：任务处理前查订单，`notify_status=3/4` 直接完成；重复任务无副作用。
- **兜底**：notify-sweep job（`app.Component`，每 5m）重新入队满足「`notify_status=1` 且（无任何尝试记录且终态超 10m，或最后一次尝试距今超 2h）」的单——订单行即简化 outbox，「事务提交成功但入队失败」不丢通知；条件带「最后尝试时间」是为了不与 asynq 自身的退避重试叠加出重复任务。
- **重复容忍**：重试与兜底并发下商户仍可能收到重复通知（内容一致），商户端须按 `(order_no, status)` 幂等——写进商户接入文档。

## 10. `pkg/queue` 设计（asynq 薄封装）

```go
// 生产侧
type Client interface {
    Enqueue(ctx, typename string, payload []byte, opts ...Option) error  // Option: MaxRetry / ProcessIn / Queue
    Close() error
}
// 消费侧：Worker 实现 app.Component；Handler 注册进内部 mux
func NewWorker(cfg Config, logger *slog.Logger) *Worker
func (w *Worker) Handle(typename string, h func(ctx context.Context, payload []byte) error)
```

- 配置：`addrs / password / db / concurrency`；Redis 连接由 asynq 自管（不复用 `pkg/redis` 句柄），
  部署要求（独立实例、noeviction、AOF）写进包注释与 yaml 注释。
- 返回 error 即重试、nil 即完成——错误语义与宪法第 1 条一致；不包 asynq 全部旋钮，够 payment 用即止。
- 零业务概念；payment 的任务名（如 `payment:notify`）定义在 `internal/payment`，不进 pkg。

## 11. 装配与配置

```
createInfra:  telemetry → pgsql → queue client
createServer: channelclient(gRPC 直连 channel，装配期建连失败即死)
            → service（启动全量同步 channel_instances，失败即死）
            → [notify worker(pkg/queue Worker) + sync job + notify-sweep job + order-sweep job]
            → transport.Server(
                WithService(paymentv1 + gatewayv1),
                WithGateway(paymentv1),
                WithMount("/callbacks/", callbackHandler),
              )
```

`configs/payment.yaml` 段：`log / app / telemetry / transport(grpc_addr :9093, http_addr :8093) /
pgsql / queue(redis) / channel(addr, sync_interval) / notify(callback_base_url 即回调外网基址)`。
零硬编码：全部地址、周期、基址来自 yaml（宪法第 4 条）；sweep 阈值（30m/10m/2h）为实现内常量，
属行为参数而非环境差异项，有环境化需求时再提配置。生产边缘只放行 `/v1/*` 与 `/callbacks/*`，
`/docs`、`/healthz` 不对公网放行（网络策略属部署侧，不属服务配置）。

## 12. 分层落点（`internal/payment/`）

| 层 | 内容 |
|---|---|
| handler | `grpc.go`（paymentv1，薄壳）、`gateway.go`（gatewayv1 两个 RPC，薄壳）、`callback.go`（mount 的原生 HTTP handler，薄壳：解析原文 → service） |
| service | 验签与商户鉴权、选路派单、状态机（唯一写订单终态的入口，回调/回推/查单共用）、通知任务生产、实例同步 |
| repo | merchants / channel_instances / merchant_channels / payment_orders / callbacks / order_notifications 的 GORM 实现，接口由 service 声明 |
| model | 六张表模型，不出服务边界 |
| job | `sync`（实例定时同步）、`notify_sweep`（通知兜底扫描）、`order_sweep`（created 滞留兜底）；notify worker 挂 `pkg/queue` |
| channel_client | channel gRPC 客户端（形态同 channel 的 `gateway_client`） |

错误流向遵守 AGENTS.md：repo 翻译 GORM 错误为 service 哨兵 → service 翻译为 50000 段 errcode（Wrap 挂 cause）→ transport 出口打包；callback HTTP 面是原生 handler，由 handler 层把 errcode 翻译为 §7 的应答约定。

## 13. 安全不变量（评审与实现的检查单）

| 不变量 | 理由 |
|---|---|
| 签名一律 HMAC-SHA256，密钥不进拼串本体 | 杜绝弱哈希与长度扩展类弱点 |
| 全字段（含空值）参与签名 | 任何字段被剥离或篡改即验签失败 |
| IP 白名单逐项精确匹配 | 子串/前缀匹配会误放行同前缀地址 |
| 先落单再派单 | 三方侧有单则我方必有记录，可对账 |
| success 终态不可回退 | 商户收到的成功通知永不被反转 |
| 回调金额严格相等才入终态 | 金额异常不自动收敛，留人工 |
| 一切状态变更来源落 `callbacks` 表 | 回调与补单回推同等留痕，可审计可回溯 |
| 补单回推返回值语义明确 | 不可收敛留人工、基础设施错误才重试，无静默失败 |
| 错误双通道 | 原始错误只进日志，对外只出业务码与安全消息（宪法第 2 条） |

## 14. 测试策略

- **单测（service 层，repo/channel mock）**：签名规范（正例 + 空值字段 + 时间戳过期 + 篡改）、
  状态机全转移表（含矛盾回调、金额不符、重复回调幂等）、选路（指定/未指定/限额过滤/全败）。
  签名与状态机是资金安全边界，纪律同 channel 的 signer_test：**必配，先写用例后实现**（TDD）。
- **repo 不 mock GORM**：repo 层薄，错误翻译逻辑随 e2e/集成验证。
- **验证闭环**：`make check` 全绿（宪法第 5 条）→ `make run SVC=payment` → `/docs` 可读 →
  HTTP 冒烟（下单 → 模拟回调 → 查单终态 → 通知留痕）。
