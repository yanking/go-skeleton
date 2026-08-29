# price 行情采集服务实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 从 Binance 与 OKX 采集现货行情——ws 收已收线 K 线入库、ticker 与浅层盘口写 Redis 最新值,REST 补历史与导入交易对——只落库,不出查询口。

**Architecture:** 交易所包只产出「声明式连接计划」(endpoint + 订阅帧 + 解码器),连接的活法(拨号、心跳、退避重连、订阅集变更重建)完全通用,所以加一家交易所只写协议翻译。事件按数据语义分队列:kline 不可丢(队列满即阻塞上游),ticker/depth 可丢(弃旧)。补洞只有一个触发抽象——连接进入可用状态。

**Tech Stack:** Go 单 module;**两个新运行时依赖**——`github.com/coder/websocket`(ws 客户端)与 `golang.org/x/time`(限速),二者当前均不在模块图内(含间接),需分别 `go get`;既有 `pkg/{app,conf,log,pgsql,redis,httpc,telemetry}`;GORM;goose 迁移;测试用标准库 + `gorm.io/gorm/utils/tests` DryRun,不引测试框架。

**Spec:** `docs/superpowers/specs/2026-08-28-price-service-design.md`

## Global Constraints

- **变体 none**:price 无 transport、无 proto、无 handler 层。`configs/price.yaml` 不含 `transport` 段。
- **错误码不占分段**:price 无对外出口,业务错误只进日志。用 service 哨兵 error + `%w`,不引 `errcode.Code`。AGENTS.md 错误码表登记一行说明,不留静默空缺。
- **本期只做现货**:Binance `spot`、OKX `instType=SPOT`。市场字段在模型与键里保留,但配置只放现货,合约留待后续。
- **时间一律 UTC 毫秒,以开盘时间为准**(spec §3)。
- **K 线只写已收线的**:Binance `k.x == true`,OKX candle 数组索引 8 `confirm == "1"`。
- **K 线写入一律 upsert**,唯一键 `(exchange, market, symbol, interval, open_time)`,冲突时后写覆盖。
- **交易所方言不出交易所包**:symbol 形态、周期拼写、分页方向、限速数字、连接上限,全部在 `internal/price/exchange/<name>` 内翻译。
- **kline 不可丢,ticker/depth 可丢**——两者的背压策略必须不同。
- **REST 配额全进程共享**:常驻补洞与 `backfill` 子命令共用同一个令牌桶实例。
- **零硬编码**(宪法第 4 条):运行时参数只来自 `configs/price.yaml`。
- **提交门槛**(宪法第 5 条):每个 Task 末尾 `make check` 全绿才算完成。
- **已核实的协议事实**(可直接写进代码,注释里标注来源日期 2026-08-28):
  - Binance 合并流 `wss://stream.binance.com:9443/stream?streams=a/b/c`;K 线流名 `<symbol小写>@kline_<interval>`;报文 `{e,E,s,k:{t,T,s,i,o,c,h,l,v,n,x,q,...}}`,`k.t` 开盘时间(ms)、`k.x` 收线标志;浅层盘口流 `<symbol>@depth<5|10|20>@<100ms|1000ms>`,payload `{lastUpdateId,bids,asks}`。
  - Binance 服务端**每 20 秒**发 ping 帧,1 分钟内收不到 pong 即断开。(**订正**:本计划初稿写的「3 分钟 / 10 分钟」是 2025 年 2 月之前的旧值,来源混入了 binance.us 文档;Task 6 现场核实官方 binance-spot-api-docs 后订正。教训:凡标「已核实」的数字仍须注明来源仓库与核实日期。)
  - OKX 公共 ws `wss://ws.okx.com:8443/ws/v5/public`;K 线频道 `candle<bar>`(如 `candle1m`、`candle1H`);candle 为 9 元素数组 `[ts,o,h,l,c,vol,volCcy,volCcyQuote,confirm]`,`confirm=="1"` 为已收线。
  - OKX 要求**客户端**在 30 秒内无消息时主动发文本 `ping`,期待 `pong`;超时须重连。
- **仍待核实(Task 6/7/8 的第一步必须现场查文档并把结论写进包注释)**:Binance 单连接流数上限与每秒入站消息数上限(常见引用为 1024 与 5,但来源含 binance.us,须对 .com 文档确认)、单连接 24 小时强制断开是否适用于 .com、OKX 单连接订阅数上限、两家 REST 分页上限与权重、历史端点时间参数的开闭区间、OKX 历史端点的排序方向。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `cmd/price/main.go` | 子命令路由:无参→常驻;`instruments`;`backfill`。各自解析自己的 flag |
| `cmd/price/initial/init_app.go` | 常驻装配:createInfra(遥测/PG/Redis)+ createServer(采集器、reload job) |
| `cmd/price/initial/oneshot.go` | 两个一次性子命令的装配:只造需要的资源,跑完即退 |
| `internal/price/config/config.go` | 配置结构体,绑定 `configs/price.yaml` |
| `internal/price/model/{instrument,subscription,kline}.go` | 三张表的模型与状态常量 |
| `internal/price/repo/{instrument,subscription,kline,latest,errors}.go` | PG 三表 + Redis 最新值;ORM 不出本层 |
| `internal/price/exchange/exchange.go` | 中立事件类型、`Exchange` 接口、连接计划类型 |
| `internal/price/exchange/binance/{binance,ws,rest}.go` | Binance 协议翻译 |
| `internal/price/exchange/okx/{okx,ws,rest}.go` | OKX 协议翻译 |
| `internal/price/stream/{conn,manager}.go` | ws 连接生命周期:拨号、心跳、退避重连、订阅集变更重建 |
| `internal/price/ratelimit/bucket.go` | 按交易所命名的共享令牌桶 |
| `internal/price/service/{service,plan,route,backfill,instruments}.go` | 编排:连接计划、事件路由、补洞、交易对导入 |
| `internal/price/job/reload.go` | 订阅集定期重载组件 |
| `migrations/price/*.sql` | goose 迁移 |
| `configs/price.yaml` | 声明式配置 |
| `test/e2e/price/run.sh` | 端到端脚本(mock ws + mock REST) |
| `docs/price/README.md` | 服务文档 |

