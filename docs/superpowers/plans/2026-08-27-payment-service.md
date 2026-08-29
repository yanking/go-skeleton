# payment 服务（代收闭环）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 go-skeleton 上新建 payment 微服务：商户接入（HMAC 验签）→ 订单与状态机 → 静态选路调 channel → 三方回调/补单终态 → asynq 商户异步通知。

**Architecture:** both 变体（gRPC :9093 + gateway HTTP :8093），商户 API 经 grpc-gateway 转译、三方回调走 `WithMount` 原生 HTTP 面；同一 gRPC 端口实现 `payment.v1.PaymentService` 与仓内既有 `gateway.v1.GatewayService`（channel 补单对账零改动接入）。PostgreSQL 六表 + asynq（Redis）通知队列。

**Tech Stack:** Go 1.27 / buf + grpc-gateway / GORM(pgx) / asynq / 仓内 pkg（app·conf·errcode·httpc·log·pgsql·telemetry·transport + 新增 queue）。

**Spec:** `docs/superpowers/specs/2026-08-27-payment-service-design.md`（本计划的唯一论据来源，执行者两份都要读）

## Global Constraints

- 宪法五条全程有效：错误必须上抛；业务错误 `errcode.Wrap(cause, ec)` 双通道；`pkg/` 零业务概念；零硬编码（运行时配置只来自 `configs/payment.yaml`）；每任务收尾 `make check` 全绿（build/vet/test + gofmt 零漂移）才许提交。
- `.agents/go-style.md` 硬规则：注释/错误消息中文；文件名 snake_case；接口在使用方声明；表驱动测试 + 手写 mock（禁 testify）；依赖显式注入。
- 金额一律分（int64）、费率千分位；payment 错误码段 **50000–59999**；对外订单状态 `1 处理中 2 成功 3 失败`。
- **全新项目**：任何文档、注释、提交信息不得引用旧系统/遗留协议。
- 分支 `feature/payment-service`；提交信息末尾带（每个提交都要）：
  `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` 与 `Claude-Session: https://claude.ai/code/session_01YLfKFXhKGFGQPwiDVPGzTY`
- 只动本计划列出的文件；不顺手改 channel 或 pkg 既有代码。

---

### Task 1: 骨架渲染与错误码登记

**Files:**
- Create（脚本生成）: `cmd/payment/main.go`、`cmd/payment/initial/init_app.go`、`internal/payment/config/config.go`、`configs/payment.yaml`、`internal/payment/{service,repo,model,job,handler}/doc.go`、`openapi/`（聚合包，脚本检测缺失时落地）
- Modify: `AGENTS.md`（错误码分段表）

**Interfaces:**
- Produces: 可编译的 payment 空壳；`configs/payment.yaml`（grpc_addr `:9093` 放开、http_addr 保持注释——Task 12 放开）

- [ ] **Step 1: 渲染 both 变体骨架**

```bash
bash .agents/skills/new-service/scripts/new_service.sh payment both
```

- [ ] **Step 2: 调整端口**：`configs/payment.yaml` 的 `transport.grpc_addr` 改为 `":9093"`（注释说明与 channel :9092 错开）；`http_addr` **保持注释状态**（WithGateway 在 Task 12 一起放开，先配端口会装配期报错）。

- [ ] **Step 3: 登记错误码**：AGENTS.md 错误码分段表加一行：

```markdown
| 50000–59999 | payment（已用：50001 商户订单号重复、50002 金额超限、50003 指定渠道未绑定或不可用、50004 无可用渠道、50005 订单状态冲突、50006 商户状态受限） |
```

- [ ] **Step 4: 验证并提交**

```bash
make check   # 全绿
git add -A && git commit -m "feat(payment): 渲染 both 变体服务骨架并登记错误码分段"
```

---

### Task 2: proto 契约与代码生成

**Files:**
- Create: `api/payment/v1/payment.proto`
- Generate: `gen/payment/v1/*.pb.go`、`gen/payment/v1/*.pb.gw.go`、`openapi/payment/v1/payment.openapi.json`

**Interfaces:**
- Produces: `paymentv1.PaymentServiceServer`（3 RPC）、`paymentv1.RegisterPaymentServiceHandler`；消息字段如下（后续任务以生成代码为准）

- [ ] **Step 1: 写 proto**。注释格式看 `.agents/go-style.md`「proto 注释」节（RPC 注释首行是标题、空行后描述；字段注释成句）。骨架：

