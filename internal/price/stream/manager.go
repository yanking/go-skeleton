package stream

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/yanking/go-skeleton/internal/price/exchange"
)

// managedConn 一条由 Manager 起管的连接：goroutine 内跑 conn.Run(ctx)，cancel
// 取消它，done 在其 goroutine 返回后关闭——供 Rebuild/Stop 等待其真正退出用。
type managedConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager 持有一组由同一个 Decoder/Handler/OnReady/Policy 驱动的 *Conn，
// 按订阅集变更整体重建连接；实现 app.Component，随服务启停。交易所之间的差异
// （不同 Decoder 等）不在本包内区分，由上层为每家交易所各构造一个 Manager 承担。
type Manager struct {
	dec    Decoder
	handle Handler
	ready  OnReady
	logger *slog.Logger
	policy Policy

	mu    sync.Mutex
	conns []managedConn
	// plans 是「当前 m.conns 确认在跑的连接计划」的规范化（排序）独立副本，
	// 仅在全部旧连接确认停干净、新连接确认起好之后才更新——不是「最近一次
	// 收到的 plans 参数」。任何时候只要不能保证这个不变量成立（如
	// stopAllLocked 超时，无法确认旧连接是否真的都退出了），就必须清空它，
	// 否则短路逻辑会拿一个不再可信的值去判断「变没变」。
	plans []exchange.ConnPlan
}

// NewManager 构造连接管理器。
func NewManager(dec Decoder, h Handler, ready OnReady, logger *slog.Logger, policy Policy) *Manager {
	return &Manager{dec: dec, handle: h, ready: ready, logger: logger, policy: policy}
}

// Name 实现 app.Component。
func (m *Manager) Name() string { return "price-stream" }

// Start 实现 app.Component：本组件的常驻工作由 Rebuild 起的连接 goroutine 各自
// 承担，Start 自身只需阻塞到 ctx 取消——真正的停止动作在 Stop 里做。
func (m *Manager) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop 实现 app.Component：停掉全部在管连接，最多等到 ctx 到期。app.Component
// 的约定是「Stop 须在 ctx 到期前返回」，且这个宽限期是全部组件共享的——
// 老老实实遵守，不然一个赖着不退出的连接会拖累后面组件的停机预算。
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopAllLocked(ctx)
}

// Rebuild 用新的连接计划整体替换现有连接：plans 规范化（排序，消掉「同一个
// 逻辑订阅集因为切片顺序不同」这类无害抖动，见 normalizePlans）后与上一次
// Rebuild 确认生效的结果完全相同时直接跳过——上层（如按 reload_interval 周期
// 调用的重载任务）不必自己 diff、更不必保证顺序稳定，判断责任放在持有当前
// plans 的这里。plans 真的变了才停掉全部旧连接，最多等到 ctx 到期；到期仍未
// 停完就放弃重建、原样保留旧连接并返回 error——调用方（重载任务）据此知道
// 这次重载没成功，下一个周期会用同样的 plans 再试一次。宁可暂时留着旧连接
// 不放，也不能在旧连接没退出前就起新连接：那样新旧两条连接会同时收同一个流，
// 事件重复入库；重建失败是可恢复的，持锁死等不是——Rebuild 全程持有 m.mu，
// 死等会连累 Stop 等其它调用一起卡死。
func (m *Manager) Rebuild(ctx context.Context, plans []exchange.ConnPlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	normalized := normalizePlans(plans)
	if reflect.DeepEqual(m.plans, normalized) {
		return nil
	}

	if err := m.stopAllLocked(ctx); err != nil {
		return fmt.Errorf("重建连接: %w", err)
	}

	// 起新连接仍按调用方给的原始顺序（不用 normalized）——normalizePlans 的
	// 排序只服务于比较与存储，不该反过来影响实际拨号/发订阅帧的顺序。
	conns := make([]managedConn, 0, len(plans))
	for _, plan := range plans {
		conns = append(conns, m.startLocked(plan))
	}
	m.conns = conns
	m.plans = normalized
	return nil
}

// startLocked 起一条新连接：conn.Run 在独立 goroutine 内跑，直到其 ctx 被取消
// （由 stopAllLocked 触发）才退出。调用方须持有 m.mu。
func (m *Manager) startLocked(plan exchange.ConnPlan) managedConn {
	ctx, cancel := context.WithCancel(context.Background())
	conn := NewConn(plan, m.dec, m.handle, m.ready, m.logger, m.policy)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Conn.Run 按其自身约定只在 ctx 取消时返回 nil；这里仍处理非 nil
		// 分支，是防御性写法，不是把断线错误吞掉——断线由 Run 内部自愈。
		if err := conn.Run(ctx); err != nil {
			m.logger.Error("ws 连接异常退出", "url", plan.URL, "err", err)
		}
	}()
	return managedConn{ctx: ctx, cancel: cancel, done: done}
}

