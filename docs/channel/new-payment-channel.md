# 接入新支付渠道指南

> 目标读者：要把一个新三方支付渠道接入 channel 服务的工程师（或 AI 助手）。
> 动手前先读 [README.md](README.md) 的领域概念与 [architecture.md](architecture.md) 的适配层设计。

## 第 0 步：摸清渠道形态（决定后面所有分支）

从渠道方文档确认六件事，对号入座：

| 问题 | 影响 |
|---|---|
| 下单/查询/余额的鉴权方式？ | 签名（payapay 形态）还是 token/secret 头（neokred 形态）；token 是否要登录换发、401 刷新 |
| 金额单位？ | 我们统一**分**；渠道要元则 adapter 出入互转（参考 neokred `amountYuan`） |
| 有没有回调？ | 无回调 → 实现返回 `ErrCallbackUnsupported`，商户行置 `reconcile_enabled = true` 走轮询 |
| 回调验签算法与报文来源？ | body JSON 还是 URL query；签名重算规则；成功/失败取哪个金额字段 |
| 渠道私有状态枚举？ | 整理「渠道状态 → 对外状态（处理中/成功/失败）」映射表，落进 [sequences.md](sequences.md) |
| 查询用我方单号还是渠道单号？ | `QueryIn` 两者都有，渠道只认一个时在 adapter 里校验（参考 neokred 须带 out_order_no） |

## 第 1 步：登记渠道商户行

`channels` 表 INSERT 一行（或新商户较少时让运维直接入库；表结构变更才需要 `make migrate-create SVC=channel NAME=xxx` 新增迁移文件）：

- 通用列：路由三元组（`channel_name` 用小写规范形式）、限额（分）、费率（千分位）、回调约定、`payout_supports`。
- `platform` JSONB：该渠道私有配置的原文，结构与第 2 步的 `Platform` 结构体一致（base_url、API 路径、密钥）。
- `reconcile_enabled`：无回调渠道置 `true`。
- **密钥不入仓库**：种子迁移只放本地演示值，生产由运维入库。

行先进库没关系：未登记渠道名或 `platform` 配置非法的行只跳过并告警，不影响其他商户
（机制见 [architecture.md](architecture.md) 路由表机制）；排查看启动日志。

## 第 2 步：实现 adapter 包

新建 `internal/channel/adapter/<渠道名>/`（目录名 = `channel_name` = `Name()` 返回值，小写），
实现 `adapter.Adapter` 八个方法。文件按关注点固定划分，问题直达文件（与 payapay、neokred
一致，第 N 个渠道保持同构）：`config.go`（Platform 结构）/ `<pkg>.go`（包注释、Client、New、
共用底座）/ `order.go`（代收代付下单）/ `query.go`（查单与余额）/ `callback.go`（回调验签与
状态映射；无回调渠道两个 stub 也放这，让「无回调」可见）；签名、登录态、IP 池等渠道特有机制
各占一个领域命名文件（如 `signer.go`、`token.go`），不设 `help.go` 类杂物文件。骨架：

```go
// ---- config.go ----

// Platform Xxx 渠道私有配置，与 channels.platform 的 JSON 结构对应。
type Platform struct { /* ... */ }

// ---- xxx.go（主文件）----

// Package xxx 是 Xxx 渠道适配器：<一句话形态描述>。
package xxx

// Client Xxx 渠道客户端：实现 adapter.Adapter。
type Client struct {
	conf Platform
	http *httpc.Client
}

// New 反序列化 platform 配置并构造适配器；配置不合法当场报错（装配期暴露）。
// hc 由 service 的 builders 登记表闭包注入（内建链路追踪，见 pkg/httpc）。
func New(hc *httpc.Client, platform json.RawMessage) (adapter.Adapter, error) { /* ... */ }

func (a *Client) Name() string { return "xxx" }

// ---- order.go / query.go / callback.go ----
// PaymentOrder / PayoutOrder | PaymentQuery / PayoutQuery / BalanceQuery |
// PaymentCallback / PayoutCallback 按上述划分各归其文件。
```

