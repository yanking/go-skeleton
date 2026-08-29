# payment 服务文档

> 代收支付平台的商户面服务：受理商户下单、按绑定优先级选路到渠道实例、经 channel 服务
> 下发三方渠道，并以订单状态机收敛回调/补单/查询三路结果，终态异步通知商户。
> 本页是 payment 的结构化文档入口；随代码演进的权威文档仍是各包 Go doc 注释，
> 本文与其冲突时以代码为准。

改契约、错误码、状态机时同步更新本页对应表格（宪法第 6 条）。

## 业务定位

payment 是**有账的一侧**——订单、商户、绑定关系、通知记录都落在本服务的库里；
channel 是无账的纯通道（报文翻译 + 验签 + 状态映射）。两者职责不重叠：

| | payment | channel |
|---|---|---|
| 订单落库 | 有（`payment_orders` 等六表） | 无 |
| 商户鉴权 | 有（固定字段签名） | 无 |
| 选路 | 有（商户绑定 + 优先级 + 限额） | 无（调用方给三元组） |
| 渠道报文/签名 | 无（不碰渠道差异） | 有（adapter 层） |

## 领域概念

| 概念 | 含义 |
|---|---|
| 商户 | 接入方，`merchants` 一行；持 app_id/app_secret，带 IP 白名单、限额、费率 |
| 渠道实例 | channel 侧的渠道商户号，本地 `channel_instances` 是其**只读副本**，由同步任务整体覆盖 |
| 商户绑定 | `merchant_channels`：商户可用哪些渠道实例、优先级多少，选路的唯一依据 |
| 平台订单号 | `order_no`，本服务生成，全局唯一 |
| 商户订单号 | `mch_order_no`，商户侧唯一——唯一性按 (商户, 商户订单号) 判定，跨商户同号不冲突 |
| 渠道侧订单号 | `out_order_no`，渠道回执号，与实例一起构成回调反查键 |
| 选路 | 按币种取商户绑定的候选实例，过限额、按优先级排序，逐个尝试下单直到成交 |

## 出口一览

| 出口 | 协议 | 用途 |
|---|---|---|
| `payment.v1.PaymentService/CreatePaymentOrder` | gRPC + HTTP `POST /v1/payment/orders` | 下单，返回平台订单号与支付链接 |
| `payment.v1.PaymentService/QueryPaymentOrder` | gRPC + HTTP `POST /v1/payment/orders/query` | 查单（平台单号或商户单号，二选一） |
| `payment.v1.PaymentService/ListAvailableChannels` | gRPC + HTTP `POST /v1/payment/channels/query` | 该商户当前可用渠道与限额 |
| `gateway.v1.GatewayService/TripartiteUnfinishedOrders` | gRPC | 供 channel 补单：拉本服务未完结订单 |
| `gateway.v1.GatewayService/TripartiteOrderCallback` | gRPC | 供 channel 补单：回推核对后的终态 |
| 渠道回调 | HTTP `POST\|GET /callbacks/payment/{instance_id}` | 三方渠道回调入口，报文原样交 channel 验签 |
| 接口文档 | HTTP `GET /docs` | 由 proto 注释生成的 OpenAPI 阅读页 |

商户面契约见 `api/payment/v1/payment.proto`；对账契约是上游 gateway-backend 的镜像
`api/gateway/v1/gateway.proto`（见 AGENTS.md「外部契约镜像」）。

## 签名规范

请求的**全部标量字段**（`sign` 自身除外，含空值与零值）按字段名 ASCII 升序拼为
`k=v&k=v…` 规范串，以商户 `app_secret` 做 HMAC-SHA256，取十六进制小写。

```
# 下单请求（省略未设置的可选字段仍要参与拼接，值为空）
app_id=demo&amount=10000&channel_name=&currency=INR&mch_order_no=M20260828001
&notify_url=https://m.example.com/cb&payer_email=&payer_name=&payer_phone=
&timestamp=1756339200000
        ↓ HMAC-SHA256(app_secret) → hex lower
sign=3f2a…（填回请求的 sign 字段）
```

三条硬约束（改签名相关代码前必读 `internal/payment/sign` 包注释）：

1. **空值字段必须参与**——允许剥离空值字段，攻击者删掉未设置的可选字段即可复用旧签名重放。
2. **值内的 `&` / `=` 不转义**——服务端按固定的 proto 字段键集重算，不反解拼接串，故无歧义。
3. **未支持的标量 Kind 一律拒签**——静默跳过会留下「加个字段就能绕过签名」的口子。

鉴权固定顺序：查商户 → IP 白名单 → 时间戳窗口（±5 分钟）→ 验签 → 商户状态。
查不到商户与验签失败对外统一返回 10004，不区分——否则可被用来探测 app_id 是否存在。

## 订单状态机

```
                        ┌──────────────► success (3) ◄──┐
                        │ 渠道成功且金额相等              │ 渠道成功（先失败后成功）
   created (1) ──下发──► sent (2)                       │
      │  渠道受理成功        │ 渠道失败                    │
      │                    └─────────► failed (4) ───────┘
      │ 下发失败 / 滞留 30 分钟兜底         ▲
      └───────────────────────────────────┘
```