---

### Task 1: 服务骨架、配置结构与错误码登记

**Files:**
- Create: `cmd/price/main.go`、`cmd/price/initial/init_app.go`、`internal/price/config/config.go`、`configs/price.yaml`(由技能脚本生成后改写)
- Modify: `AGENTS.md`(错误码分段表)

**Interfaces:**
- Produces: `config.Config`,字段见 Step 3;`initial.App(ctx, config.Config, *slog.Logger) error`

- [ ] **Step 1: 用技能渲染 none 变体骨架**

```bash
bash .agents/skills/new-service/scripts/new_service.sh price none
```

- [ ] **Step 2: AGENTS.md 错误码表加一行**

在 `10000–19999` 行下方插入:

```markdown
| — | price(无对外出口,业务错误只进日志,不占分段) |
```

- [ ] **Step 3: 改写 `internal/price/config/config.go`**

```go
// Config price 服务的全部配置。
type Config struct {
	Log       log.Config       `yaml:"log"`
	App       app.Config       `yaml:"app"`
	Telemetry telemetry.Config `yaml:"telemetry"`
	Pgsql     pgsql.Config     `yaml:"pgsql"`
	Redis     redis.Config     `yaml:"redis"`
	Collector Collector        `yaml:"collector"`
	Exchanges map[string]Exchange `yaml:"exchanges"`
}

// Collector 采集器的通用参数,与具体交易所无关。
type Collector struct {
	// ReloadInterval 订阅集重载周期,零值取 5m。
	ReloadInterval time.Duration `yaml:"reload_interval"`
	// MaxBackfillWindow 新标的首次补洞的最大回溯窗口,零值取 720h(30 天)。
	MaxBackfillWindow time.Duration `yaml:"max_backfill_window"`
	// BackfillConcurrency 同时补洞的订阅数,零值取 2。
	BackfillConcurrency int `yaml:"backfill_concurrency"`
	// KlineQueueSize kline 队列容量,满即阻塞上游(不可丢)。零值取 1024。
	KlineQueueSize int `yaml:"kline_queue_size"`
	// SnapshotQueueSize ticker/depth 队列容量,满即弃旧(可丢)。零值取 256。
	SnapshotQueueSize int `yaml:"snapshot_queue_size"`
}

// Exchange 单个交易所的连接与限速参数。
type Exchange struct {
	Enabled           bool          `yaml:"enabled"`
	WSURL             string        `yaml:"ws_url"`
	RESTURL           string        `yaml:"rest_url"`
	MaxStreamsPerConn int           `yaml:"max_streams_per_conn"`
	RESTPerSecond     float64       `yaml:"rest_per_second"`
	RESTBurst         int           `yaml:"rest_burst"`
	DialTimeout       time.Duration `yaml:"dial_timeout"`
	ImportQuotes      []string      `yaml:"import_quotes"`
}
```

- [ ] **Step 4: 写 `configs/price.yaml`**

```yaml
# price 服务配置:只放声明式参数。本期只做现货。

log:
  name: price
  level: info
  format: json

app:
  stop_timeout: 30s

telemetry:
  exporter: stdout

pgsql:
  write: "postgres://app:app@127.0.0.1:5432/app?sslmode=disable"
  max_open_conns: 20
  conn_max_lifetime: 30m

redis:
  addrs: ["127.0.0.1:6379"]
  db: 1 # 与其它服务错开,避免最新值 key 与任务队列同库

collector:
  reload_interval: 5m
  max_backfill_window: 720h # 30 天;更早的历史要人显式跑 backfill
  backfill_concurrency: 2
  kline_queue_size: 1024
  snapshot_queue_size: 256

exchanges:
  binance:
    enabled: true
    ws_url: "wss://stream.binance.com:9443/stream"
    rest_url: "https://api.binance.com"
    max_streams_per_conn: 1024 # 待核实:见 Task 6 Step 1
    rest_per_second: 10
    rest_burst: 20
    dial_timeout: 10s
    import_quotes: ["USDT"]
  okx:
    enabled: true
    ws_url: "wss://ws.okx.com:8443/ws/v5/public"
    rest_url: "https://www.okx.com"
    max_streams_per_conn: 240 # 待核实:见 Task 7 Step 1
    rest_per_second: 5
    rest_burst: 10
    dial_timeout: 10s
    import_quotes: ["USDT"]
```

- [ ] **Step 5: `make check`**

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(price): 渲染 none 变体骨架与配置结构"
```

---

### Task 2: 迁移与数据模型

**Files:**
- Create: `migrations/price/<时间戳>_create_price_tables.sql`(用 `make migrate-create SVC=price NAME=create_price_tables` 生成文件名)
- Create: `internal/price/model/{instrument.go,subscription.go,kline.go}`

**Interfaces:**
- Produces:
  - `model.Instrument{ID int64; Exchange, Market, NativeSymbol, Symbol, Base, Quote string; Status int32; UpdatedAt time.Time}`
  - `model.Subscription{ID int64; Exchange, Market, NativeSymbol, StreamType, Interval string; Enabled bool}`
  - `model.Kline{Exchange, Market, NativeSymbol, Interval string; OpenTime int64; Open, High, Low, Close, Volume, QuoteVolume string; Source int32}`
  - 常量:`InstrumentStatusTrading=1`、`InstrumentStatusDelisted=2`;`StreamKline="kline"`、`StreamTicker="ticker"`、`StreamDepth="depth"`;`KlineSourceStream=1`、`KlineSourceBackfill=2`

- [ ] **Step 1: 写迁移**

```sql
-- +goose Up
CREATE TABLE price_instruments (
    id BIGSERIAL PRIMARY KEY,
    exchange TEXT NOT NULL, market TEXT NOT NULL, native_symbol TEXT NOT NULL,
    symbol TEXT NOT NULL, base TEXT NOT NULL, quote TEXT NOT NULL,
    status INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_instrument UNIQUE (exchange, market, native_symbol)
);
CREATE TABLE price_subscriptions (
    id BIGSERIAL PRIMARY KEY,
    exchange TEXT NOT NULL, market TEXT NOT NULL, native_symbol TEXT NOT NULL,
    stream_type TEXT NOT NULL, interval TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_subscription UNIQUE (exchange, market, native_symbol, stream_type, interval)
);
CREATE TABLE price_klines (
    exchange TEXT NOT NULL, market TEXT NOT NULL, native_symbol TEXT NOT NULL,
    interval TEXT NOT NULL, open_time BIGINT NOT NULL,
    open NUMERIC NOT NULL, high NUMERIC NOT NULL, low NUMERIC NOT NULL, close NUMERIC NOT NULL,
    volume NUMERIC NOT NULL, quote_volume NUMERIC NOT NULL,
    source INT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_kline PRIMARY KEY (exchange, market, native_symbol, interval, open_time)
);

