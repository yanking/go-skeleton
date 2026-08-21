# Go 编码规范

> 基线是 Effective Go 与 Google Go Style Guide，本文件只写本项目的收紧与额外约定，基线内容不重复。
>
> **适用范围**：仅约束手写代码。`gen/**` 与 `openapi/**` 的生成物不受本规范约束（`init()` 注册等是生成器行为，且受宪法第一条「禁手改」保护）；`.golangci.yml` 已相应排除这两个目录。生成的 Go 注释来自 `.proto`，按文档优先写、不以符号名开头（见 architecture.md「协议层规则」），这不违反本文件的注释约定——本文件不管生成物。

## 硬性约定

- 注释一律中文；导出符号必须有注释，且以符号名开头：`// UserService 负责……`。
- gofmt + goimports 无 diff 才能提交（`make lint` 强制）。
- 禁止用 `panic` 做业务控制流；panic 只允许出现在 main 装配期「起不来就死」的场景，运行期由 recovery 拦截器兜底。
- 禁止包级可变状态（`init()` 注册、全局单例）；一切依赖显式经构造函数传入。
  - **唯一例外（OTel 全局 provider）**：`otel.SetTracerProvider` / `SetMeterProvider` / `SetTextMapPropagator` 只允许由 `pkg/telemetry` 在构造时设置一次，其他任何位置（含 cmd、`internal/**`、`pkg/` 其他包）禁止调用 `otel.Set*`。理由：otelsql、otelredis 等第三方埋点库以及库内部的 `otel.Tracer(...)` 只能经全局拿到 provider，不设全局它们会**静默**退化为 noop——不报错、不告警，只是 trace 里少了一整段。责任集中在一处，使用方忘不掉，也便于审计。我们自己的埋点仍须显式注入（`otelgrpc.WithTracerProvider(tel.TracerProvider())` 等），不依赖全局。

## 命名

- 包名：小写单个词、单数、无下划线；不用 util / common / base 这类无信息名。
- 错误哨兵 `ErrXxx`；自定义错误类型 `XxxError`。
- 惯用缩写保持一致：`ctx`、`req`、`resp`、`tx`。

## 错误处理

- 传播用 `fmt.Errorf("动作语境: %w", err)`；语境写「在做什么」，不写「出错了」这类废话。
- 业务错误在 biz 层定义（哨兵或带码类型）；service 层集中映射为 grpc status，错误只在这一处翻译，不散落各层。
- 判定用 `errors.Is / errors.As`，禁止字符串匹配错误内容。
- 忽略错误必须显式 `_ = f()` 并注释为何可忽略。

## context

- `ctx context.Context` 恒为第一参数，不存进 struct。
- 库代码不自造 `context.Background()`；后台任务的根 ctx 由 cmd 层统一给出。

## 并发

- 优先 `errgroup` / `sync.WaitGroup`，不写裸 `go func()`；每个 goroutine 有明确退出条件。
- channel 谁写谁关；跨函数传 channel 时注释所有权。
- 共享可变数据先想「能不能不共享」，再考虑加锁。

## 测试

- 表驱动 + `t.Run` 子测试；对照变量命名 `got` / `want`。
- 断言用标准库 + `google/go-cmp`，不引断言框架。
- 白盒测试与被测代码同包；只测导出行为时用 `_test` 包黑盒，二选一想清楚。
- biz 层测试不碰真实存储：用 data 接口的手写 fake，不引 mock 生成框架。
