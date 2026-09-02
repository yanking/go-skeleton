package queue

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/hibiken/asynq"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "addr 缺失", cfg: Config{}, wantErr: true},
		{name: "addr 非空", cfg: Config{Addr: "127.0.0.1:6379"}, wantErr: false},
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