`ApplyChannelResult` 是终态的**唯一写入口**（回调、补单回推共用），在行锁事务内决策：

| 当前状态 | 渠道成功事件 | 渠道失败事件 |
|---|---|---|
| created(1) | → success（派单未落库的残留单，放行） | → failed |
| sent(2) | → success | → failed |
| failed(4) | → success（先失败后成功，允许） | 幂等忽略 |
| success(3) | 幂等忽略 | **拒绝**：终态不可回退，标无效留人工 |

另有两条前置否决，先于事件分派：

- **实例一致性**：订单已归属某实例后只接受该实例的结果——防被攻陷的渠道用自身凭证
  验签通过后，以回调体里的 `order_no` 推进他人实例的订单（跨实例串单）。
- **金额严格相等**：成功事件金额与订单不符即拒绝，不设容差。

## 兜底机制

| 机制 | 触发 | 做什么 | 阈值 |
|---|---|---|---|
| 实例同步 | `instance-sync` job + 装配期一次 | 从 channel 拉全量实例覆盖本地副本 | 周期 `channel.sync_interval`，默认 5m |
| 滞留单收敛 | `order-sweep` job | 把派单中途进程退出留下的 created 残单置失败 | 创建满 30m 仍是 created |
| 通知重投 | `notify-sweep` job | 把入队丢失/长期未推进的通知重新入队 | 完成 10m 无记录，或最近尝试早于 24h |
| 补单对账 | channel 侧发起，打本服务 gRPC | channel 拉未完结单、核对渠道真实状态后回推 | 由 channel 的 job 周期决定 |
| 通知重试 | asynq | 商户返回非 `success` 即重试 | 最多 15 次，默认退避跑满约 50h(单次间隔最大约 14h) |

商户通知判成功的条件：HTTP 200 **且** 响应体忽略大小写与首尾空白后等于 `success`；
无论成败都落一条 `order_notifications` 留痕。

## 错误码（50000–59999 分段，登记于 AGENTS.md）

| 码 | 含义 | gRPC 状态 | 典型来源 |
|---|---|---|---|
| 50001 | 商户订单号重复 | AlreadyExists | 同商户 `mch_order_no` 撞唯一键 |
| 50002 | 金额超出限额 | InvalidArgument | 超商户限额，或候选实例按限额筛空——商户一条绑定都没有时候选即为空，同样归此码 |
| 50003 | 指定渠道未绑定或不可用 | FailedPrecondition | 请求指定了 `channel_name` 但该商户没绑定/已停用 |
| 50004 | 无可用渠道 | Unavailable | 候选逐个 failover 后全部下单失败 |
| 50005 | 订单状态冲突 | FailedPrecondition | 状态机拒绝的迁移 |
| 50006 | 商户状态受限 | PermissionDenied | 商户被封禁 |

通用码 10001（参数错误）、10002（订单不存在）、10003（内部错误）、10004（未认证，含验签失败）同样适用。
错误流向见 AGENTS.md「错误处理约定」。

## 关键不变量

1. 金额单位分、费率千分位，全链路统一。
2. 商户订单号唯一性按 **(商户, 商户订单号)** 判定，跨商户同号不冲突。
3. 终态只经 `ApplyChannelResult` 写入，且必过实例一致性与金额相等两道否决。
4. `channel_instances` 是只读副本，只由同步任务整体覆盖，业务路径不写。
5. ORM 不出 repo 层；pb 类型不出 handler 与 `channel_client` 层。
6. 入队失败只 Warn 不上抛——订单行已置待通知，由 `notify-sweep` 兜底重投。

## 本地运行动线

前置：本地 Postgres（`configs/payment.yaml` 的 `pgsql.write`）、Redis（`queue.addr`）、
已在跑的 channel 服务（`channel.addr`，默认 `127.0.0.1:9092`）。

```bash
make migrate-up SVC=channel && make migrate-up SVC=payment  # 建表 + 种子(演示商户 demo)
make run SVC=channel                                        # 另开一个终端
make run SVC=payment                                        # 装配期会同步一次渠道实例，同步不通即退出
```

演示商户没有渠道绑定（绑定需引用同步生成的真实实例 id），下单前手工绑一条：

```sql
INSERT INTO merchant_channels (merchant_id, channel_instance_id, priority, enabled)
SELECT m.id, i.id, 100, TRUE
FROM merchants m, channel_instances i
WHERE m.app_id = 'demo' AND i.channel_name = 'payapay' AND i.currency = 'INR';
```

冒烟：`http://127.0.0.1:8093/docs` 看接口文档，`POST /v1/payment/orders` 走通鉴权链
（签名不对应返回 10004；演示商户未绑定渠道时候选为空，返回 50002）。

## 落点

服务六件套 + `channel_client`（跨服务客户端）+ `sign`（领域工具包）+ `docs/payment/` 本目录，
位置遵循 AGENTS.md「服务分层」；装配在 `cmd/payment/initial`。