```proto
syntax = "proto3";
package payment.v1;
option go_package = "github.com/yanking/go-skeleton/gen/payment/v1;paymentv1";
import "google/api/annotations.proto";

message CreatePaymentOrderRequest {
  string app_id = 1;        // 商户应用标识。
  string mch_order_no = 2;  // 商户订单号，商户侧唯一。
  int64 amount = 3;         // 金额（分）。
  string currency = 4;      // 币种，如 INR。
  string channel_name = 5;  // 指定渠道名，空即按绑定优先级自动选路。
  string notify_url = 6;    // 终态异步通知地址，空即不通知。
  string payer_name = 7;    // 付款人姓名。
  string payer_phone = 8;   // 付款人手机号。
  string payer_email = 9;   // 付款人邮箱。
  int64 timestamp = 10;     // 毫秒时间戳，与服务器偏差 5 分钟内有效。
  string sign = 11;         // HMAC-SHA256 签名（规范见 /docs 签名说明）。
}
message CreatePaymentOrderResponse {
  string order_no = 1;      // 平台订单号。
  string pay_url = 2;       // 支付链接。
}
message QueryPaymentOrderRequest {
  string app_id = 1;
  string order_no = 2;      // 平台订单号，与 mch_order_no 至少其一。
  string mch_order_no = 3;
  int64 timestamp = 4;
  string sign = 5;
}
message QueryPaymentOrderResponse {
  string order_no = 1;
  string mch_order_no = 2;
  int32 status = 3;         // 1 处理中 2 成功 3 失败。
  int64 amount = 4;
  int64 fee = 5;            // 商户手续费（分）。
  string reference_no = 6;  // 渠道支付参考号，无则为空。
  int64 completed_at = 7;   // 终态毫秒时间戳，未完结为 0。
}
message ListAvailableChannelsRequest {
  string app_id = 1;
  string currency = 2;      // 空即返回全部币种。
  int64 timestamp = 3;
  string sign = 4;
}
message AvailableChannel {
  string channel_name = 1;
  string currency = 2;
  int64 limit_min = 3;      // 单笔下限（分），同渠道多实例取并集。
  int64 limit_max = 4;
}
message ListAvailableChannelsResponse { repeated AvailableChannel channels = 1; }

service PaymentService {
  rpc CreatePaymentOrder(CreatePaymentOrderRequest) returns (CreatePaymentOrderResponse) {
    option (google.api.http) = { post: "/v1/payment/orders", body: "*" };
  }
  rpc QueryPaymentOrder(QueryPaymentOrderRequest) returns (QueryPaymentOrderResponse) {
    option (google.api.http) = { post: "/v1/payment/orders/query", body: "*" };
  }
  rpc ListAvailableChannels(ListAvailableChannelsRequest) returns (ListAvailableChannelsResponse) {
    option (google.api.http) = { post: "/v1/payment/channels/query", body: "*" };
  }
}
```

- [ ] **Step 2: 生成并验证**

```bash
make proto    # 生成 pb/gw/openapi；若 openapi/ 聚合包 embed 报 glob 不匹配，确认 spec 落在 openapi/payment/v1/
make check
```

- [ ] **Step 3: Commit**：`git add -A && git commit -m "feat(payment): 商户面 proto 契约与生成代码"`

---

### Task 3: 数据库迁移与 model

**Files:**
- Create: `migrations/payment/<时间戳>_create_payment_tables.sql`（用 `make migrate-create SVC=payment NAME=create_payment_tables` 生成文件名）
- Create: `internal/payment/model/{merchant.go,channel_instance.go,order.go,callback.go,notification.go}`；删掉脚手架 `model/doc.go` 的占位（包注释挪到 merchant.go 或保留 doc.go 均可，但包注释仅一处）

**Interfaces:**
- Produces（后续任务全部依赖，字段名以此为准）:
  - `model.Merchant{ID, AppID, AppSecret, Name, Status int32, IPWhitelist string/*JSON []string*/, LimitMin, LimitMax int64, FeeRate int32, FeeExtra int32, CreatedAt, UpdatedAt}`，`TableName()="merchants"`；常量 `MerchantStatusNormal=1, MerchantStatusBanned=2`
  - `model.ChannelInstance{ID, ChannelName, MerchantNo, Currency, Enabled bool, LimitPaymentMin, LimitPaymentMax int64, CallbackHeaders string/*JSON*/, CallbackDataSource int32, CallbackReturn, CallbackIPWhitelist string, SyncedAt time.Time, CreatedAt, UpdatedAt}`，`TableName()="channel_instances"`
  - `model.MerchantChannel{ID, MerchantID, ChannelInstanceID int64, Priority int32, Enabled bool, CreatedAt, UpdatedAt}`，`TableName()="merchant_channels"`
  - `model.PaymentOrder{ID, OrderNo, MerchantID, MchOrderNo, Amount, Fee int64, Currency string, Status int32, ChannelInstanceID int64, OutOrderNo, ReferenceNo, PayURL, NotifyURL, Response string, CompletedAt *time.Time, NotifyStatus int32, NotifiedAt *time.Time, CreatedAt, UpdatedAt}`，`TableName()="payment_orders"`；常量 `OrderStatusCreated=1, OrderStatusSent=2, OrderStatusSuccess=3, OrderStatusFailed=4`；`NotifyStatusNone=0, NotifyStatusPending=1, NotifyStatusDone=3, NotifyStatusSkipped=4`；出参映射函数 `func OutStatus(s int32) int32`（created/sent→1，success→2，failed→3）
  - `model.Callback{ID, ChannelInstanceID int64, Source int32, Headers string/*JSON map*/, Query, Body, IP string, Status int32, OrderNo, Note string, CreatedAt}`，`TableName()="callbacks"`；常量 `CallbackSourceHTTP=1, CallbackSourceReconcile=2`；`CallbackStatusReceived=1, CallbackStatusVerified=2, CallbackStatusInvalid=3`
  - `model.OrderNotification{ID, OrderNo string, Attempt int32, ResponseCode int32, ResponseBody string, CreatedAt}`，`TableName()="order_notifications"`

- [ ] **Step 1: 生成迁移文件并填写**。goose 注解格式对照 `migrations/channel/20260826100000_create_channels.sql`（一个文件一对 Up/Down；TEXT JSON 列默认 `'[]'`/`'{}'`）。六表 DDL 要点：