-- +goose Down
DROP TABLE price_klines;
DROP TABLE price_subscriptions;
DROP TABLE price_instruments;
```

- [ ] **Step 2: 写三个 model 文件**,每个带包/类型/常量注释,`TableName()` 返回上面的表名。价格用 `string` 承载 NUMERIC——交易所给的是十进制字符串,转 float64 会丢精度。

- [ ] **Step 3: `make migrate-up SVC=price` 跑通,再 `make migrate-down SVC=price` 验证可回滚,最后再 up 一次**

- [ ] **Step 4: `make check`**

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(price): 三表迁移与数据模型"
```

---

### Task 3: repo 层

**Files:**
- Create: `internal/price/repo/{doc.go,errors.go,instrument.go,subscription.go,kline.go,latest.go}`
- Test: `internal/price/repo/{kline_test.go,subscription_test.go}`

**Interfaces:**
- Consumes: Task 2 的 model
- Produces:
  - `repo.ErrRowNotFound`
  - `NewInstrument(*gorm.DB) *Instrument`,方法 `UpsertAll(ctx, []model.Instrument) error`(按唯一键 upsert)、`MarkDelistedExcept(ctx, exchange, market string, keep []string) error`
  - `NewSubscription(*gorm.DB) *Subscription`,方法 `ListEnabled(ctx) ([]model.Subscription, error)`
  - `NewKline(*gorm.DB) *Kline`,方法 `Upsert(ctx, []model.Kline) error`(主键冲突时覆盖全部值列)、`MaxOpenTime(ctx, exchange, market, symbol, interval string) (int64, bool, error)`
  - `NewLatest(redis.UniversalClient) *Latest`,方法 `Set(ctx, key string, payload []byte) error`;key 形如 `price:{exchange}:{market}:{symbol}:{stream}`

- [ ] **Step 1: 写失败测试**——沿用本仓既有做法:GORM DryRun + `tests.DummyDialector` + 捕获日志断言 SQL 形状,不连真库、不引新依赖(照抄 `internal/payment/repo/order_test.go` 的 `capturingLogger`)。

```go
func TestKlineUpsert_ConflictTargetIsPrimaryKey(t *testing.T) {
	lg := &capturingLogger{}
	db := newDryRun(t, lg)
	r := NewKline(db)

	_ = r.Upsert(context.Background(), []model.Kline{{
		Exchange: "binance", Market: "spot", NativeSymbol: "BTCUSDT",
		Interval: "1m", OpenTime: 1700000000000, Open: "1", High: "2", Low: "1", Close: "2",
		Volume: "10", QuoteVolume: "20", Source: model.KlineSourceStream,
	}})

	for _, want := range []string{"ON CONFLICT", "exchange", "native_symbol", "open_time", "DO UPDATE"} {
		if !strings.Contains(lg.sql, want) {
			t.Errorf("生成的 SQL 缺少 %q:\n%s", want, lg.sql)
		}
	}
}

func TestSubscriptionListEnabled_FiltersDisabled(t *testing.T) {
	lg := &capturingLogger{}
	db := newDryRun(t, lg)
	_, _ = NewSubscription(db).ListEnabled(context.Background())
	if !strings.Contains(lg.sql, "enabled") {
		t.Errorf("查询未按 enabled 过滤:\n%s", lg.sql)
	}
}
```

- [ ] **Step 2: 运行,确认失败**

Run: `go test ./internal/price/repo/ -run 'TestKlineUpsert|TestSubscriptionListEnabled' -v`
Expected: FAIL(`NewKline` 未定义)

- [ ] **Step 3: 实现**。Upsert 用 `clause.OnConflict{Columns: 主键五列, DoUpdates: clause.AssignmentColumns([...值列...])}`;`MaxOpenTime` 用 `Select("max(open_time)")` 并以 `sql.NullInt64` 承接空表。

- [ ] **Step 4: 运行,确认通过;`make check`**

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(price): 仓储层(三表 upsert 与 Redis 最新值)"
```

---

### Task 4: 中立类型与 Exchange 接口

**Files:**
- Create: `internal/price/exchange/exchange.go`
- Test: `internal/price/exchange/exchange_test.go`

**Interfaces:**
- Produces(后续 Task 6/7/8/10 全部依赖这些名字):

```go
// Sub 一条订阅,来自 subscriptions 表,已是交易所原生形态。
type Sub struct {
	Market, NativeSymbol, StreamType, Interval string
}

