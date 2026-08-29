# gateway-channel → channel 服务迁移说明

> 面向 gateway-backend 侧的对接改造指引，以及其余 ~300 条渠道分支的后续迁移模式。
> channel 服务位于本仓库 `cmd/channel` + `internal/channel`，契约在 `api/channel/v1/channel.proto`。

## 背景与形态变化

| | 原形态 | 新形态 |
|---|---|---|
| 部署 | 一渠道-商户一分支一镜像一 pod（etcd key `channel-*` 注册） | 单一 gRPC 服务，一个 pod |
| 商户配置 | 烤死在分支的 etc/channel.yaml | PostgreSQL `channels` 表一行一商户 |
| 渠道差异 | 整仓复制改 logic/signer | `internal/channel/adapter/<渠道>` 一个实现包 |
| 服务发现 | gateway etcd 前缀 watch + `Info()` | 直连 channel 服务 + `ListChannels()` |
| 补单对账 | 17 个分支自带 cron task | `reconcile_enabled` 行级开关，job 统一轮询 |

## gateway-backend 侧改造点

1. **发现与注册**：删除 etcd `channel-` 前缀 watch 逻辑（manager/core.go）。启动时与定期调
   `channel.v1.ChannelService/ListChannels` 拉全量元数据（原 `InfoReply` 字段全保留，按
   `channel_name + merchant_no + currency` 三元组做路由键），据比 `Upsert` 进 `channels` 表。
   channel 名统一为小写规范形式（如 `payapay`），存量数据需一次性刷成小写。
2. **调用路由**：原 `zrpc.NewClientWithTarget(kv.Value)` 直连各 pod 的调用，改为对 channel
   服务的单一 client；每个请求带上路由三元组（`route` 字段）。
3. **回调转发**：HTTP 入口 `/:category/:channelId` 收到三方回调后，转发目标从「按 channelId
   找 pod」改为「按 channelId 反查三元组」，带 `route` 调 `PaymentCallback/PayoutCallback`。
4. **对账回推**：gateway 侧 `TripartiteUnfinishedOrders/TripartiteOrderCallback` 契约不变
   （channel 侧镜像在 `api/gateway/v1/gateway.proto`）；channel 的补单 job 主动来拉，gateway
   无须改动。channel 侧 `configs/channel.yaml` 的 `gateway.addr` 指向 gateway-rpc 即启用。
5. **错误出口**：业务错误经 gRPC status details 携带错误码（ErrorInfo.reason，40000 段），
   原渠道实例宕机（pod 消失）场景对应新的 40001（渠道实例不存在，DB 行缺失）。

## 渠道迁移模式（剩余分支照此搬）

1. 建迁移行：`channels` 表 INSERT 一行（General 列 + platform JSON，值抄原分支 etc/channel.yaml）。
2. 建实现包：`internal/channel/adapter/<渠道名>/`，实现 `adapter.Adapter` 八个方法——
   原分支 `internal/logic/*_logic.go` 的参数拼装、签名、解析、状态映射逐段移植，
   错误用 `fmt.Errorf("%w: …", adapter.ErrChannelRejected)` 等哨兵包装。
3. 登记：`internal/channel/service/channel_service.go` 的 `builders()` 加一行。
4. 签名单测必配（移植原分支已有的 signer_test，无则补）：签名与回调验签是资金安全边界。
5. 若渠道无回调（neokred 形态），置 `reconcile_enabled = true` 走查询轮询兜底。
6. 验证：`make check` 全绿 → `make run SVC=channel` → grpcurl 按三元组冒烟查询/下单沙箱。

## 注意事项

- 金额单位分、费率千分位的约定原样保留；neokred 查询「失败态映射为处理中」的语义是
  刻意行为（渠道存在先失败又成功），移植其他渠道时不要「顺手修正」。
- `platform` JSONB 含渠道密钥，生产由运维直接入库，不走仓库迁移文件；种子迁移仅本地演示。
- 未迁移的渠道行在路由表构建时跳过并告警，不影响其他商户——增量迁移期 DB 可先行。
