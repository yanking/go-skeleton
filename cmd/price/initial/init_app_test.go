package initial

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yanking/go-skeleton/internal/price/config"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"github.com/yanking/go-skeleton/pkg/pgsql"
	"github.com/yanking/go-skeleton/pkg/redis"
	"github.com/yanking/go-skeleton/pkg/telemetry"
)

// testServerConfig 构造一份可安全传给 createServer 的最小配置：pgsql/redis
// 用语法合法但从不会真的连接的地址——两个包的 New 都是惰性建连，装配期不
// ping（见各自包注释），createServer 本身现在也是纯构造（不做任何 I/O，
// 见其类型注释），所以这里不需要真实网络/DB 就能测组件顺序与日志行为。
// exchanges 为 nil 时等价于 yaml 里完全没写 exchanges 段。
func testServerConfig(exchanges map[string]config.Exchange) config.Config {
	return config.Config{
		Pgsql:     pgsql.Config{Write: "postgres://u:p@127.0.0.1:1/db?sslmode=disable"},
		Redis:     redis.Config{Addrs: []string{"127.0.0.1:1"}},
		Exchanges: exchanges,
	}
}

// testInfra 按 c 构造 createServer 所需的 tel/db/rdb 三个句柄，全部惰性
// 建连、不做真实网络 I/O。
func testInfra(c config.Config) (*telemetry.Telemetry, *pgsql.DB, *redis.DB) {
	tel := telemetry.New(context.Background(), telemetry.Config{Service: "price-test"})
	db := pgsql.New(c.Pgsql)
	rdb := redis.New(c.Redis)
	return tel, db, rdb
}

// fakeWriterRunner 是 writerRunner 的测试替身：记录 RunWriters 实际收到的
// ctx，可选地在 ctx 取消后再卡一会儿（release）或完全无视 ctx 永久阻塞
// （block）——分别用来验证 writerComponent.Stop 的「等真正返回」与「超时
// 放弃」两条路径。
type fakeWriterRunner struct {
	started  chan struct{}
	release  chan struct{}
	block    bool
	startCtx context.Context
	err      error
}

func (f *fakeWriterRunner) RunWriters(ctx context.Context) error {
	f.startCtx = ctx
	close(f.started)
	if f.block {
		select {} // 故意永不返回，模拟 RunWriters 卡死
	}
	<-ctx.Done()
	if f.release != nil {
		<-f.release
	}
	return f.err
}

// TestWriterComponent_StartUsesInternalCtxDecoupledFromAppCtx 锚定
// writerComponent 类型注释里的核心约束：Start 传给 RunWriters 的 ctx 必须是
// 组件内部独立持有的，不能是 pkg/app 传给 Start 的根 ctx 本身——即便调用方
// 传入的 ctx 已经取消（模拟停机信号已打到、全部组件的 Start 几乎同时收到
// 通知），RunWriters 收到的 ctx 也不该立即变为已取消，只有本组件的 Stop
// 被调用才应该取消它。
func TestWriterComponent_StartUsesInternalCtxDecoupledFromAppCtx(t *testing.T) {
	fake := &fakeWriterRunner{started: make(chan struct{})}
	w := newWriterComponent(fake)

	appCtx, cancelApp := context.WithCancel(context.Background())
	cancelApp() // 外部（app 根）ctx 预先取消

	done := make(chan error, 1)
	go func() { done <- w.Start(appCtx) }()

	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("RunWriters 未被调用")
	}
	if fake.startCtx.Err() != nil {
		t.Fatal("RunWriters 收到的 ctx 在外部 ctx 已取消时就已经是取消状态——" +
			"必须是内部独立 ctx，只由 Stop 取消，否则写协程会与 ws Manager 同时" +
			"停止消费 klineCh，见 writerComponent 类型注释")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	if err := w.Stop(stopCtx); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Start 返回值 = %v, want nil", err)
	}
}