```sql
-- +goose Up
CREATE TABLE merchants (
    id BIGSERIAL PRIMARY KEY,
    app_id TEXT NOT NULL, app_secret TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
    status INT NOT NULL DEFAULT 1,
    ip_whitelist TEXT NOT NULL DEFAULT '[]',
    limit_min BIGINT NOT NULL DEFAULT 0, limit_max BIGINT NOT NULL DEFAULT 0,
    fee_rate INT NOT NULL DEFAULT 0, fee_extra INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_merchant_app UNIQUE (app_id)
);
CREATE TABLE channel_instances (
    id BIGSERIAL PRIMARY KEY,
    channel_name TEXT NOT NULL, merchant_no TEXT NOT NULL, currency TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    limit_payment_min BIGINT NOT NULL DEFAULT 0, limit_payment_max BIGINT NOT NULL DEFAULT 0,
    callback_headers TEXT NOT NULL DEFAULT '[]', callback_data_source INT NOT NULL DEFAULT 1,
    callback_return TEXT NOT NULL DEFAULT '', callback_ip_whitelist TEXT NOT NULL DEFAULT '',
    synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_instance_route UNIQUE (channel_name, merchant_no, currency)
);
CREATE TABLE merchant_channels (
    id BIGSERIAL PRIMARY KEY,
    merchant_id BIGINT NOT NULL, channel_instance_id BIGINT NOT NULL,
    priority INT NOT NULL DEFAULT 100, enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_merchant_channel UNIQUE (merchant_id, channel_instance_id)
);
CREATE TABLE payment_orders (
    id BIGSERIAL PRIMARY KEY,
    order_no TEXT NOT NULL, merchant_id BIGINT NOT NULL, mch_order_no TEXT NOT NULL,
    amount BIGINT NOT NULL, fee BIGINT NOT NULL DEFAULT 0, currency TEXT NOT NULL,
    status INT NOT NULL DEFAULT 1,
    channel_instance_id BIGINT NOT NULL DEFAULT 0,
    out_order_no TEXT NOT NULL DEFAULT '', reference_no TEXT NOT NULL DEFAULT '',
    pay_url TEXT NOT NULL DEFAULT '', notify_url TEXT NOT NULL DEFAULT '',
    response TEXT NOT NULL DEFAULT '',
    completed_at TIMESTAMPTZ, notify_status INT NOT NULL DEFAULT 0, notified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uniq_order_no UNIQUE (order_no),
    CONSTRAINT uniq_merchant_order UNIQUE (merchant_id, mch_order_no)
);
CREATE INDEX idx_order_reconcile ON payment_orders (channel_instance_id, status, created_at);
CREATE INDEX idx_order_notify ON payment_orders (notify_status, completed_at);
CREATE TABLE callbacks (
    id BIGSERIAL PRIMARY KEY,
    channel_instance_id BIGINT NOT NULL, source INT NOT NULL DEFAULT 1,
    headers TEXT NOT NULL DEFAULT '{}', query TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '',
    ip TEXT NOT NULL DEFAULT '', status INT NOT NULL DEFAULT 1,
    order_no TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE order_notifications (
    id BIGSERIAL PRIMARY KEY,
    order_no TEXT NOT NULL, attempt INT NOT NULL,
    response_code INT NOT NULL DEFAULT 0, response_body TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_notification_order ON order_notifications (order_no, created_at);
-- +goose Down
DROP TABLE order_notifications; DROP TABLE callbacks; DROP TABLE payment_orders;
DROP TABLE merchant_channels; DROP TABLE channel_instances; DROP TABLE merchants;
```

- [ ] **Step 2: 写 model**。GORM tag 风格对照 `internal/channel/model/channel.go`（`gorm:"column:..."` 显式列名、`TableName()` 固定表名、金额单位注释）。字段与常量按上方 Produces 清单一字不差。
- [ ] **Step 3: `make check` 全绿后提交**：`git commit -m "feat(payment): 六表迁移与数据模型"`

---

### Task 4: pkg/queue（asynq 薄封装）

**Files:**
- Create: `pkg/queue/queue.go`、`pkg/queue/queue_test.go`
- Modify: `go.mod`（`go get github.com/hibiken/asynq@latest` 后 `make tidy`）、`AGENTS.md`（pkg 地图表加 `pkg/queue` 一行：「asynq 任务队列薄封装：Client 入队 + Worker 消费（实现 app.Component）」）

**Interfaces:**
- Produces:
  - `queue.Config{Addr string `yaml:"addr"`, Password string `yaml:"password"`, DB int `yaml:"db"`, Concurrency int `yaml:"concurrency"`}`，`Validate() error`（Addr 空报错）
  - `queue.NewClient(cfg Config) *Client`；`(*Client) Enqueue(ctx, typename string, payload []byte, opts ...Option) error`；`(*Client) Close() error`
  - `queue.MaxRetry(n int) Option`、`queue.ProcessIn(d time.Duration) Option`
  - `queue.NewWorker(cfg Config, logger *slog.Logger) *Worker`；`(*Worker) Handle(typename string, h func(ctx context.Context, payload []byte) error)`；Worker 实现 `app.Component`（`Name()="queue-worker"`；`Start` 阻塞跑 asynq server；`Stop` 优雅停）

- [ ] **Step 1: 写失败测试**（表驱动、标准库断言）：`Config.Validate` 空 Addr 报错、非空通过；`MaxRetry/ProcessIn` 选项映射为预期的 `[]asynq.Option`（导出内部转换函数 `options(opts []Option) []asynq.Option` 供测试，或经未导出函数 + 同包测试）；`Worker.Handle` 注册后重复注册同名 panic（asynq mux 行为，验证我们不吞它）。
- [ ] **Step 2: 跑测试确认失败**：`go test ./pkg/queue/ -v` → FAIL（未实现）。
- [ ] **Step 3: 实现**。包注释写明部署要求（专用 Redis 实例、noeviction、AOF）与错误语义（Handler 返回 error 即重试、nil 即完成，对齐宪法第 1 条）。`Enqueue` 内部 `asynq.NewTask(typename, payload)` + `client.EnqueueContext`；Worker.Start 里 `asynq.NewServer(asynq.RedisClientOpt{Addr, Password, DB}, asynq.Config{Concurrency, Logger: 适配 slog})` + `srv.Run(mux)`（Run 阻塞，Stop 调 `srv.Shutdown()`——阻塞循环交给 pkg/app 的约定）。
- [ ] **Step 4: 跑测试通过 + `make check`**（含 `make tidy`）。
- [ ] **Step 5: Commit**：`git commit -m "feat(queue): asynq 任务队列薄封装"`

