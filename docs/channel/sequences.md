# channel 请求时序

> 三张时序图（archify 生成，浏览器打开可交互、可导出 PNG/SVG）：
> [代收下单](diagrams/sequence-payment-order.html) ·
> [回调验签](diagrams/sequence-callback.html) ·
> [补单对账](diagrams/sequence-reconcile.html)。
> 图源为同目录同名 `.json`，修改方式见 [../toolbox.md](../toolbox.md)。

## 1. 代收下单（PaymentOrder）

代付（PayoutOrder）同构，仅入参多收款方银行四要素、无支付链接出参。

1. gateway 携带路由三元组发起 gRPC 调用，先穿拦截链（出口翻译 / 访问日志 / otel）。
2. handler 薄壳互转后调 service；service `lookup` 命中内存路由表（TTL 60s 到期先惰性重载）。
3. adapter 按渠道约定拼参数、签名，经 `pkg/httpc` 打下游（缺省超时 15s，出站 span 续在同一条链上）。
4. 校验 HTTP 200 → 解析 JSON → 校验渠道业务码（payapay `code=200` / neokred `statusCode=200`）→
   映射为中立出参，响应原文留在 `response` 字段。
5. 回程原路返回；任何一步失败按哨兵翻译为业务 errcode，经拦截链打包进 status details。

失败分支：路由未命中 40001；三元组缺字段 10001；网络错/非 200/业务码失败/缺字段 40002；
响应无法解析 40005。原始错误挂 cause 链只进日志。

## 2. 回调验签（PaymentCallback / PayoutCallback）

三方回调**不直接打 channel**：先到 gateway 的 HTTP 入口，gateway 按 channelId 反查三元组，
把 header 与 body **原样转发**给 channel——报文不做任何加工，验签上下文完整保留。

1. adapter 将 body 按 `UseNumber` 解成字符串 map（数值保持字面形式不丢精度）。
2. 重算签名与报文 `sign` 比对（payapay：跳过 sign 与空值、按 key 排序拼串接 `&key=secret` 取 MD5）。
   不符 → 40003。
3. 状态映射：渠道私有状态 → 对外回调类型，金额字段口径见下方总表；状态不在枚举 → 40004。
4. 返回 `CallbackOut{order_no, out_order_no, callback_type, amount}`，gateway 据此推进订单，
   并按渠道 `callback_return` 约定应答三方（如 `success`）。

无回调渠道（neokred 形态）实现直接返回 `ErrCallbackUnsupported` → 10001，状态闭环交给补单对账。

## 3. 补单对账（Reconcile Job）

条件装配：`configs/channel.yaml` 的 `gateway.addr` 非空才装配 job；按商户行 `reconcile_enabled`
逐路由启用（渠道无回调或回调不可靠时开）。

1. 每 15s 一轮（首轮立即）：取 `ReconcileRoutes()` 开启补单的路由清单。
2. 对每个路由调 gateway-rpc `TripartiteUnfinishedOrders`，拉取 30 分钟窗口内未完结的代收/代付单。
3. 逐单走 service 查询（同一条查询链路，含路由解析与哨兵翻译）拿渠道真实状态与金额。
4. **金额一致且状态非处理中才回推**：`TripartiteOrderCallback` 携带终态、金额与 reference_no；
   处理中或金额不符一律跳过（金额不符留人工核查，防止渠道侧错乱放大为资金事故）。
5. 单轮任何失败只记 Warn 日志，下一轮重试——对账是兜底通道，不放大为服务故障。

## 状态映射总表

对外口径（proto 契约）：查询 `Status`：`1 处理中 2 成功 3 失败`；回调 `CallbackType`：`1 成功 2 失败`。

| 渠道 | 来源 | → 对外状态 |
|---|---|---|
| payapay 查询 | 私有 `status` 2 | 成功 |
| payapay 查询 | 私有 `status` 3 | 失败 |
| payapay 查询 | 其余 | 处理中 |
| payapay 代收回调 | `status` 2（取 real_price） | 成功回调 |
| payapay 代收回调 | `status` 3（取 order_price） | 失败回调 |
| payapay 代收回调 | 其他 | 40004 |
| payapay 代付回调 | `status` 2 | 成功回调 |
| payapay 代付回调 | `status` 3 / 7 / 9（取 order_price） | 失败回调 |
| neokred 代收查询 | CREDITED / DEBITED / CREDIT / SUCCESS | 成功 |
| neokred 代收查询 | 失败类状态 | **处理中**（刻意语义：渠道存在先失败又成功，勿「修正」） |
| neokred 代付查询 | INITIATED / RECEIVED / INPROGRESS | 处理中 |
| neokred 代付查询 | TRANSFER_SUCCESS / CREDIT_CONFIRMATION / TRANSFER_ACKNOWLEDGED / PREFUND / SUCCESS | 成功 |
| neokred 代付查询 | 未知状态 | 失败 |