// ConnPlan 一条 ws 连接的声明式描述:交易所包只产出它,不负责怎么连。
type ConnPlan struct {
	URL        string   // 完整拨号地址(合并流形态可能已把订阅编进 query)
	Subscribe  [][]byte // 连上后按序发送的订阅帧;为空表示订阅已在 URL 里
	Subs       []Sub    // 本连接覆盖的订阅,供日志与补洞使用
	ClientPing []byte   // 需要客户端主动心跳时的帧内容;nil 表示由服务端 ping
	PingEvery  time.Duration // ClientPing 非 nil 时的发送间隔
}

// Kline 中立 K 线,只在已收线时产出。
type Kline struct {
	Market, NativeSymbol, Interval string
	OpenTime                       int64 // UTC 毫秒
	Open, High, Low, Close, Volume, QuoteVolume string
}

// Snapshot 中立快照(ticker 或 depth),只留最新值。
type Snapshot struct {
	Market, NativeSymbol, StreamType string
	EventTime                        int64  // 交易所事件时间,UTC 毫秒
	Payload                          []byte // 归一化后的 JSON,直接写 Redis
}

// Event 一帧解码结果:三个指针至多一个非 nil;全 nil 表示该帧无需处理
// (心跳应答、订阅确认、未收线的 K 线)。
type Event struct {
	Kline    *Kline
	Snapshot *Snapshot
}

// Exchange 一家交易所的协议翻译。实现者不得触碰存储、重连与限速。
type Exchange interface {
	Name() string
	// Plan 把订阅切分成若干条连接的计划;切分上限由实现按自身文档决定。
	Plan(subs []Sub) ([]ConnPlan, error)
	// Decode 解一帧原始消息。无法识别的帧返回零值 Event 与 nil error——
	// 交易所会推送订阅确认、心跳应答等与业务无关的帧,它们不是错误。
	Decode(raw []byte) (Event, error)
	// Instruments 拉全量交易对(REST)。
	Instruments(ctx context.Context, market string) ([]Instrument, error)
	// Klines 拉一段历史 K 线,返回一律按开盘时间正序。
	// 返回的第二个值为下一页起点;为 0 表示已到 end。
	Klines(ctx context.Context, s Sub, start, end int64) ([]Kline, int64, error)
}

// Instrument 中立交易对。
type Instrument struct {
	Market, NativeSymbol, Symbol, Base, Quote string
	Trading                                   bool
}
```

- [ ] **Step 1: 写失败测试**——本任务只有类型,唯一值得锚定的是 `Event` 的三态语义:

```go
func TestEvent_ZeroValueMeansIgnorable(t *testing.T) {
	var e Event
	if e.Kline != nil || e.Snapshot != nil {
		t.Fatal("零值 Event 应表示「本帧无需处理」")
	}
}
```

- [ ] **Step 2: 运行确认失败(包不存在)** — `go test ./internal/price/exchange/`

- [ ] **Step 3: 写 `exchange.go`**,每个导出类型与字段都带注释,尤其写清 `Decode` 为何用「零值 + nil error」而不是 error 表示可忽略帧。

- [ ] **Step 4: 通过 + `make check`**

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(price): 中立事件类型与 Exchange 接口"
```

---

### Task 5: ws 连接生命周期

**Files:**
- Create: `internal/price/stream/{doc.go,conn.go,manager.go}`
- Test: `internal/price/stream/conn_test.go`
- Modify: `go.mod`(`go get github.com/coder/websocket`)

**Interfaces:**
- Consumes: `exchange.ConnPlan`、`exchange.Event`
- Produces:
  - `stream.Handler func(exchange.Event)`
  - `stream.OnReady func(subs []exchange.Sub)` — 连接进入可用状态的回调,补洞挂在这里
  - `NewConn(plan exchange.ConnPlan, dec Decoder, h Handler, ready OnReady, logger *slog.Logger, backoff Backoff) *Conn`
  - `(*Conn).Run(ctx) error` — 阻塞直到 ctx 取消;内部自愈重连,不把断线当错误返回
  - `type Decoder interface{ Decode([]byte) (exchange.Event, error) }`
  - `type Backoff struct{ Min, Max time.Duration }`,`(Backoff).Next(attempt int) time.Duration`(指数 + 抖动,封顶 Max)

- [ ] **Step 1: 写失败测试**——用 `httptest.NewServer` 起一个真 ws 服务端,不打真实交易所:

```go
func TestConn_ReconnectsAndFiresReadyEachTime(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil { return }
		n := conns.Add(1)
		if n == 1 {
			c.Close(websocket.StatusInternalError, "强制断开,触发重连") // 第一次立刻断
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"ok":1}`))
		<-r.Context().Done()
	}))
	defer srv.Close()

	var ready atomic.Int32
	got := make(chan struct{}, 1)
	c := NewConn(
		exchange.ConnPlan{URL: "ws" + strings.TrimPrefix(srv.URL, "http")},
		decoderFunc(func(b []byte) (exchange.Event, error) {
			select { case got <- struct{}{}: default: }
			return exchange.Event{}, nil
		}),
		func(exchange.Event) {},
		func([]exchange.Sub) { ready.Add(1) },
		quietLogger(), Backoff{Min: time.Millisecond, Max: 10 * time.Millisecond},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("重连后未收到消息")
	}
	if ready.Load() < 2 {
		t.Errorf("OnReady 触发 %d 次, want ≥2(首连 + 重连各一次)", ready.Load())
	}
}
```

- [ ] **Step 2: 运行确认失败** — `go test ./internal/price/stream/ -run TestConn_Reconnects -v`

- [ ] **Step 3: 实现 `conn.go`**:拨号(带 `DialTimeout`)→ 发订阅帧 → 触发 `OnReady` → 读循环(每帧交 `Decode`,非 nil error 只 Warn 不断连——一帧解不动不该杀连接)→ 断开则按 `Backoff` 重来。`ClientPing` 非 nil 时另起一个 ticker 协程发心跳。**ctx 取消是唯一的正常退出路径,返回 nil。**

- [ ] **Step 4: 实现 `manager.go`**:持有一组 `*Conn`,`Rebuild(plans []ConnPlan)` 停掉旧的、起新的;`Manager` 实现 `app.Component`。

- [ ] **Step 5: 补一个测试**——订阅集变更时旧连接必须停:

```go
func TestManager_RebuildStopsOldConns(t *testing.T) { /* 断言旧 Conn 的 ctx 已取消 */ }
```

- [ ] **Step 6: 通过 + `make check`**

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "feat(price): ws 连接生命周期(退避重连与订阅集重建)"
```

