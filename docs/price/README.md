# price 服务文档

> 从 Binance 与 OKX 采集行情：ws 收实时 K 线、ticker、浅层盘口，REST 补历史，
> 只落库、不出查询口。是**采集侧**，不碰交易——与 channel（三方支付通道对接）
> 业务域完全独立，唯一共性是「交易所/渠道方言翻译不出各自的适配层」这个分层
> 套路。设计依据见
> [design.md](../superpowers/specs/2026-08-28-price-service-design.md)；
> 随代码演进的权威文档仍是各包 Go doc 注释，本文与其冲突时以代码为准。

## 业务定位

| | price | channel（参照） |
|---|---|---|
| 角色 | 采集侧：只读交易所公共行情流，写本地 | 通道侧：代表商户与三方支付渠道下单/查询/收回调 |
| 出口 | 无对外 gRPC/HTTP（`none` 变体），消费方直接读库 / 读 Redis | gRPC `ChannelService`，8 个 RPC |
| 私有数据 | 不需要密钥，只读公共流，不碰交易/账户 | 需要渠道商户密钥，下单即产生资金动作 |
| 错误处理 | 无客户端，业务错误只进日志，不占错误码分段（见 AGENTS.md 错误码分段表） | errcode 双通道翻译，占 40000–49999 段 |

一个直接后果：**price 的表结构与 Redis key 是事实上的对外契约。** 没有 gRPC 出口
挡在前面，AGENTS.md「model 不出服务边界」这条约定在这里不成立——改表 = 破坏性
变更，得按契约对待。

## 数据落点

三张 PostgreSQL 表（`migrations/price/20260828093516_create_price_tables.sql`）
+ 一个 Redis key 空间：

| 落点 | 唯一键 / key 形状 | 装的是什么 |
|---|---|---|
| `price_instruments` | `(exchange, market, native_symbol)` | 交易所全量交易对的本地镜像，是「能订什么」的权威范围；下架的标 `status=2`（delisted）而不删行——历史 K 线仍按这三列引用它 |
| `price_subscriptions` | `(exchange, market, native_symbol, stream_type, interval)` | 「实际订什么」的声明式清单，一行 = 一条流；`stream_type` 取 `kline`/`ticker`/`depth`，ticker/depth 行 `interval` 为空串；`enabled=false` 的行重载器不会拿去建连 |
| `price_klines` | `(exchange, market, native_symbol, interval, open_time)` | 唯一入库的时序数据，ws 收线与 REST 补洞共用同一张表，`source` 列区分来源（1=实时流，2=补洞回填）；一律 upsert，冲突以后写覆盖 |
| Redis `price:{exchange}:{market}:{symbol}:{stream}` | 无 TTL | ticker/depth 的最新一帧，payload 是 `{event_time, recv_time, payload}`——交易所事件时间 + 本地接收时间 + 归一化报文主体 |

**为什么 Redis key 不设 TTL**：设了 TTL，断流后 key 消失，消费方看到的是「没有
这个标的」（误导——该报采集故障却报成配置错误）；不设 TTL，消费方看到的是
「有，但数据是 N 分钟前的」，这才是事实。陈旧判定的阈值属于消费方的策略，不该
由采集方用 TTL 替它决定。实现见 `internal/price/repo/latest.go`（`Latest.Set`）
与 `internal/price/service/route.go`（`latestPayload`）。

## 两个子命令

无参数即常驻采集（`make run SVC=price`、容器 CMD 均不带参数，必须是常驻，不能
是打印用法）；另有两个跑完即退出的子命令，三者均需 `-config`（默认
`./configs/price.yaml`）：

| 子命令 | 必填参数 | 语义 | 幂等性 |
|---|---|---|---|
| （无参） | — | 常驻：建连接、收流、批量落库/写 Redis；收到 SIGINT/SIGTERM 优雅停机（停止重连 → 关连接 → 排空在途 kline 落盘） | — |
| `instruments` | `-exchange` | 拉 `-exchange` 指定交易所的全量现货交易对，upsert 进 `price_instruments`；本轮未返回的标的标记为已下架（不删行） | 可重复跑，结果收敛 |
| `backfill` | `-exchange -market -symbol -interval -from -to` | 对单条订阅在 `[-from, -to]` 闭区间内分页拉历史 K 线并 upsert；`-market` 默认 `spot` | 可重复跑，可与常驻并行（见下方「跨进程限速」边界） |