// stopAllLocked 取消并等待全部现有连接的 goroutine 退出，最多等到 ctx 到期。
// 一旦决定停掉现有连接就立即清空 m.plans——不管接下来是等到全部退出（成功）
// 还是超时放弃（部分未确认退出），已发出的 cancel() 都不可撤销，m.plans
// 记录的「当前在跑什么」从这一刻起就不再可信。不清空的后果很具体：Rebuild
// 超时后 m.plans 停留在旧值，若订阅集之后又“抖回”这个旧值（标的短暂下架又
// 上架、Plan() 内部 map 分组顺序变化等），下一次 Rebuild 会被短路命中直接
// 跳过——Manager 自认为在跑这组 plans，实际一条连接都没有，采集永久静默
// 停摆，只留一条几分钟前的 error 日志。
//
// 到期后放弃等待并返回 error，且不清空 m.conns——已发出的 cancel() 不影响
// 连接的读循环在自己的下一次 I/O 边界感知 ctx 取消并退出，只是这里不再等它；
// 但调用方（Stop/Rebuild）不能假定这些连接已经停干净：m.conns 仍指向它们，
// 供下一次调用继续尝试其 done channel——若这里连 m.conns 也清空，Manager 会
// 彻底丢失这些连接的追踪，之后的 Stop 会谎报「停干净了」。只有全部确认退出
// （成功路径）才清空 m.conns。调用方须持有 m.mu。
func (m *Manager) stopAllLocked(ctx context.Context) error {
	m.plans = nil
	for _, c := range m.conns {
		c.cancel()
	}
	for _, c := range m.conns {
		select {
		case <-c.done:
		case <-ctx.Done():
			return fmt.Errorf("停止连接管理器: 宽限期耗尽，仍有连接未退出: %w", ctx.Err())
		}
	}
	m.conns = nil
	return nil
}

// normalizePlans 返回 plans 的规范化独立副本：外层按 URL 与内容拼成的 key
// 排序，每条 ConnPlan 内的 Subs 按 (Market, NativeSymbol, StreamType,
// Interval)、Subscribe 按字节序各自排序，ClientPing 也复制一份。
//
// 目的是让 Rebuild 的“变没变”判断与顺序无关：上游查询没写 ORDER BY，或者
// Exchange.Plan() 内部用 map 分组（Go 的 map 迭代顺序每次随机），都会让
// 同一个逻辑订阅集在相邻两次调用里产生顺序不同但内容相同的切片——按值比较
// 顺序敏感的话，短路逻辑会次次判定“变了”，退化回每次都全量断链重连。这里
// 只对比较/存储用的副本排序，不影响 Rebuild 实际起连接时使用的原始顺序。
//
// 规范化天然产出与入参完全独立的副本：调用方之后复用同一个底层切片（如
// 复用一个 scratch buffer）也不会在背后污染 m.plans 已经存下的值。
func normalizePlans(plans []exchange.ConnPlan) []exchange.ConnPlan {
	out := make([]exchange.ConnPlan, len(plans))
	for i, p := range plans {
		out[i] = p
		out[i].Subs = append([]exchange.Sub(nil), p.Subs...)
		sort.Slice(out[i].Subs, func(a, b int) bool {
			return subKey(out[i].Subs[a]) < subKey(out[i].Subs[b])
		})
		out[i].Subscribe = append([][]byte(nil), p.Subscribe...)
		sort.Slice(out[i].Subscribe, func(a, b int) bool {
			return bytes.Compare(out[i].Subscribe[a], out[i].Subscribe[b]) < 0
		})
		out[i].ClientPing = append([]byte(nil), p.ClientPing...)
	}
	sort.Slice(out, func(a, b int) bool {
		return planKey(out[a]) < planKey(out[b])
	})
	return out
}

// subKey 是 Sub 用于排序/比较的复合键，字段间用 NUL 分隔避免拼接歧义
// （如 Market="a", NativeSymbol="bc" 与 Market="ab", NativeSymbol="c" 不
// 会被拼成同一个字符串）。
func subKey(s exchange.Sub) string {
	return s.Market + "\x00" + s.NativeSymbol + "\x00" + s.StreamType + "\x00" + s.Interval
}

// planKey 是 ConnPlan 用于排序的复合键：调用方须先对 p.Subs 排好序（
// normalizePlans 保证）。仅用 URL 排序在「多条连接共享同一个拨号地址、订阅
// 信息全靠连接后的 Subscribe 帧区分」的交易所形态下会有大量并列，
// sort.Slice 对并列元素的相对顺序不保证稳定，故再叠加 Subs/Subscribe 的
// 内容作为次级键，确保只要两条 ConnPlan 内容有任何差异，key 就不同。
func planKey(p exchange.ConnPlan) string {
	var b strings.Builder
	b.WriteString(p.URL)
	for _, s := range p.Subs {
		b.WriteByte(0)
		b.WriteString(subKey(s))
	}
	for _, frame := range p.Subscribe {
		b.WriteByte(0)
		b.Write(frame)
	}
	return b.String()
}