---

### Task 6: Binance 协议翻译

**Files:**
- Create: `internal/price/exchange/binance/{doc.go,binance.go,ws.go,rest.go}`
- Test: `internal/price/exchange/binance/ws_test.go`

**Interfaces:**
- Consumes: Task 4 的 `exchange.*`;`pkg/httpc` 的 `*httpc.Client`
- Produces: `binance.New(cfg Config) *Binance`,实现 `exchange.Exchange`;`binance.Config{WSURL, RESTURL string; MaxStreamsPerConn int; HTTP *httpc.Client}`

- [ ] **Step 1: 核实并记录**——打开 Binance 现货 ws 文档,把这四个数字确认后写进 `doc.go` 包注释(标注核实日期):单连接最大流数、每秒入站消息上限、单连接是否 24 小时强制断开、服务端 ping 间隔与 pong 超时。若与 `configs/price.yaml` 的 `max_streams_per_conn: 1024` 不符,以文档为准改配置。

- [ ] **Step 2: 写失败测试**(报文取自已核实的官方样例):

```go
func TestDecode_ClosedKlineOnly(t *testing.T) {
	closed := []byte(`{"stream":"bnbbtc@kline_1m","data":{"e":"kline","E":1672515782136,"s":"BNBBTC","k":{"t":1672515780000,"T":1672515839999,"s":"BNBBTC","i":"1m","o":"0.001","c":"0.002","h":"0.0025","l":"0.0015","v":"1000","q":"1","x":true}}}`)
	open := bytes.Replace(closed, []byte(`"x":true`), []byte(`"x":false`), 1)
	b := New(Config{})

	ev, err := b.Decode(closed)
	if err != nil { t.Fatalf("Decode 已收线帧出错: %v", err) }
	if ev.Kline == nil { t.Fatal("已收线的 K 线应产出事件") }
	if ev.Kline.OpenTime != 1672515780000 {
		t.Errorf("OpenTime = %d, want 1672515780000(取 k.t 开盘时间,不是 k.T)", ev.Kline.OpenTime)
	}
	if ev.Kline.Close != "0.002" { t.Errorf("Close = %q, want \"0.002\"", ev.Kline.Close) }

	ev, err = b.Decode(open)
	if err != nil { t.Fatalf("Decode 未收线帧不该报错: %v", err) }
	if ev.Kline != nil { t.Error("未收线的 K 线不得产出事件") }
}

func TestDecode_UnknownFrameIsIgnorable(t *testing.T) {
	ev, err := New(Config{}).Decode([]byte(`{"result":null,"id":1}`))
	if err != nil { t.Fatalf("订阅确认帧不该报错: %v", err) }
	if ev.Kline != nil || ev.Snapshot != nil { t.Error("订阅确认帧不该产出事件") }
}

func TestPlan_SplitsByStreamLimit(t *testing.T) {
	subs := make([]exchange.Sub, 5)
	for i := range subs {
		subs[i] = exchange.Sub{Market: "spot", NativeSymbol: "S" + strconv.Itoa(i), StreamType: exchange.StreamKline, Interval: "1m"}
	}
	plans, err := New(Config{WSURL: "wss://x/stream", MaxStreamsPerConn: 2}).Plan(subs)
	if err != nil { t.Fatal(err) }
	if len(plans) != 3 { t.Errorf("连接数 = %d, want 3(5 条订阅,每连接 2 条)", len(plans)) }
}
```

- [ ] **Step 3: 运行确认失败** — `go test ./internal/price/exchange/binance/ -v`

- [ ] **Step 4: 实现**。`Plan` 把订阅翻成 `<symbol小写>@kline_<interval>` / `@ticker` / `@depth20@100ms`,按 `MaxStreamsPerConn` 切分并拼进合并流 URL 的 `streams=` 参数(此形态无需订阅帧,`Subscribe` 留空,`ClientPing` 为 nil——服务端主动 ping)。`Decode` 先取 `data.e` 分派。`Klines` 走 `GET /api/v3/klines`,参数 `symbol/interval/startTime/endTime/limit`,返回已是正序,下一页起点取最后一根的开盘时间 + 一个周期。`Instruments` 走 `GET /api/v3/exchangeInfo`,按 `import_quotes` 与 `status=="TRADING"` 过滤。

- [ ] **Step 5: 通过 + `make check`**

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(price): Binance 协议翻译"
```

---

### Task 7: OKX 协议翻译

**Files:**
- Create: `internal/price/exchange/okx/{doc.go,okx.go,ws.go,rest.go}`
- Test: `internal/price/exchange/okx/ws_test.go`

**Interfaces:**
- Produces: `okx.New(cfg Config) *OKX`,实现 `exchange.Exchange`;`okx.Config` 字段与 binance 同名

- [ ] **Step 1: 核实并记录**——确认后写进 `doc.go`(标注核实日期):单连接订阅数上限、`candle<bar>` 的全部 bar 拼写(尤其小时/天/周是否大写)、`books5` 的推送频率、`/api/v5/market/candles` 与 `/api/v5/market/history-candles` 各自的 limit 上限、时间参数 `after`/`before` 的开闭语义与返回排序方向。

- [ ] **Step 2: 写失败测试**

```go
func TestDecode_ConfirmFlagGatesKline(t *testing.T) {
	// candle 数组:[ts,o,h,l,c,vol,volCcy,volCcyQuote,confirm],confirm 在索引 8
	confirmed := []byte(`{"arg":{"channel":"candle1m","instId":"BTC-USDT"},"data":[["1597026383085","3.721","3.743","3.677","3.708","8422410","22698348.04","12698348.04","1"]]}`)
	unconfirmed := bytes.Replace(confirmed, []byte(`,"1"]`), []byte(`,"0"]`), 1)
	o := New(Config{})

	ev, err := o.Decode(confirmed)
	if err != nil { t.Fatalf("Decode 出错: %v", err) }
	if ev.Kline == nil { t.Fatal("confirm=1 应产出事件") }
	if ev.Kline.OpenTime != 1597026383085 { t.Errorf("OpenTime = %d", ev.Kline.OpenTime) }

	ev, err = o.Decode(unconfirmed)
	if err != nil { t.Fatalf("未收线帧不该报错: %v", err) }
	if ev.Kline != nil { t.Error("confirm=0 不得产出事件") }
}