// TestWriterComponent_StopWaitsForRunWritersToReturn 锚定 Stop 必须等
// RunWriters 真正返回才能返回，不能在取消内部 ctx 后就提前认为“已停止”——
// 否则装配层无从保证「RunWriters 停止消费 klineCh」与「全部 ws 读循环已退出」
// 之间的先后关系（那部分由注册顺序保证，这里只保证 Stop 本身语义正确）。
func TestWriterComponent_StopWaitsForRunWritersToReturn(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeWriterRunner{started: make(chan struct{}), release: release}
	w := newWriterComponent(fake)

	go w.Start(context.Background())
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("RunWriters 未被调用")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- w.Stop(context.Background()) }()

	select {
	case <-stopDone:
		t.Fatal("RunWriters 尚未真正返回（release 未放行），Stop 不该提前返回")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop 返回值 = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("release 放行后 Stop 应及时返回，未在预期时间内返回")
	}
}

// TestWriterComponent_StopReturnsErrorWhenRunWritersNeverReturns 锚定
// Stop 的宽限期语义：RunWriters 完全无视 ctx 取消、永不返回时，Stop 必须在
// 调用方给的 ctx 到期后报错返回，而不是死等——这个 ctx 是 pkg/app 全部组件
// 共享的停机预算，Stop 死等会拖累按逆序排在后面的组件挤不出停机时间。
func TestWriterComponent_StopReturnsErrorWhenRunWritersNeverReturns(t *testing.T) {
	fake := &fakeWriterRunner{started: make(chan struct{}), block: true}
	w := newWriterComponent(fake)

	go w.Start(context.Background())
	select {
	case <-fake.started:
	case <-time.After(time.Second):
		t.Fatal("RunWriters 未被调用")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := w.Stop(stopCtx); err == nil {
		t.Fatal("RunWriters 永不返回时 Stop 应在 ctx 到期后报错返回，got nil")
	}
}

// TestWriterComponent_Name 锚定组件名，仅用于日志与错误标注，回归时若手误
// 改动应能被测试抓到。
func TestWriterComponent_Name(t *testing.T) {
	w := newWriterComponent(&fakeWriterRunner{started: make(chan struct{})})
	if got := w.Name(); got != "price-writer" {
		t.Errorf("Name() = %q, want %q", got, "price-writer")
	}
}

// TestBuildExchange_UnsupportedNameReturnsError 锚定 buildExchange 对不认识
// 的交易所名的处理：返回 error 而不是 panic——调用方（daemon 视为装配期错误
// 自行 panic，子命令把 error 原样交还给 CLI 调用方）需要自己决定怎么处理
// 这种失败，本函数不该替调用方下这个判断。
func TestBuildExchange_UnsupportedNameReturnsError(t *testing.T) {
	hc := httpc.New(httpc.Config{})
	if _, err := buildExchange("kraken", config.Exchange{}, hc); err == nil {
		t.Fatal("不支持的交易所名应返回 error，got nil")
	}
}

// TestBuildExchange_KnownNames 锚定 name 到具体 adapter 的映射没有接反——
// binance 必须构造出 Name()=="binance" 的实现，okx 同理。
func TestBuildExchange_KnownNames(t *testing.T) {
	hc := httpc.New(httpc.Config{})
	cfg := config.Exchange{WSURL: "wss://example.invalid", RESTURL: "https://example.invalid"}

	for _, name := range []string{"binance", "okx"} {
		impl, err := buildExchange(name, cfg, hc)
		if err != nil {
			t.Fatalf("buildExchange(%q) 失败: %v", name, err)
		}
		if impl == nil {
			t.Fatalf("buildExchange(%q) 返回 nil 实现", name)
		}
		if got := impl.Name(); got != name {
			t.Errorf("buildExchange(%q).Name() = %q, want %q（name 到 adapter 的映射接反了）", name, got, name)
		}
	}
}

// TestWriterComponent_Stop_ToleratesNotYetStarted 锚定 pkg/app.Component 的
// 约定：Stop 可能在组件尚未真正运行时被调用，实现须容忍，不 panic、不阻塞。
// 与 job.reload 的同名测试同一个理由：Stop 若无条件等 done，Start 从未被
// 调用过时用 context.Background() 调用本方法会永久阻塞。
func TestWriterComponent_Stop_ToleratesNotYetStarted(t *testing.T) {
	w := newWriterComponent(&fakeWriterRunner{started: make(chan struct{})})

	done := make(chan error, 1)
	go func() { done <- w.Stop(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Stop() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start 从未被调用时 Stop 不该阻塞，1s 内应返回")
	}
}

// TestCreateServer_WriterRegisteredBeforeAllStreamManagers 是评审 R33 要求的
// 核心不变量测试：writer 组件必须先于全部 stream.Manager 注册（细则见
// createServer 里 writer append 处的大段注释），否则停机时会有 goroutine
// 永久卡在 klineCh 上。createServer 现在是纯构造（不做网络/DB I/O，见其
// 类型注释），因此这里不需要真实 Postgres/Redis 就能验证组件顺序这个纯粹
// 静态的属性。
func TestCreateServer_WriterRegisteredBeforeAllStreamManagers(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := testServerConfig(map[string]config.Exchange{
		"binance": {Enabled: true, WSURL: "wss://example.invalid", RESTURL: "https://example.invalid"},
		"okx":     {Enabled: true, WSURL: "wss://example.invalid", RESTURL: "https://example.invalid"},
	})
	tel, db, rdb := testInfra(c)

	components, _, managers := createServer(c, logger, tel, db, rdb)

	if len(managers) != 2 {
		t.Fatalf("managers 数量 = %d, want 2（binance、okx 都已启用）", len(managers))
	}

	writerIdx, firstManagerIdx := -1, -1
	for i, comp := range components {
		switch comp.Name() {
		case "price-writer":
			if writerIdx == -1 {
				writerIdx = i
			}
		case "price-stream": // stream.Manager.Name() 对全部交易所返回同一个常量
			if firstManagerIdx == -1 {
				firstManagerIdx = i
			}
		}
	}

	if writerIdx == -1 {
		t.Fatal("未找到 writer 组件（Name()==\"price-writer\"）")
	}
	if firstManagerIdx == -1 {
		t.Fatal("未找到任何 stream.Manager 组件（Name()==\"price-stream\"）")
	}
	if writerIdx >= firstManagerIdx {
		t.Fatalf("writer 必须先于全部 stream.Manager 注册：writer 在下标 %d，"+
			"最早的 Manager 在下标 %d——顺序反了会导致停机时 Manager.Stop 仍在等待的"+
			"读循环永久卡在往 klineCh 发送（细则见 createServer 注释）", writerIdx, firstManagerIdx)
	}
}

// TestCreateServer_LogsWhenExchangeDisabled 锚定评审 Important 2：
// enabled: false（含漏写 enabled 字段的零值）跳过某交易所时必须留一条日志，
// 不能让进程照常起来、一条连接都不建，日志里却找不到任何线索。
func TestCreateServer_LogsWhenExchangeDisabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	c := testServerConfig(map[string]config.Exchange{
		"binance": {Enabled: false, WSURL: "wss://example.invalid", RESTURL: "https://example.invalid"},
	})
	tel, db, rdb := testInfra(c)

	_, _, managers := createServer(c, logger, tel, db, rdb)

	if len(managers) != 0 {
		t.Fatalf("唯一的交易所已关停，managers 数量 = %d, want 0", len(managers))
	}
	logOutput := buf.String()
	if !strings.Contains(logOutput, "交易所已关停，跳过装配") {
		t.Errorf("跳过关停交易所时应记录日志，日志内容 = %q", logOutput)
	}
	if !strings.Contains(logOutput, "没有任何已启用的交易所") {
		t.Errorf("managers 为空时应有醒目的 Warn 日志，日志内容 = %q", logOutput)
	}
}
