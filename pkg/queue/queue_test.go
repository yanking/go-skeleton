package queue

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/yanking/go-skeleton/pkg/app"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "addr 缺失", cfg: Config{}, wantErr: true},
		{name: "addr 非空", cfg: Config{Addr: "127.0.0.1:6379"}, wantErr: false},
		{name: "并发数为负", cfg: Config{Addr: "h:6379", Concurrency: -1}, wantErr: true},
		{name: "池大小为负", cfg: Config{Addr: "h:6379", PoolSize: -1}, wantErr: true},
		{name: "停机超时为负", cfg: Config{Addr: "h:6379", ShutdownTimeout: -1}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewClientPanicsOnInvalidConfig(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("want panic, got 正常返回")
		}
	}()
	NewClient(Config{})
}

func TestNewWorkerPanicsOnInvalidConfig(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("want panic, got 正常返回")
		}
	}()
	NewWorker(Config{}, nil)
}

func TestOptions(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want []asynq.Option
	}{
		{name: "无选项", opts: nil, want: nil},
		{
			name: "MaxRetry 映射",
			opts: []Option{MaxRetry(3)},
			want: []asynq.Option{asynq.MaxRetry(3)},
		},
		{
			name: "ProcessIn 映射",
			opts: []Option{ProcessIn(5 * time.Second)},
			want: []asynq.Option{asynq.ProcessIn(5 * time.Second)},
		},
		{
			name: "TaskID 映射",
			opts: []Option{TaskID("email:1")},
			want: []asynq.Option{asynq.TaskID("email:1")},
		},
		{
			name: "Timeout 映射",
			opts: []Option{Timeout(5 * time.Second)},
			want: []asynq.Option{asynq.Timeout(5 * time.Second)},
		},
		{
			name: "组合选项按传入顺序映射",
			opts: []Option{MaxRetry(1), ProcessIn(2 * time.Second)},
			want: []asynq.Option{asynq.MaxRetry(1), asynq.ProcessIn(2 * time.Second)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := options(tt.opts); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("options() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkerHandleDuplicateRegistrationPanics(t *testing.T) {
	// NewWorker/NewServer 惰性建连，构造过程不触网，无需真实 Redis。
	w := NewWorker(Config{Addr: "127.0.0.1:6379"}, nil)
	h := func(context.Context, []byte) error { return nil }

	w.Handle("email:send", h)

	defer func() {
		if r := recover(); r == nil {
			t.Error("对同一 typename 重复 Handle 应当 panic，实际未 panic")
		}
	}()
	w.Handle("email:send", h)
}

func TestWorkerName(t *testing.T) {
	w := NewWorker(Config{Addr: "127.0.0.1:6379"}, nil)
	if got := w.Name(); got != "queue-worker" {
		t.Errorf("Name() = %q, want %q", got, "queue-worker")
	}
}

// TestWorkerStopWithoutStartIsSafe 覆盖 app.Component 的契约：Stop 可能在组件
// 尚未真正运行时被调用（例如别的组件先失败触发全量停机），实现须容忍——不
// panic、不阻塞。目前靠 asynq.Server.Shutdown 对 srvStateNew 状态的隐性兜底
// （直接短路返回），本用例把这条隐性行为钉成回归网，升级 asynq 版本时若该
// 行为改变（比如 Shutdown 在未 Start 时也去等 in-flight 任务）能第一时间暴露。
func TestWorkerStopWithoutStartIsSafe(t *testing.T) {
	w := NewWorker(Config{Addr: "127.0.0.1:6379"}, nil)

	done := make(chan error, 1)
	go func() { done <- w.Stop(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop() 在未 Start 时调用 got %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() 在未 Start 时调用不应阻塞")
	}
}

// TestWorkerStopIsIdempotent 覆盖 app.Component 的另一条契约边界：多个组件同时
// 停机、或停机超时后放弃等待仍会让 Stop 被再次调用，Stop 必须幂等——同样靠
// asynq.Server.Shutdown 对 srvStateClosed 状态的短路兜底，本用例钉住这条行为。
func TestWorkerStopIsIdempotent(t *testing.T) {
	w := NewWorker(Config{Addr: "127.0.0.1:6379"}, nil)

	for i := 1; i <= 2; i++ {
		done := make(chan error, 1)
		go func() { done <- w.Stop(context.Background()) }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("第 %d 次 Stop() got %v, want nil", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次 Stop() 不应阻塞", i)
		}
	}
}

// TestClientIsComponent 钉住 Client 能直接交给 pkg/app 编排。
//
// 不实现组件接口的话，任何会入队的服务都得自己手搓一个包装器才能把它放进
// createInfra 的 []app.Component——或者干脆忘掉 Close，于是停机时 Redis 连接
// 不被优雅关闭、服务端留下半开连接。这正是「模式复制到第 10 个服务」时每次
// 都要重做一遍的地方。
func TestClientIsComponent(t *testing.T) {
	var _ app.Component = (*Client)(nil)

	c := NewClient(Config{Addr: "127.0.0.1:6379"})
	if got := c.Name(); got != "queue-client" {
		t.Errorf("Name got %q, want %q", got, "queue-client")
	}
	if err := c.Start(context.Background()); err != nil {
		t.Errorf("Start got %v, want nil", err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Errorf("Stop got %v, want nil", err)
	}
}

// TestRedisOptMapping 钉住连接参数的映射：Username 与 TLS 漏传就连不上托管 Redis
// （ElastiCache 的 in-transit encryption、Azure Cache 默认强制 TLS、Redis 6 ACL）。
func TestRedisOptMapping(t *testing.T) {
	opt := Config{
		Addr: "h:6379", Username: "app", Password: "pw", DB: 3,
		PoolSize: 7, TLS: true,
	}.redisOpt()

	if opt.Addr != "h:6379" || opt.Username != "app" || opt.Password != "pw" {
		t.Errorf("连接参数未透传: %+v", opt)
	}
	if opt.DB != 3 || opt.PoolSize != 7 {
		t.Errorf("DB/PoolSize 未透传: %+v", opt)
	}
	if opt.TLSConfig == nil {
		t.Error("配了 tls: true 应给出 TLSConfig")
	} else if opt.TLSConfig.InsecureSkipVerify {
		t.Error("不应跳过证书校验——那是最常见的降级误用")
	}

	if plain := (Config{Addr: "h:6379"}).redisOpt(); plain.TLSConfig != nil {
		t.Error("未配 tls 不应给 TLSConfig")
	}
}

// TestWorkerHealthy 钉住队列健康状态的流转。
//
// 没有这条通路时，Worker 起来之后 Redis 才不可用的情形完全没有信号：asynq 内部
// 重试并打日志，Start 早已返回、app 收不到致命错误，实例一直报「就绪」而任务在
// 堆积——纯异步服务（none 变体）尤其危险，进程里没有任何会退出的组件。
func TestWorkerHealthy(t *testing.T) {
	w := NewWorker(Config{Addr: "127.0.0.1:6379"}, nil)

	// 尚未跑过检查：不把实例判为不就绪，否则每次发布都要多等一个检查周期。
	if err := w.Healthy(); err != nil {
		t.Errorf("未跑过检查时应为健康, got %v", err)
	}

	down := errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")
	w.recordHealth(down)
	if err := w.Healthy(); !errors.Is(err, down) {
		t.Errorf("检查失败后应报出原因, got %v", err)
	}

	// 恢复后要能回到健康，否则一次抖动就把实例永久摘掉。
	w.recordHealth(nil)
	if err := w.Healthy(); err != nil {
		t.Errorf("恢复后应回到健康, got %v", err)
	}
}