---

### Task 5: sign 签名包（安全边界，TDD）

**Files:**
- Create: `internal/payment/sign/sign.go`、`internal/payment/sign/sign_test.go`

**Interfaces:**
- Produces:
  - `sign.Canonical(fields map[string]string) string` — 字段名 ASCII 升序拼 `k=v&…`
  - `sign.HMAC(secret, canonical string) string` — hex 小写 HMAC-SHA256
  - `sign.Verify(secret string, fields map[string]string, sig string) bool` — 常数时间比较（`hmac.Equal`）
  - `sign.FieldsFromProto(m proto.Message) (fields map[string]string, sig string)` — protoreflect 遍历标量字段：string 原文、int32/int64 十进制、bool true/false；proto3 未设置字段取零值形态（"0"/"false"/""）**也参与**；`sign` 字段单独返回不进 fields；字段名用 proto 字段名（snake_case）

- [ ] **Step 1: 写失败测试**（表驱动）：
  - Canonical：乱序 map → 升序拼串；含空值字段仍在串中（`a=&b=1`）。
  - HMAC：固定 secret+串 → 预计算的 hex 值（用 Go 现场算一次填进用例，测试防回归）。
  - Verify：正确签名 true；篡改一个字段 false；**剥离一个空值字段 false**（把带空 notify_url 签的名用于去掉该字段的 fields）；大小写变体 false。
  - FieldsFromProto：用 `paymentv1.CreatePaymentOrderRequest{Amount: 500, Timestamp: 1756..., AppId: "demo", Sign: "abc"}` 断言 fields 含 `amount=500`、`notify_url=`（零值在场）、不含 `sign`，sig=="abc"。
- [ ] **Step 2: `go test ./internal/payment/sign/ -v` → FAIL**
- [ ] **Step 3: 实现**（`crypto/hmac` + `crypto/sha256` + `google.golang.org/protobuf/reflect/protoreflect`；repeated/message 类型字段跳过——本契约请求全是标量，注释说明该约束）。
- [ ] **Step 4: 测试通过 + `make check`**
- [ ] **Step 5: Commit**：`git commit -m "feat(payment): 商户签名规范实现（HMAC-SHA256 全字段）"`

---

### Task 6: repo 层

**Files:**
- Create: `internal/payment/repo/{merchant.go,instance.go,binding.go,order.go,callback.go,notification.go}`；替换脚手架 `repo/doc.go` 包注释位置（包注释一处即可）

**Interfaces:**
- Consumes: Task 3 的 model。
- Produces（service 在 Task 8 按此声明接口；构造函数一律 `func NewX(db *gorm.DB) *X`）:
  - `repo.Merchant.FindByAppID(ctx, appID string) (*model.Merchant, error)` — 未命中返回 `ErrRowNotFound` 哨兵（本包 `var ErrRowNotFound = errors.New("记录不存在")`，各仓储共用）
  - `repo.Instance`：`ReplaceAll(ctx, rows []model.ChannelInstance) error`（事务：按三元组 upsert 更新全列+enabled=true+synced_at；库中不在本批三元组的行置 enabled=false）；`FindByID(ctx, id int64)`；`FindByRoute(ctx, channelName, merchantNo, currency string)`（未命中 ErrRowNotFound）；`ListEnabled(ctx)`
  - `repo.Binding.ListCandidates(ctx, merchantID int64, currency string) ([]model.ChannelInstance, error)` — JOIN：绑定 enabled 且实例 enabled 且币种相等，按 `merchant_channels.priority` 升序
  - `repo.Order`：`Create(ctx, *model.PaymentOrder) error`（唯一冲突翻译为 `ErrDuplicate` 哨兵——判 pgx 错误码 23505，`errors.Is` 不可得时用 `pgconn.PgError.Code`）；`FindByOrderNo(ctx, orderNo string)`；`FindForMerchant(ctx, merchantID int64, orderNo, mchOrderNo string)`（按有值的键查、校验归属）；`FindByOut(ctx, instanceID int64, outOrderNo string)`；`MarkSent(ctx, orderNo string, instanceID int64, outOrderNo, payURL, response string) (bool, error)`（`UPDATE … WHERE order_no=? AND status=1`，返回 RowsAffected>0）；`MarkFailedDispatch(ctx, orderNo string) (bool, error)`（同上条件更新 status=4、notify_status=4、completed_at=now）；`Transition(ctx, orderNo string, fn func(o *model.PaymentOrder) (*model.PaymentOrder, error)) error`（事务内 `clauses.Locking{Strength:"UPDATE"}` 取行 → fn 返回 nil,nil 即无变更 → 非 nil 则 Save；行不存在返回 ErrRowNotFound）；`ListUnfinished(ctx, instanceID int64, since time.Time) ([]model.PaymentOrder, error)`（status IN (1,2) AND created_at>=since）；`ListStaleCreated(ctx, before time.Time)`；`ListNotifyStuck(ctx, neverTriedBefore, lastTriedBefore time.Time) ([]string, error)`（notify_status=1 且［order_notifications 无记录且 completed_at<neverTriedBefore，或 max(created_at)<lastTriedBefore］，两段 SQL UNION）
  - `repo.Callback`：`Create(ctx, *model.Callback) error`（回填 ID）；`Mark(ctx, id int64, status int32, orderNo, note string) error`
  - `repo.Notification`：`Create(ctx, *model.OrderNotification) error`；`CountByOrder(ctx, orderNo string) (int64, error)`

- [ ] **Step 1: 实现全部方法**。风格对照 `internal/channel/repo/channel.go`（`db.WithContext(ctx)`、错误 `fmt.Errorf("动词: %w", err)` 包一次）。repo 层不写单测（薄层纪律，spec §14）。
- [ ] **Step 2: `make check` 全绿**（编译即验证签名一致）。
- [ ] **Step 3: Commit**：`git commit -m "feat(payment): 仓储层"`

