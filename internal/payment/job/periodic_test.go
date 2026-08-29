package job

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPeriodic_RunsFirstRoundImmediately(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	p := &periodic{name: "t", interval: time.Hour, logger: quietLogger(), run: func(context.Context) error {
		if calls.Add(1) == 1 {
			close(done)
		}
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("首轮未立即执行：等待 1s 仍未调用 run")
	}
}

func TestPeriodic_RepeatsOnInterval(t *testing.T) {
	var calls atomic.Int32
	third := make(chan struct{})
	p := &periodic{name: "t", interval: 5 * time.Millisecond, logger: quietLogger(), run: func(context.Context) error {
		if calls.Add(1) == 3 {
			close(third)
		}
		return nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()

	select {
	case <-third:
	case <-time.After(time.Second):
		t.Fatalf("未按周期重复执行：1s 内只调用了 %d 次", calls.Load())
	}
}

func TestPeriodic_StartReturnsOnContextCancel(t *testing.T) {
	p := &periodic{name: "t", interval: time.Hour, logger: quietLogger(), run: func(context.Context) error { return nil }}

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- p.Start(ctx) }()
	cancel()

	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("Start() error = %v, want nil(正常停机)", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 Start 未返回")
	}
}

func TestPeriodic_RoundErrorDoesNotStopLoop(t *testing.T) {
	var calls atomic.Int32
	second := make(chan struct{})
	p := &periodic{name: "t", interval: 5 * time.Millisecond, logger: quietLogger(), run: func(context.Context) error {
		if calls.Add(1) == 2 {
			close(second)
		}
		return errors.New("本轮失败")
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Start(ctx) }()

	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("单轮出错后循环停止了，want 下一轮继续")
	}
}

// stubSyncService 只为构造 NewSync，用例不触发循环，方法体不会被调用。
type stubSyncService struct{}

func (stubSyncService) SyncInstances(context.Context) error { return nil }

func TestNewSync_ZeroIntervalTakesDefault(t *testing.T) {
	c := NewSync(stubSyncService{}, 0, quietLogger())
	p, ok := c.(*periodic)
	if !ok {
		t.Fatalf("NewSync() 返回 %T, want *periodic", c)
	}
	if p.interval != defaultSyncInterval {
		t.Errorf("interval = %v, want 默认值 %v", p.interval, defaultSyncInterval)
	}
}

func TestNewSync_ConfiguredIntervalWins(t *testing.T) {
	c := NewSync(stubSyncService{}, 30*time.Second, quietLogger())
	if p := c.(*periodic); p.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", p.interval)
	}
}