`backfill` 不依赖常驻进程——典型用途正是常驻进程不在的时候：新接一个标的、事故
后补数据。

## 背压与补洞机制

| 机制 | 触发条件 | 行为 |
|---|---|---|
| kline 队列背压 | `klineCh`（容量 `collector.kline_queue_size`，零值 1024）满 | 阻塞 `route()` 的发送，直到 writer 腾出位置——收线帧丢一帧就是一个洞，只能靠 REST 补回，宁可让 ws 读循环变慢 |
| ticker/depth 队列背压 | `snapCh`（容量 `collector.snapshot_queue_size`，零值 256）满 | 丢弃队列里最旧一帧，换新帧进去——它们是快照，只有最新一帧有意义 |
| 自动补洞触发 | 连接进入可用状态（首连/断线重连/订阅集重建），经 `stream.OnReady` | 对该连接名下全部 kline 订阅调用 `Price.Backfill`：起点取 `KlineRepo.MaxOpenTime`，无历史则回溯 `collector.max_backfill_window`（零值 720h）；止点为调用时刻 |
| 手工补洞 | 运维执行 `backfill` 子命令 | 起止点由 `-from`/`-to` 显式给定，不查库，可重复对同一区间补 |
| 补洞并发上限 | 单次 `Backfill` 调用内 | `collector.backfill_concurrency`（零值 2）控制**这一次调用**同时处理的订阅数——不是跨同一交易所全部连接的全局并发上限，真正兜底配额的是共享限速桶（见下方边界） |

## 故障矩阵

对照 design.md §7，逐项与实现比对：

| 故障 | 行为 | 数据后果 |
|---|---|---|
| ws 断线 | 指数退避 + 抖动重连，上限封顶（`reconnect_backoff_min/max`）；重连后重订阅并触发补洞 | K 线无洞（补回）；ticker/depth 断流期间为陈旧值 |
| 单条订阅被拒（标的已下架等） | **未实现**：两个 adapter 的 `Decode` 按契约把订阅拒绝一类控制帧（含 error 帧）当零值事件忽略（见 `exchange.Exchange.Decode` 接口注释），`stream.Conn.readLoop` 只在 `Decode` 返回 error 时才记日志——一条被拒的订阅是零日志、零告警、永久静默，不会被剔出连接计划 | 该订阅永久不产出数据，且不会被察觉；Binance 把同一交易所全部订阅打进一条合并流 URL，一条坏订阅按协议形态会影响**整条连接**（握手被拒或后续持续报错），不是本行曾经宣称的「不拖垮」 |
| REST 限流 | 按限速桶等待取用令牌 | 补洞变慢，不丢 |
| PG 不可达 | kline 写入原地重试，`klineCh` 随之阻塞——背压沿 `route()` 一路传导回 ws 读循环 | 不丢（代价是最终阻塞读取，是刻意设计） |
| Redis 不可达 | ticker/depth 直接丢弃并记 Warn 日志（日志即计数口径） | 只影响最新值，K 线链路不受牵连 |
| 交易所定时强制断连 | 与普通断线同一路径，无特殊分支 | 同「ws 断线」一行 |

## 边界与已知限制

以下是实测确认或代码逐行核对过的边界，不是可以事后「顺手补上」的缺口——运维与
后续改动前须知道。

**必须知道的三条（直接影响运维操作）：**