---

### Task 7: channel_client（channel 服务客户端）

**Files:**
- Create: `internal/payment/channel_client/client.go`

**Interfaces:**
- Consumes: `gen/channel/v1`（仓内已有）。
- Produces（package channelclient；service 在 Task 8/9/10 按此声明接口并手写 mock）:
  - `channelclient.New(target string) (*Client, error)`（对照 `internal/channel/gateway_client/client.go`：`grpc.NewClient` + insecure，失败返回错误）；`Close() error`
  - `type Route struct{ ChannelName, MerchantNo, Currency string }`
  - `type OrderIn struct{ OrderNo string; Amount int64; PayerName, PayerPhone, PayerEmail, NotifyURL string }`
  - `type OrderOut struct{ PayURL, OutOrderNo, Response string }`
  - `CreateOrder(ctx, r Route, in OrderIn) (OrderOut, error)` → 调 `channelv1.PaymentOrder`
  - `type CallbackOut struct{ OrderNo, OutOrderNo string; CallbackType int32; Amount int64; ReferenceNo string }`
  - `VerifyCallback(ctx, r Route, header map[string]string, data string) (CallbackOut, error)` → 调 `channelv1.PaymentCallback`
  - `type Instance struct{ ChannelName, MerchantNo, Currency string; LimitPaymentMin, LimitPaymentMax int64; CallbackHeaders []string; CallbackDataSource int32; CallbackReturn, CallbackIPWhitelist string }`
  - `ListInstances(ctx) ([]Instance, error)` → 调 `channelv1.ListChannels`

- [ ] **Step 1: 实现**（薄转换层，无单测——真实 gRPC 交互由运行冒烟覆盖）。
- [ ] **Step 2: `make check`**；**Step 3: Commit** `git commit -m "feat(payment): channel 服务客户端"`

---

### Task 8: service·骨架与商户鉴权（TDD）

**Files:**
- Create: `internal/payment/service/{service.go,errcode.go,auth.go,auth_test.go}`；替换脚手架 `service/doc.go` 占位

**Interfaces:**
- Consumes: Task 5 sign、Task 6 repo 签名、Task 7 channelclient 类型。
- Produces:
  - `service.go`：接口声明（在使用方声明原则；签名与 Task 6/7 的 Produces 完全一致）：`MerchantRepo`、`InstanceRepo`、`BindingRepo`、`OrderRepo`、`CallbackRepo`、`NotificationRepo`、`ChannelClient`（含 CreateOrder/VerifyCallback/ListInstances）、`Notifier interface{ Enqueue(ctx, typename string, payload []byte, opts ...queue.Option) error }`
  - `type Config struct{ CallbackBaseURL string }`（装配期传入）
  - `func New(cfg Config, deps Deps, logger *slog.Logger) *Payment`，`type Deps struct{ Merchants MerchantRepo; Instances InstanceRepo; Bindings BindingRepo; Orders OrderRepo; Callbacks CallbackRepo; Notifications NotificationRepo; Channel ChannelClient; Queue Notifier }`
  - `errcode.go`：`var ErrDuplicateOrder = errcode.New(50001, "商户订单号重复", codes.AlreadyExists)`、`ErrAmountOutOfLimit = errcode.New(50002, "金额超出限额", codes.InvalidArgument)`、`ErrChannelNotBound = errcode.New(50003, "指定渠道未绑定或不可用", codes.FailedPrecondition)`、`ErrNoAvailableChannel = errcode.New(50004, "无可用渠道", codes.Unavailable)`、`ErrStateConflict = errcode.New(50005, "订单状态冲突", codes.FailedPrecondition)`、`ErrMerchantRestricted = errcode.New(50006, "商户状态受限", codes.PermissionDenied)`
  - `auth.go`：`func (s *Payment) Authenticate(ctx context.Context, fields map[string]string, sig string) (*model.Merchant, error)`；`func clientIP(ctx context.Context) string`（metadata `x-forwarded-for` 首值首跳，缺省回退 `peer.FromContext`）

- [ ] **Step 1: 写失败测试** `auth_test.go`（手写 mock MerchantRepo：函数字段结构体）。表驱动用例：app_id 未命中→10004；IP 不在白名单→10004（白名单 `["1.2.3.4"]`、来 IP `1.2.3.41` 必须拒——精确匹配用例）；白名单空→放行；时间戳偏差 6 分钟→10004；签名错→10004；status=2→50006；全对→返回商户。metadata 注入用 `metadata.NewIncomingContext(ctx, metadata.Pairs("x-forwarded-for", "1.2.3.4, 10.0.0.1"))`。断言错误码用 `errors.As(err, &errcode.Code{})` 取 Code 比对。
- [ ] **Step 2: `go test ./internal/payment/service/ -v` → FAIL**
- [ ] **Step 3: 实现**。鉴权顺序按 spec §3：查商户→IP→时间戳（`fields["timestamp"]` 解析毫秒，`math.Abs` 5 分钟窗）→ `sign.Verify` → 状态。查商户 ErrRowNotFound 与验签失败统一 `errcode.Wrap(cause, errcode.ErrUnauthenticated)`（对外不区分，防探测）。
- [ ] **Step 4: 测试通过 + `make check`**；**Step 5: Commit** `git commit -m "feat(payment): 商户鉴权链（IP 精确匹配/时间窗/HMAC 验签）"`

---

### Task 9: service·下单与选路（TDD）

**Files:**
- Create: `internal/payment/service/{order.go,order_test.go}`

