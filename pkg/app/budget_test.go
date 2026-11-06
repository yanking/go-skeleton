package app

// 停机预算分配的内部测试。放在包内是因为 budgetFor 不导出，而它的分配策略正是
// 「组件一多就互相挤压」这类问题的根源，值得直接验算而不是只靠端到端观察。

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// quietLogger 内部测试自己的日志出口：app_test.go 的 discardLogger 在外部测试包
// （package app_test）里，两边不共享。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBudgetReservesForLaterComponents 钉住保留制：最先停的组件拿到几乎全部预算，
// 排在后面的各有保底。
//
// 这条用例对着一个具体的事故：均分时六组件、25s 预算下，最先停的传输组件只拿到
// 4.2s——连摘流（5s）都装不下，于是在途请求被 grpc.Stop() 当场切断，而每次停机
// 都以「宽限期耗尽」收场。而各组件的时间需求本来就不均：传输要等在途请求做完
// （秒级），DB、Redis 的 Stop 是关个池（毫秒级）。
func TestBudgetReservesForLaterComponents(t *testing.T) {
	const (
		total      = 25 * time.Second
		components = 6 // 遥测 + DB + Redis + 队列客户端 + 队列 worker + 传输
	)
	a := New(Config{Logger: quietLogger(), StopTimeout: total})

	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()

	// 第一个停的组件（remaining == components）。
	first := a.budgetFor(ctx, components)
	reserved := time.Duration(components-1) * minStopBudget
	if first < total-reserved-time.Second {
		t.Errorf("最先停的组件应拿到几乎全部预算，got %v（总额 %v，为后面保留 %v）",
			first, total, reserved)
	}
	// 均分会给它 total/6 ≈ 4.17s；这条断言专门拦住那种分法回归。
	if first < total/2 {
		t.Errorf("最先停的组件拿到 %v，不足总额一半——这是均分的症状，"+
			"摘流加排空会装不下", first)
	}

	// 最后一个组件（remaining == 1）：没有要保留的，拿到全部剩余。
	if last := a.budgetFor(ctx, 1); last < total-time.Second {
		t.Errorf("最后一个组件应拿到全部剩余，got %v", last)
	}
}

// TestBudgetFloorForLateComponents 钉住保底：预算快耗尽时，排在后面的组件仍拿到
// minStopBudget，而不是一个已过期的 ctx。
//
// 原先全部组件共享预算、先到先吃：传输层被慢 handler 挂住就把预算吃光，DB、Redis
// 拿到已过期的 ctx 立刻返回「超时」——而它们的 Stop 其实只需要几毫秒。于是关库与
// 仍在执行的 handler 重叠，handler 的下一条 SQL 撞上 sql: database is closed。
func TestBudgetFloorForLateComponents(t *testing.T) {
	a := New(Config{Logger: quietLogger(), StopTimeout: time.Second})

	// 预算已耗尽的 ctx。
	expired, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()

	for _, remaining := range []int{1, 3, 6} {
		if got := a.budgetFor(expired, remaining); got != minStopBudget {
			t.Errorf("remaining=%d 时应给保底 %v，got %v", remaining, minStopBudget, got)
		}
	}
}

// TestBudgetWithoutDeadline 没有 deadline 的 ctx（理论上不会出现，Run 总会设）
// 退回整个 StopTimeout，而不是 0。
func TestBudgetWithoutDeadline(t *testing.T) {
	a := New(Config{Logger: quietLogger(), StopTimeout: 3 * time.Second})
	if got := a.budgetFor(context.Background(), 5); got != 3*time.Second {
		t.Errorf("无 deadline 时应退回 StopTimeout，got %v", got)
	}
}