func TestPlan_EmitsSubscribeFramesAndClientPing(t *testing.T) {
	plans, err := New(Config{WSURL: "wss://x/ws/v5/public", MaxStreamsPerConn: 10}).Plan(
		[]exchange.Sub{{Market: "spot", NativeSymbol: "BTC-USDT", StreamType: exchange.StreamKline, Interval: "1m"}})
	if err != nil { t.Fatal(err) }
	if len(plans) != 1 || len(plans[0].Subscribe) != 1 {
		t.Fatalf("OKX 需要订阅帧, got %+v", plans)
	}
	if !bytes.Contains(plans[0].Subscribe[0], []byte(`"candle1m"`)) {
		t.Errorf("订阅帧未带频道名: %s", plans[0].Subscribe[0])
	}
	if string(plans[0].ClientPing) != "ping" || plans[0].PingEvery == 0 {
		t.Error("OKX 要求客户端主动心跳,ConnPlan 必须声明")
	}
}
```

- [ ] **Step 3: 运行确认失败** — `go test ./internal/price/exchange/okx/ -v`

- [ ] **Step 4: 实现**。`Plan` 产出 `{"op":"subscribe","args":[{"channel":"candle1m","instId":"BTC-USDT"}]}` 帧,`ClientPing = []byte("ping")`、`PingEvery = 20s`(文档要求 <30s)。`Decode` 按 `arg.channel` 前缀分派;`pong` 与 `event:subscribe` 帧返回零值 Event。`Klines` 按 Step 1 的核实结果选端点,**内部反转成正序**后返回。`Instruments` 走 `GET /api/v5/public/instruments?instType=SPOT`。

- [ ] **Step 5: 通过 + `make check`**

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(price): OKX 协议翻译"
```

---

### Task 8: 共享限速令牌桶

**Files:**
- Create: `internal/price/ratelimit/bucket.go`
- Test: `internal/price/ratelimit/bucket_test.go`

**Interfaces:**
- Produces:`ratelimit.New(perSecond float64, burst int) *Bucket`;`(*Bucket).Wait(ctx) error`。用 `golang.org/x/time/rate` 薄封装(已在依赖图内,`go list -m all | grep golang.org/x/time` 确认;不在则 `go get`)。

- [ ] **Step 1: 引入依赖** — `go get golang.org/x/time` 并 `make tidy`。它当前不在模块图内(含间接),这是本计划引入的第二个新依赖;选它而非手写令牌桶,是因为 `Wait(ctx)` 的计时器回收与公平性容易写错,而该模块由 Go 团队维护、无传递依赖。

- [ ] **Step 2: 写失败测试**

```go
func TestBucket_SecondCallWaitsForRefill(t *testing.T) {
	b := New(10, 1) // 每秒 10 个,桶容量 1
	ctx := context.Background()
	if err := b.Wait(ctx); err != nil { t.Fatal(err) }
	start := time.Now()
	if err := b.Wait(ctx); err != nil { t.Fatal(err) }
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("第二次取用只等了 %v, want ≥50ms(令牌未耗尽说明没限住)", elapsed)
	}
}

func TestBucket_RespectsContextCancel(t *testing.T) {
	b := New(0.1, 1)
	_ = b.Wait(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx); err == nil {
		t.Error("ctx 超时后 Wait 应返回 error,而不是一直等")
	}
}
```

- [ ] **Step 3: 运行确认失败** — `go test ./internal/price/ratelimit/ -v`

- [ ] **Step 4: 实现**(约 20 行薄封装)。包注释写清:**每个交易所一个实例,由装配层构造并同时传给常驻采集器与 backfill 子命令**——两者共用一个桶是配额正确性的前提。

- [ ] **Step 5: 通过 + `make check`**

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(price): 交易所 REST 共享限速桶"
```

---

### Task 9: service 编排与背压

**Files:**
- Create: `internal/price/service/{doc.go,service.go,plan.go,route.go}`
- Test: `internal/price/service/route_test.go`

**Interfaces:**
- Consumes: Task 3 repo、Task 4 exchange、Task 5 stream
- Produces:
  - `service.Deps{Instruments InstrumentRepo; Subs SubscriptionRepo; Klines KlineRepo; Latest LatestRepo; Exchanges map[string]exchange.Exchange; Limits map[string]*ratelimit.Bucket}`
  - `service.New(cfg Config, deps Deps, logger *slog.Logger) *Price`
  - `(*Price).Plans(ctx) (map[string][]exchange.ConnPlan, error)` — 读订阅表,按交易所分组产出连接计划
  - `(*Price).Route(ev exchange.Event)` — 事件入队(kline 阻塞,snapshot 弃旧)
  - `(*Price).RunWriters(ctx) error` — 消费两个队列并落库/写 Redis
  - 各仓储接口在 service 侧声明(依赖倒置支点)

- [ ] **Step 1: 写失败测试**——锚定两条背压语义的差异,这是本服务最关键的设计:

```go
func TestRoute_SnapshotQueueDropsOldestWhenFull(t *testing.T) {
	svc := New(Config{SnapshotQueueSize: 2, KlineQueueSize: 8}, Deps{}, testLogger())
	for i := 0; i < 5; i++ {
		svc.Route(exchange.Event{Snapshot: &exchange.Snapshot{
			NativeSymbol: "BTCUSDT", StreamType: model.StreamDepth, EventTime: int64(i)}})
	}
	// 队列只留最新两帧:弃旧而不是弃新——旧快照没有价值
	got := svc.drainSnapshotsForTest()
	if len(got) != 2 || got[0].EventTime != 3 || got[1].EventTime != 4 {
		t.Errorf("留下的快照 = %+v, want EventTime 3、4", got)
	}
}