**Interfaces:**
- Consumes: Task 8 的 Payment 结构与接口。
- Produces:
  - `type CreateOrderIn struct{ MchOrderNo string; Amount int64; Currency, ChannelName, NotifyURL, PayerName, PayerPhone, PayerEmail string }`
  - `func (s *Payment) CreateOrder(ctx, m *model.Merchant, in CreateOrderIn) (orderNo, payURL string, err error)`
  - 内部：`func newOrderNo() string`（`fmt.Sprintf("P%d%06d", time.Now().UnixMilli(), rand.IntN(1_000_000))`；Create 撞 uniq_order_no 重试一次）；`func fee(amount int64, rate, extra int32) int64`（`(amount*int64(rate)+500)/1000 + int64(extra)` 四舍五入）

- [ ] **Step 1: 写失败测试**（mock OrderRepo/BindingRepo/ChannelClient）。用例：参数缺失→10001；金额超商户限额→50002；指定 channel_name 过滤后为空→50003；未指定且限额筛空→50002；候选 A 失败 B 成功→订单先 Create(status=1) 再 MarkSent，payURL 来自 B；全部候选失败→MarkFailedDispatch 且返回 50004；`(merchant_id,mch_order_no)` 冲突（Create 返回 repo.ErrDuplicate）→50001；MarkSent 返回 false（并发回调已推进）→ 不报错、仍返回 payURL；fee 计算表用例（500×30‰+0=15、999×25‰四舍五入、+extra）。
- [ ] **Step 2: FAIL 确认** → **Step 3: 实现**（NotifyURL 给渠道 = `cfg.CallbackBaseURL + "/callbacks/payment/" + strconv.FormatInt(instanceID, 10)`；候选循环内错误只 Warn 日志＋记入最后错误，最终 `errcode.Wrap(lastErr, ErrNoAvailableChannel)`）。
- [ ] **Step 4: 通过 + `make check`**；**Step 5: Commit** `git commit -m "feat(payment): 下单与静态绑定选路"`

---

### Task 10: service·状态机与回调处理（TDD）

**Files:**
- Create: `internal/payment/service/{state.go,state_test.go,callback.go,callback_test.go}`

**Interfaces:**
- Consumes: Task 8 接口、Task 7 `channelclient.CallbackOut`。
- Produces:
  - `type ChannelResult struct{ InstanceID int64; OrderNo, OutOrderNo string; CallbackType int32; Amount int64; ReferenceNo string }`
  - `func (s *Payment) ApplyChannelResult(ctx context.Context, r ChannelResult) (converged bool, err error)` — spec §5 转移表逐格实现：converged=false 表示「标无效留人工」（金额不符/矛盾态）；幂等重复返回 true；仅基础设施错误返回 err
  - `type CallbackIn struct{ InstanceID int64; Headers map[string]string; Query, RawBody, IP string }`
  - `type CallbackReply struct{ HTTPStatus int; Body string }`
  - `func (s *Payment) HandleChannelCallback(ctx context.Context, in CallbackIn) CallbackReply` — spec §7 七步；**不返回 error**（应答语义内化：正常/业务无效→200+callback_return，IP 拒→403，实例不存在→404，基础设施错→500）
  - `const TaskNotify = "payment:notify"`；内部 `func (s *Payment) enqueueNotify(ctx, orderNo string)`（payload `{"order_no":"…"}`，`queue.MaxRetry(15)`；失败只 Warn——notify-sweep 兜底）

- [ ] **Step 1: 写 state_test.go 失败测试**：转移表全覆盖（mock OrderRepo.Transition 用真闭包驱动一个内存 order 副本）——sent+成功金额相等→success+completed_at+notify_status=1（notify_url 空则 4）+enqueue 调用；created+成功→success（宕机残留路径）；failed+成功→success；sent+失败→failed；created+失败→failed；金额不符→converged=false 且订单不变；success+成功→幂等 true 无副作用；success+失败→converged=false 订单不变；订单不存在→ErrRowNotFound 上抛（10002 由调用方翻译）。
- [ ] **Step 2: 写 callback_test.go 失败测试**（mock CallbackRepo/InstanceRepo/ChannelClient）：正常链路→Create 回调行→VerifyCallback→ApplyChannelResult→Mark(verified)→200+callback_return；实例不存在→404 且回调行已落库；IP 白名单不符→403+Mark(invalid)；channel 返回 PermissionDenied（业务验签失败）→200+callback_return+Mark(invalid)；channel 返回 Unavailable→500；Create 回调行失败→500。
- [ ] **Step 3: FAIL 确认** → **Step 4: 实现**。要点：ApplyChannelResult 先按 OrderNo 查、空则 `FindByOut(instanceID, outOrderNo)`；channel 业务错 vs 基础设施错用 `status.Code(err)` 区分（PermissionDenied/InvalidArgument/NotFound→业务无效；其余→500）；enqueue 在 Transition 成功返回后执行（事务外）。
- [ ] **Step 5: 通过 + `make check`**；**Step 6: Commit** `git commit -m "feat(payment): 订单状态机与三方回调处理"`

---

### Task 11: service·查询、可用渠道、实例同步、通知发送（TDD）

**Files:**
- Create: `internal/payment/service/{query.go,query_test.go,sync.go,sync_test.go,notify.go,notify_test.go}`