| 边界 | 后果 | 应对 |
|---|---|---|
| daemon 与 `backfill` 子命令跨进程不共享限速桶（`internal/price/ratelimit/bucket.go`） | 两个进程各自按同一份配置各构造一个 `*ratelimit.Bucket`，同时跑会让该交易所的 REST 请求速率翻倍 | 接受短暂翻倍（多数交易所限速有余量），或先把该交易所在 `configs/price.yaml` 里改 `enabled: false` 关停 daemon 侧消费，再跑 backfill |
| `config.Exchange.Enabled` 是 Go 零值 `false`（`internal/price/config/config.go`） | 配置块里漏写这一行，该交易所被装配层静默跳过——只有一条 Info 日志，进程照常起来、一根 K 线都不采 | 新增/修改交易所配置块后确认 `enabled: true` 显式在场 |
| 「writer 必须先于全部 `stream.Manager` 注册」的停机顺序不变量，没有任何代码强制（`cmd/price/initial/init_app.go` `createServer`） | 顺序改错不会编译失败、不会有运行期报错，只会在真实停机时因 `klineCh` 阻塞而挂死 | 唯一的确定性防线是白盒单测 `TestCreateServer_WriterRegisteredBeforeAllStreamManagers`（`cmd/price/initial/init_app_test.go`）；**`make e2e SVC=price` 不是这条不变量的回归防护**——e2e 的 `klineCh` 容量（1024）远大于在途数据量，颠倒注册顺序 e2e 依然全过（已实测确认） |

**其他已知边界（未解决，非缺陷，如实记录）：**

- `ImportInstruments`（`internal/price/service/instruments.go`）不走限速桶——每次只发一个请求，影响很小，但事实如此，不要假设它受限速保护。
- `collector.backfill_concurrency` 是**每次** `Backfill` 调用各自的并发上限，不是跨同一交易所全部连接的全局上限；真正兜底配额的是共享的 `*ratelimit.Bucket`。
- OKX 的 `validBars` 白名单（`internal/price/exchange/okx/ws.go`）是硬编码的 27 个周期拼写；OKX 新增周期档位时需要同步更新这份白名单，否则合法新周期会被 `channelFor` 直接拒绝（返回 error，不是静默放行）。
- 两家交易所的协议细节（连接上限、心跳周期、REST 分页上限、排序方向等）已逐条核实并记在各自 `doc.go`（`internal/price/exchange/binance/doc.go`、`internal/price/exchange/okx/doc.go`），附核实日期与出处——这些是交易所自己会改、且改了会**静默出错**（不报错，只是悄悄少一段数据）的东西，复用这些数字前先核对文档日期是否还是当期。

## 本地运行动线

前置：本地 PostgreSQL（`configs/price.yaml` 的 `pgsql.write`）、本地 Redis
（`redis.addrs`）。

**建表之后、起常驻进程之前必须往 `price_subscriptions` 插至少一行**：重载
job（`internal/price/job/reload.go`）只把 `enabled=true` 的行变成 ws 连接
（见 `service.Plans()`），一行都没有的话 daemon 会正常起来、0 条连接、一根
K 线都不采，且没有任何报错——只照下面第一条命令跑完，会误以为常驻进程本身
有问题。SQL 写法可参考 `test/e2e/price/run.sh` 里对 `price_subscriptions` 的
插入。

```bash
make migrate-up SVC=price        # 建三张表
psql "postgres://app:app@127.0.0.1:5432/app?sslmode=disable" -c \
  "INSERT INTO price_subscriptions (exchange, market, native_symbol, stream_type, interval, enabled) VALUES ('binance', 'spot', 'BTCUSDT', 'kline', '1m', TRUE);"
make run SVC=price               # 常驻采集（等价 go run ./cmd/price）
./bin/price instruments -config configs/price.yaml -exchange=binance   # 或 -exchange=okx
./bin/price backfill -config configs/price.yaml -exchange=binance \
  -market=spot -symbol=BTCUSDT -interval=1m \
  -from=2026-01-01T00:00:00Z -to=2026-01-02T00:00:00Z
make e2e SVC=price                # 端到端：mock ws/REST，覆盖 instruments/常驻/停机排空/backfill
```

`make e2e SVC=price` 前置额外需要 `psql`、`curl`、`lsof`；不连真实交易所，
ws/REST 均指向 `test/e2e/price/mockd`。

## 落点

服务六件套（含新增的 `exchange`/`stream`/`ratelimit` 三层，登记于 AGENTS.md
「服务分层」）+ `docs/price/README.md` 本文 + `test/e2e/price/`，位置遵循仓库
「关注点为根、服务为子目录」约定。price 是本仓第一个 `none` 变体的子命令形态
服务：无参数必须是常驻（不能打印用法），见 `cmd/price/main.go` 包注释。
