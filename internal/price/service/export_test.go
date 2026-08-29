package service

// export_test.go 集中放测试专用的访问器与辅助函数：Go 的惯用做法是放在同包
// 的 _test.go 文件里，不挂在生产类型上（AGENTS.md 评审规则把生产代码里的
// 测试专用方法视为缺陷）。本包后续任务（Task 10/11）新增的测试文件复用这里的
// testLogger，不必各自重复定义。

import (
	"io"
	"log/slog"

	"github.com/yanking/go-skeleton/internal/price/exchange"
	"github.com/yanking/go-skeleton/internal/price/model"
)

// testLogger 构造一个丢弃全部输出的 logger，避免测试断言与生命周期日志混在
// 一起，同时满足 New 对 *slog.Logger 非 nil 的隐含要求。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// drainKlinesForTest 非阻塞地排空 kline 队列当前已缓冲的全部元素，按入队
// 顺序返回；用于验证攒批内容，或单纯放水解除 Route 因队列满而产生的阻塞。
func (s *Price) drainKlinesForTest() []model.Kline {
	var out []model.Kline
	for {
		select {
		case k := <-s.klineCh:
			out = append(out, k)
		default:
			return out
		}
	}
}

// drainSnapshotsForTest 非阻塞地排空 snapshot 队列当前已缓冲的全部元素，
// 按入队顺序（FIFO，从最旧到最新）返回，供测试观察 Route 的丢弃行为。
func (s *Price) drainSnapshotsForTest() []exchange.Snapshot {
	var out []exchange.Snapshot
	for {
		select {
		case item := <-s.snapCh:
			out = append(out, item.snapshot)
		default:
			return out
		}
	}
}