**Interfaces:**
- Consumes: Task 8 接口；`pkg/httpc`（通知发送用，service.Deps 增加 `HTTP interface{ PostJSON(ctx context.Context, url string, header map[string]string, body any, timeout time.Duration) (int, string, error) }`——在 service.go 补声明，httpc.Client 天然满足）。
- Produces:
  - `type OrderView struct{ OrderNo, MchOrderNo string; Status int32; Amount, Fee int64; ReferenceNo string; CompletedAt int64 }`；`func (s *Payment) QueryOrder(ctx, m *model.Merchant, orderNo, mchOrderNo string) (OrderView, error)`（两键皆空→10001；未命中/他人订单→10002；Status 用 `model.OutStatus`）
  - `type ChannelView struct{ ChannelName, Currency string; LimitMin, LimitMax int64 }`；`func (s *Payment) AvailableChannels(ctx, m *model.Merchant, currency string) ([]ChannelView, error)`（候选按 (channel_name,currency) 聚合，限额取区间并集 min/max）
  - `func (s *Payment) SyncInstances(ctx context.Context) error`（ListInstances→映射 model（CallbackHeaders JSON 编码）→ReplaceAll）
  - `func (s *Payment) SendNotify(ctx context.Context, payload []byte) error` — worker handler：解 order_no→取单（notify_status≠1 直接 nil）→取商户→通知体 map`{order_no: mch单号, sys_order_no, status, amount, fee, reference_no, timestamp}`+`sign`（sign.HMAC 同规范）→`PostJSON(notify_url, nil, body, 30*time.Second)`→记 `order_notifications`（attempt=Count+1，body 截 500）→200 且 body 忽略大小写等 "success"→Transition 内置 notify_status=3+notified_at 并返回 nil；否则返回 error（asynq 重试）
- [ ] **Step 1–4: TDD 循环**（同前风格；sync 用例：channel 返回 2 实例→ReplaceAll 收到映射后的行；notify 用例：成功判定大小写混合 "Success" 通过、非 200 或 body 不符返回 error 且已留痕、notify_status=3 幂等直接 nil）。
- [ ] **Step 5: `make check` + Commit** `git commit -m "feat(payment): 查单/可用渠道/实例同步/商户通知发送"`

---

### Task 12: handler 三出口（paymentv1 / gatewayv1 / callback HTTP）

**Files:**
- Create: `internal/payment/handler/{grpc.go,gateway.go,callback.go,callback_test.go}`；替换脚手架 handler/doc.go 占位

**Interfaces:**
- Consumes: Task 2 pb、Task 5 sign.FieldsFromProto、Task 8–11 service 方法。
- Produces:
  - `handler.NewGRPC(svc *service.Payment) *GRPC`（实现 `paymentv1.PaymentServiceServer`）——每个 RPC：`fields, sig := sign.FieldsFromProto(req)` → `m, err := svc.Authenticate(ctx, fields, sig)` → 组 service 入参调用 → proto 出参。薄壳，无业务分支。
  - `handler.NewGateway(svc *service.Payment) *Gateway`（实现 `gatewayv1.GatewayServiceServer`）：`TripartiteUnfinishedOrders`→`svc` 侧新增薄方法 `UnfinishedOrders(ctx, channelName, merchantNo, currency string, periodMinutes int32) ([]model.PaymentOrder, error)`（route 查实例→Orders.ListUnfinished；本 Task 在 service 补这一个方法与用例）；`TripartiteOrderCallback`→落 `callbacks` 表(source=2) + `svc.ApplyChannelResult`，仅基础设施错误返回 error（转移表语义已在 service 内）。
  - `handler.NewCallback(svc *service.Payment) http.Handler` — 处理 `POST|GET /callbacks/payment/{instanceID}`：读 header（全量 map，多值取首个）、query 原串、body（`io.ReadAll` 上限 1MB）、IP（`X-Forwarded-For` 首跳，缺省 RemoteAddr）→ `svc.HandleChannelCallback` → 按 CallbackReply 写响应。路径不匹配→404。
- [ ] **Step 1: callback_test.go 失败测试**（`httptest` + mock 化的 service？service.Payment 是具体类型——handler 直接依赖它即可，callback_test 用 `httptest.NewRequest` 只测**路径解析与读取组装**：抽 `parseCallbackPath(path string) (int64, error)` 纯函数测之；组装后的行为已在 Task 10 覆盖）。
- [ ] **Step 2: 实现三文件** → **Step 3: `make check`**；channel 侧回调头约定：instance 快照 `callback_headers` 的抽取在 service（Task 10 已做），handler 只透传全量 header。
- [ ] **Step 4: Commit** `git commit -m "feat(payment): gRPC/网关/回调三出口"`

---

### Task 13: job 三件与通知 worker 接线

**Files:**
- Create: `internal/payment/job/{sync.go,order_sweep.go,notify_sweep.go}`；替换脚手架 job/doc.go 占位

**Interfaces:**
- Consumes: service 方法（job 在使用方声明小接口，对照 `internal/channel/job/reconcile.go` 的写法）。
- Produces（全部实现 `app.Component`，`Start` ticker 阻塞、`Stop` 关停；首轮立即执行）:
  - `job.NewSync(svc SyncService, interval time.Duration, logger *slog.Logger) *Sync` — `SyncService interface{ SyncInstances(ctx) error }`；失败仅 Warn，下轮再试
  - `job.NewOrderSweep(svc OrderSweepService, logger *slog.Logger) *OrderSweep` — 每 5 分钟：service 新增 `func (s *Payment) SweepStaleCreated(ctx context.Context) error`（ListStaleCreated(now-30m)→逐单 Transition created→failed＋notify_status（有 notify_url 则 1 并 enqueue，无则 4）；本 Task 在 service 补该方法与表驱动用例）
  - `job.NewNotifySweep(svc NotifySweepService, logger *slog.Logger) *NotifySweep` — 每 5 分钟：service 新增 `func (s *Payment) SweepStuckNotify(ctx context.Context) error`（ListNotifyStuck(now-10m, now-2h)→逐单 enqueueNotify；同样补用例）
  - 阈值常量（30m/10m/2h/5m 周期）定义在各 job/service 文件顶部 const 并注释语义
- [ ] **Step 1: service 两个 sweep 方法 TDD**（mock repo 驱动）→ **Step 2: 三个 job 组件实现**（ticker 骨架对照 channel 的 reconcile job）→ **Step 3: `make check`** → **Step 4: Commit** `git commit -m "feat(payment): 实例同步与滞留/通知兜底任务"`

