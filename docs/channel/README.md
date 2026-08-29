# channel 服务文档

> 统一多渠道商户的三方支付对接服务：单一 gRPC 服务以路由三元组定位渠道商户实例，
> 签名、报文、状态映射等渠道差异收敛在 adapter 层。本目录是 channel 的结构化文档入口；
> 随代码演进的权威文档仍是各包 Go doc 注释，本文与其冲突时以代码为准。

## 文档地图

| 文档 | 内容 |
|---|---|
| [architecture.md](architecture.md) | 技术架构：分层、装配、路由表、适配层、数据模型、拦截链 |
| [sequences.md](sequences.md) | 请求时序：下单、回调验签、补单对账，含状态映射表 |
| [new-payment-channel.md](new-payment-channel.md) | 接入新支付渠道指南（六步动线 + 检查单 + 常见坑） |
| [gateway-migration.md](gateway-migration.md) | gateway-channel → channel 迁移说明（gateway 侧对接改造 + 渠道分支迁移模式） |
| [diagrams/](diagrams/) | 架构图与时序图（archify 生成，含可改的 JSON 图源；改图见 [../toolbox.md](../toolbox.md)） |

改契约、错误码、状态映射时同步更新本页与 [sequences.md](sequences.md) 对应表格。

## 业务定位

channel 服务对上游 gateway-backend 提供两类能力：

- **通道能力**：代收（Payment，用户付款进来）与代付（Payout，钱出款到用户账户）的下单、
  查询、回调验签、余额查询。channel 自己不落订单、不做账务——订单与账务在 gateway 侧，
  channel 只做「报文翻译 + 签名验签 + 状态映射」的纯通道。
- **元数据能力**：`ListChannels` 全量输出渠道商户的限额、费率、回调约定等元数据，
  gateway 启动时与定期拉取做路由。

## 领域概念

| 概念 | 含义 |
|---|---|
| 渠道商户实例 | 我们在某个三方渠道开的一个商户号，`channels` 表一行；限额、费率、回调约定挂在实例上 |
| 路由三元组 | `channel_name + merchant_no + currency`，与表唯一约束一一对应，每个请求携带以定位实例 |
| 通用元数据（General） | 实例的渠道无关信息：限额、费率、回调头约定、代付方式，对外暴露给网关做路由 |
| platform 私有配置 | 实例的渠道私有信息（base_url、API 路径、密钥），JSONB 原文存储，由 adapter 各自反序列化 |
| 代收 / 代付 | Payment（收款，UPI/银行卡入账方向）/ Payout（出款，银行转账或 UPI 出账方向） |
| 回调验签 | 三方渠道回调先打到 gateway HTTP 入口，原样转发 header + body 到 channel，adapter 内验签并映射状态 |
| 补单对账 | 渠道无回调或回调不可靠时，job 定期拉网关未完结单、查渠道真实状态、金额一致才回推 |

## RPC 一览（`channel.v1.ChannelService`，8 个）

| RPC | 用途 |
|---|---|
| `ListChannels` | 全量渠道元数据快照 |
| `PaymentOrder` | 代收下单，返回支付链接与渠道单号 |
| `PayoutOrder` | 代付下单（银行 1 / UPI 2） |
| `PaymentQuery` | 代收订单查询（order_no / out_order_no 至少其一） |
| `PayoutQuery` | 代付订单查询 |
| `PaymentCallback` | 代收回调验签（header + body 原样转发） |
| `PayoutCallback` | 代付回调验签 |
| `BalanceQuery` | 渠道商户余额查询 |

契约见 `api/channel/v1/channel.proto`；对外状态枚举与各渠道映射见
[sequences.md](sequences.md) 状态映射总表。

## 错误码（40000–49999 分段，登记于 AGENTS.md）

| 码 | 含义 | gRPC 状态 | 典型来源 |
|---|---|---|---|
| 40001 | 渠道实例不存在 | NotFound | 路由三元组未命中路由表 |
| 40002 | 下游渠道请求失败 | Unavailable | 网络错、非 200、渠道业务码失败、响应缺字段 |
| 40003 | 回调验签失败 | PermissionDenied | 重算签名与报文 sign 不符 |
| 40004 | 回调状态未知 | InvalidArgument | 报文状态不在渠道约定枚举内 |
| 40005 | 渠道响应解析失败 | Internal | 响应无法反序列化为预期结构 |

通用码 10001（参数错误，三元组缺字段、渠道不支持回调）与 10003（内部错误，未识别哨兵的兜底）同样适用。
错误流向：adapter 哨兵 → service `translate` 统一翻译 → transport 出口打包；细则见
AGENTS.md「错误处理约定」与 [architecture.md](architecture.md) 适配层。

## 关键不变量

1. 金额单位分、费率千分位，全链路统一；adapter 对渠道单位（如元）自行互转。
2. 签名、报文拼装、响应解析、状态映射不出 adapter 层。
3. 路由表内存态，TTL 60s 惰性重载；机制见 [architecture.md](architecture.md) 路由表机制。
4. ORM（GORM）不出 repo 层；handler 是薄壳，业务逻辑不进 handler。
5. 渠道响应原文保留在出参 `response` 字段，供网关侧排障留痕。
6. 补单对账「金额一致且非处理中」才回推——金额不符一律跳过留人工。

## 落点

服务六件套 + `docs/channel/` 本目录 + `test/e2e/channel/`，位置全部遵循仓库「关注点为根、服务为子目录」约定（见 AGENTS.md）；各层职责见 [architecture.md](architecture.md) 进程内分层表。