func TestRoute_KlineQueueNeverDrops(t *testing.T) {
	svc := New(Config{SnapshotQueueSize: 1, KlineQueueSize: 3}, Deps{}, testLogger())
	done := make(chan struct{})
	go func() {
		for i := 0; i < 4; i++ { // 第 4 条必然阻塞,直到有人消费
			svc.Route(exchange.Event{Kline: &exchange.Kline{OpenTime: int64(i)}})
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("kline 队列满时 Route 直接返回了——收线帧被丢弃,会在 K 线里留洞")
	case <-time.After(100 * time.Millisecond): // 阻塞住才是对的
	}
	svc.drainKlinesForTest() // 放水后应能收尾
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("消费后 Route 仍未返回")
	}
}
```

- [ ] **Step 2: 运行确认失败** — `go test ./internal/price/service/ -v`

- [ ] **Step 3: 实现**。kline 队列用无缓冲取舍之外的有界 channel + 阻塞发送;snapshot 队列满时先 `<-ch` 丢一个再发。`RunWriters` 批量攒 kline(最多 100 条或 1 秒)后一次 `Upsert`,减少往返。

- [ ] **Step 4: 通过 + `make check`**

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(price): 事件路由与按语义分流的背压"
```

---

### Task 10: 补洞

**Files:**
- Create: `internal/price/service/backfill.go`
- Test: `internal/price/service/backfill_test.go`

**Interfaces:**
- Produces:`(*Price).Backfill(ctx, ex string, subs []exchange.Sub) error`;`(*Price).OnReady(ex string) stream.OnReady`(返回可直接挂给 `stream.Conn` 的回调)

- [ ] **Step 1: 写失败测试**

```go
func TestBackfill_StartsAfterLastStoredKline(t *testing.T) {
	klines := &mockKlineRepo{maxOpenTime: 1700000000000, has: true}
	ex := &mockExchange{}
	svc := New(Config{MaxBackfillWindow: 720 * time.Hour}, Deps{
		Klines: klines, Exchanges: map[string]exchange.Exchange{"m": ex},
		Limits: map[string]*ratelimit.Bucket{"m": ratelimit.New(1000, 1000)},
	}, testLogger())

	sub := exchange.Sub{Market: "spot", NativeSymbol: "BTCUSDT", StreamType: exchange.StreamKline, Interval: "1m"}
	if err := svc.Backfill(context.Background(), "m", []exchange.Sub{sub}); err != nil {
		t.Fatal(err)
	}
	if ex.gotStart != 1700000000000+60_000 {
		t.Errorf("起点 = %d, want 上一根开盘时间 + 一个周期", ex.gotStart)
	}
}

func TestBackfill_FallsBackToMaxWindowWhenEmpty(t *testing.T) {
	// 库里没有任何 K 线时,起点取 now - MaxBackfillWindow,不能取 0(否则拉全部历史)
}

func TestBackfill_SkipsNonKlineSubs(t *testing.T) {
	// ticker/depth 订阅没有历史可补,不得调用 Klines
}
```

- [ ] **Step 2: 运行确认失败**

- [ ] **Step 3: 实现**。逐条订阅:`MaxOpenTime` → 定起点 → 循环 `Limits[ex].Wait(ctx)` + `Klines(...)` → `Upsert(source=KlineSourceBackfill)` → 用返回的下一页起点续,直到为 0。并发度取 `Config.BackfillConcurrency`。单条订阅失败只 Warn 并继续下一条——一个坏标的不该中断整批。

- [ ] **Step 4: 通过 + `make check`**

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(price): K 线缺口补齐"
```

---

### Task 11: 交易对导入与订阅集重载 job

**Files:**
- Create: `internal/price/service/instruments.go`、`internal/price/job/reload.go`
- Test: `internal/price/service/instruments_test.go`

**Interfaces:**
- Produces:
  - `(*Price).ImportInstruments(ctx, ex string) error`
  - `job.NewReload(svc ReloadService, mgr Rebuilder, interval time.Duration, logger *slog.Logger) app.Component`,其中 `ReloadService interface{ Plans(ctx) (map[string][]exchange.ConnPlan, error) }`、`Rebuilder interface{ Rebuild(string, []exchange.ConnPlan) }`

- [ ] **Step 1: 写失败测试**

```go
func TestImportInstruments_MarksMissingAsDelisted(t *testing.T) {
	repo := &mockInstrumentRepo{}
	ex := &mockExchange{instruments: []exchange.Instrument{{NativeSymbol: "BTCUSDT", Trading: true}}}
	svc := New(Config{}, Deps{Instruments: repo, Exchanges: map[string]exchange.Exchange{"m": ex}}, testLogger())

	if err := svc.ImportInstruments(context.Background(), "m"); err != nil { t.Fatal(err) }
	if len(repo.upserted) != 1 { t.Errorf("upsert 条数 = %d, want 1", len(repo.upserted)) }
	if repo.keptSymbols == nil {
		t.Fatal("必须调用 MarkDelistedExcept——交易所不再返回的标的要标下架而不是留着当有效")
	}
}
```

- [ ] **Step 2: 运行确认失败**

- [ ] **Step 3: 实现两者**。`reload.go` 的循环骨架与 `internal/payment/job/periodic.go` 同构(首轮立即执行、单轮出错只 Warn),但本服务不复用那份代码——它在另一个服务的包里,跨服务 import internal 是禁止的。

- [ ] **Step 4: 通过 + `make check`**

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(price): 交易对导入与订阅集重载"
```