---

### Task 14: 装配、配置、种子迁移、服务文档、终验

**Files:**
- Modify: `cmd/payment/initial/init_app.go`（替换模板注释态为真实装配）、`configs/payment.yaml`（放开 http_addr、补 pgsql/queue/channel/notify 段）、`internal/payment/config/config.go`
- Create: `migrations/payment/<时间戳>_seed_demo_merchant.sql`（本地演示商户，`make migrate-create SVC=payment NAME=seed_demo_merchant` 生成文件名）、`docs/payment/README.md`
- Modify: `AGENTS.md`（若「服务分层」或其他节有需同步的表述——检查后如无改动就不动）

**Interfaces:**
- Consumes: 全部前置任务。

- [ ] **Step 1: config.go**——对照 `internal/channel/config/config.go` 风格：

```go
type Config struct {
    Log       log.Config       `yaml:"log"`
    App       app.Config       `yaml:"app"`
    Telemetry telemetry.Config `yaml:"telemetry"`
    Transport transport.Config `yaml:"transport"`
    Pgsql     pgsql.Config     `yaml:"pgsql"`
    Queue     queue.Config     `yaml:"queue"`
    Channel   Channel          `yaml:"channel"`
    Notify    Notify           `yaml:"notify"`
}
type Channel struct {
    Addr         string        `yaml:"addr"`          // channel 服务 gRPC 地址，必填。
    SyncInterval time.Duration `yaml:"sync_interval"` // 实例同步周期，零值取 5m。
}
type Notify struct {
    CallbackBaseURL string `yaml:"callback_base_url"` // 三方回调外网基址，如 https://pay.example.com。
}
```

- [ ] **Step 2: configs/payment.yaml**——每段带注释（风格对照 channel.yaml）：transport grpc `:9093` + http `:8093`（此时放开）；pgsql write 本地 `postgres://app:app@127.0.0.1:5432/app?sslmode=disable`；queue addr `127.0.0.1:6379`（注释：专用实例、noeviction+AOF）；channel addr `127.0.0.1:9092`、sync_interval `5m`；notify callback_base_url `http://127.0.0.1:8093`（本地演示值）。
- [ ] **Step 3: init_app.go 装配**（对照 channel 的 initial 与 both 模板）：

```go
createInfra: telemetry → pgsql → queueClient（wrap 成 closer 组件，Stop 调 Close）
createServer:
  cc := channelclient.New(c.Channel.Addr)            // 失败 panic（装配期即死）
  svc := service.New(service.Config{CallbackBaseURL: c.Notify.CallbackBaseURL},
                     service.Deps{ …六仓储 repo.NewX(db.DB)…, Channel: cc, Queue: queueClient,
                                   HTTP: httpc.New(httpc.Config{TracerProvider: tel.TracerProvider()})}, logger)
  if err := svc.SyncInstances(ctx); err != nil { panic }   // 启动全量同步失败即死
  worker := queue.NewWorker(c.Queue, logger); worker.Handle(service.TaskNotify, svc.SendNotify)
  components = [ccCloser, worker, job.NewSync(svc, c.Channel.SyncInterval, logger),
                job.NewOrderSweep(svc, logger), job.NewNotifySweep(svc, logger)]
  srv := transport.NewServer(ctx, c.Transport,
      transport.WithTracerProvider(tel.TracerProvider()), transport.WithLogger(logger),
      transport.WithService(func(s *grpc.Server) {
          paymentv1.RegisterPaymentServiceServer(s, handler.NewGRPC(svc))
          gatewayv1.RegisterGatewayServiceServer(s, handler.NewGateway(svc))
      }),
      transport.WithGateway(paymentv1.RegisterPaymentServiceHandler),
      transport.WithMount("/docs", openapi.Handler()),
      transport.WithMount("/callbacks/", handler.NewCallback(svc)))
```

- [ ] **Step 4: 种子迁移**（仅本地演示，注释写明生产由运维入库、密钥不进仓库）：demo 商户一行（app_id `demo`、app_secret `demo-secret-0000`、限额 100–1000000、fee_rate 30）；绑定行不种（channel_instances 由同步生成，绑定需真实实例 id，README 给手工 INSERT 示例）。
- [ ] **Step 5: docs/payment/README.md**——对照 `docs/channel/README.md` 结构收敛为一页：业务定位、RPC 一览、错误码表、签名规范（含 canonical 示例一条）、状态机图、回调/补单/通知机制表、本地运行动线（migrate-up → 起 channel → 起 payment → 手工绑定 → 冒烟 curl 三连）。
- [ ] **Step 6: 终验**：`make check` 全绿；有本地 PG/Redis 时 `make migrate-up SVC=payment && make run SVC=payment` 冒烟（`/docs` 可读、`POST /v1/payment/orders` 走通鉴权链返回预期错误码），不可用则在完成报告如实说明未跑冒烟。
- [ ] **Step 7: Commit** `git commit -m "feat(payment): 装配接线、配置、种子迁移与服务文档"`

---

## Self-Review 记录

- **Spec 覆盖**：§2 形态→T1/T14；§3 契约与签名→T2/T5/T8；§4 六表→T3；§5 状态机→T10（转移表全用例）；§6 选路→T9；§7 回调→T10/T12；§8 补单→T12（UnfinishedOrders/OrderCallback）；§9 通知→T11/T13；§10 pkg/queue→T4；§11 装配→T14；§12 分层→各任务文件表；§13 不变量→T5/T8/T9/T10 用例显式覆盖；§14 测试策略→各 TDD 步骤。
- **类型一致性**：service 接口签名以 Task 6/7 Produces 为唯一来源；`OutStatus`、`TaskNotify`、`CallbackReply` 等跨任务符号已在各自 Produces 登记。
- **无占位**：全部步骤含具体代码/命令/预期。