四件套纪律（签名、参数拼装、响应解析、状态映射不出本层）落到代码模式：

- **请求**：`a.http.PostJSON(ctx, url, header, params, 0)` 统一出口（timeout 传 0 用 15s 缺省；表单渠道用 `PostForm`，multipart 等其余形态用 `Post` 发原文体、Content-Type 经 header 给）；网络错误与非 200
  包装 `fmt.Errorf("%w: …", adapter.ErrChannelRejected)`。
- **解析**：先 `json.Unmarshal` 到渠道结构（失败 → `ErrBadResponse`），再校验渠道业务码（失败 →
  `ErrChannelRejected`，报文拼进错误信息）。
- **回调**：验签不符 → `ErrVerifyFailed`；状态不在枚举 → `ErrUnknownCallbackStatus`；金额解析失败 →
  `ErrBadResponse`。无回调渠道两个 Callback 方法直接 `return adapter.ErrCallbackUnsupported`。
- **token 型鉴权**：参考 `neokred/token.go` 的 `tokenHolder`（缓存 + `singleflight` 防并发重复登入 + 401 单飞刷新重试一次）。

实现语义以渠道方文档为准、如实映射；既有渠道中 neokred「失败态映射为处理中」这类反直觉行为是
**刻意语义**（见 [sequences.md](sequences.md) 状态映射总表），不要顺手「修正」。

## 第 3 步：登记构造表

`internal/channel/service/channel.go` 的 `builders()` 加一行：

```go
func builders() map[string]adapter.NewFunc {
	return map[string]adapter.NewFunc{
		"payapay": payapay.New,
		"neokred": neokred.New,
		"xxx":     xxx.New, // 新渠道在此登记
	}
}
```

## 第 4 步：签名单测（必配）

签名与回调验签是资金安全边界，`adapter/<渠道>/` 下必须有自己的 `_test.go`：
用渠道文档给的对拍样例断言 `createSign`（或等价函数）输出；有回调验签则加「构造合法签名通过、
篡改任一字段失败」两个用例。参考 `adapter/payapay/payapay_test.go`。

## 第 5 步：对账开关（仅无回调渠道）

- 商户行 `reconcile_enabled = true`。
- channel 侧配置 `configs/channel.yaml` 的 `gateway.addr` 指向 gateway-rpc（留空则 job 不装配，
  多渠道共用一个 job，配置一次即可）。

## 第 6 步：验证

1. `make check` 全绿。
2. `make e2e SVC=channel` 走全链路（本地 Postgres 前置；测试商户打 mockd，不触网）。
3. `make run SVC=channel` 起服务，grpcurl 按三元组冒烟：

```sh
grpcurl -plaintext -d '{"route":{"channelName":"xxx","merchantNo":"M1","currency":"INR"}}' \
  127.0.0.1:9092 channel.v1.ChannelService/ListChannels
```

4. 沙箱渠道下单/查询/回调各打一轮，确认状态映射与金额字段正确。

## 检查单（提交前过一遍）

- [ ] `channel_name` 全链路一致：目录名 / `Name()` / `builders()` 键 / DB 行。
- [ ] 所有下游错误都包装了五个哨兵之一，没有裸 `errors.New` 逃逸到 service。
- [ ] 金额：入出参一律分；对渠道的元/分互转只在 adapter 内。
- [ ] 状态映射表已补进 [sequences.md](sequences.md) 与包注释。
- [ ] 签名单测存在且覆盖验签正反例。
- [ ] 无回调渠道：`ErrCallbackUnsupported` + `reconcile_enabled` + `gateway.addr` 三件套齐备。
- [ ] `make check` 全绿，e2e 通过。

## 常见坑

- **越层**：handler/service 里出现渠道报文细节、adapter 里出现业务 errcode——都是违规，
  错误翻译只在 service `translate`。
- **proto 字段不同步**：代收/代付的查询与回调入出参同构，buf lint 要求每 RPC 独立，
  契约字段演进须同步另一侧。
- **金额精度**：回调报文数值解析用 `UseNumber` 保字面形式（参考 payapay `stringifyCallback`），
  避免 float 精度损失影响验签与对账。