---

### Task 12: 常驻装配与子命令

**Files:**
- Modify: `cmd/price/main.go`
- Create: `cmd/price/initial/oneshot.go`
- Modify: `cmd/price/initial/init_app.go`

**Interfaces:**
- Produces:`initial.App(ctx, config.Config, *slog.Logger) error`(常驻)、`initial.Instruments(ctx, c, logger, exchange string) error`、`initial.Backfill(ctx, c, logger, args BackfillArgs) error`;`BackfillArgs{Exchange, Market, Symbol, Interval string; From, To time.Time}`

- [ ] **Step 1: 写 `main.go` 的子命令路由**

```go
func main() {
	// 无参 = 常驻采集;子命令各自解析自己的 flag。
	// 这是本仓第一个子命令形态的服务:make run SVC=price 不带参数,
	// 因此「无参」必须是常驻模式,不能是打印用法。
	args := os.Args[1:]
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "":
		runDaemon(args)
	case "instruments":
		runInstruments(args)
	case "backfill":
		runBackfill(args)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q;可用:instruments、backfill,或不带参数常驻采集\n", sub)
		os.Exit(2)
	}
}
```

- [ ] **Step 2: 实现 `init_app.go`**:createInfra 起遥测、pgsql、redis;createServer 造交易所实现、限速桶、service、`stream.Manager`、reload job;**装配期先跑一次 `Plans` 并 `Rebuild`**,再注册组件。

- [ ] **Step 3: 实现 `oneshot.go`**:两个子命令只造 pgsql(backfill 还需 redis 不需要——不造)、httpc、限速桶与 service,跑完返回,不进 `pkg/app`。

- [ ] **Step 4: 手工验证**

```bash
make build
./bin/price -h            # 应提示用法
./bin/price nosuchcmd     # 应退出码 2 并提示可用子命令
```

- [ ] **Step 5: `make check`**

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(price): 常驻装配与两个子命令"
```

---

### Task 13: 端到端脚本

**Files:**
- Create: `test/e2e/price/run.sh`、`test/e2e/price/mockd/main.go`

**Interfaces:**
- Consumes: 全部前置任务。mock 同时提供 ws 与 REST 两个面,端口经 `E2E_WS_ADDR`/`E2E_REST_ADDR` 覆盖(照 `test/e2e/channel/run.sh` 的既有做法,不写死端口)。

- [ ] **Step 1: 写 mockd**:一个 HTTP 服务,`/ws` 升级为 ws 并按固定脚本推两帧已收线 K 线 + 一帧未收线 K 线 + 一帧盘口;`/api/v3/klines` 返回三根历史 K 线;`/api/v3/exchangeInfo` 返回两个交易对。

- [ ] **Step 2: 写 run.sh**:建库 → 迁移 → 起 mockd → 用指向 mock 的临时配置跑 `price instruments` → 断言 `price_instruments` 有 2 行 → 跑常驻若干秒 → 断言 `price_klines` 只有**已收线**那两根、且盘口 key 在 Redis 里 → 停进程 → 跑 `price backfill` → 断言历史三根已 upsert 且 `source=2`。

- [ ] **Step 3: `make e2e SVC=price` 全过**

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "test(price): 端到端脚本(mock ws 与 REST)"
```

---

### Task 14: 服务文档与终验

**Files:**
- Create: `docs/price/README.md`
- Modify: `AGENTS.md`(若「服务分层」需要登记 `exchange`/`stream`/`ratelimit` 三类新目录)

- [ ] **Step 1: 写 `docs/price/README.md`**,对照 `docs/payment/README.md` 的结构收敛为一页:业务定位、数据落点(表与 Redis key 形状)、两个子命令、背压与补洞机制表、故障矩阵、本地运行动线。

- [ ] **Step 2: 登记新目录**。price 引入了 `exchange`(协议翻译)、`stream`(连接生命周期)、`ratelimit`(配额)三类不在 AGENTS.md「服务分层」现有清单里的目录——按宪法第 6 条在同一次改动里补进那节。

- [ ] **Step 3: 终验**:`make check` 全绿;`make e2e SVC=price` 全过;有网络时用真配置跑 `./bin/price instruments` 确认两家交易所都能拉到交易对。

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "docs(price): 服务文档与分层登记"
```

---

## Self-Review 记录

- **Spec 覆盖**:§0 决策→全局约束;§1 边界→T1(none 变体、不占错误码段);§2 领域模型→T2/T3,方言归一→T4/T6/T7;§3 K 线正确性→T6/T7 的 `x`/`confirm` 用例 + T3 的 upsert 用例;§4 补洞→T10(触发抽象经 T5 的 `OnReady` 落地);§5 连接与背压→T5/T9;§6 陈旧语义→T3 的 `Latest`(不设 TTL,payload 带双时间戳);§7 故障矩阵→T5(重连)、T8(限速)、T9(队列)、T10(单条失败继续);§8 子命令→T12;§9 不变量→散落于各任务用例;§10 待核实→T6/T7 各自的 Step 1 强制现场核实;§11 未决→已在全局约束里定案。
- **类型一致性**:`exchange.Sub`、`ConnPlan`、`Event`、`Kline`、`Snapshot`、`Instrument` 全部在 T4 一次定义,后续任务只引用不重定义;`stream.OnReady` 的签名在 T5 定义、T10 产出实现;`ratelimit.Bucket` 在 T8 定义、T10/T12 消费。
- **有意的重复**:`job/reload.go` 的 ticker 骨架与 payment 的 `periodic.go` 形状相同但不共用——跨服务 import `internal/` 是禁止的,提取到 `pkg/` 又会把只有两个服务用的东西塞进模板层(宪法第 2、3 条)。等第三个服务也需要时再谈提取。
