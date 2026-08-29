// Package service 是 channel 的业务层：维护「路由三元组 → 渠道实例」的内存路由表，
// 启动加载 + TTL 惰性重载（数据库加商户后至多一个 TTL 生效，无需重启）；
// 用例负责路由解析、渠道适配错误的 errcode 翻译。依赖倒置支点是 ChannelRepo 接口。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/yanking/go-skeleton/internal/channel/adapter"
	"github.com/yanking/go-skeleton/internal/channel/adapter/neokred"
	"github.com/yanking/go-skeleton/internal/channel/adapter/payapay"
	"github.com/yanking/go-skeleton/internal/channel/model"
	"github.com/yanking/go-skeleton/pkg/errcode"
	"github.com/yanking/go-skeleton/pkg/httpc"
	"google.golang.org/grpc/codes"
)

// channel 业务码（40000–49999 分段，登记于 AGENTS.md）。
var (
	ErrCodeChannelNotFound = errcode.New(40001, "渠道实例不存在", codes.NotFound)
	ErrCodeChannelRequest  = errcode.New(40002, "下游渠道请求失败", codes.Unavailable)
	ErrCodeVerifyFailed    = errcode.New(40003, "回调验签失败", codes.PermissionDenied)
	ErrCodeUnknownStatus   = errcode.New(40004, "回调状态未知", codes.InvalidArgument)
	ErrCodeBadResponse     = errcode.New(40005, "渠道响应解析失败", codes.Internal)
)

// refreshTTL 路由表惰性重载周期：DB 变更至多一个周期后对请求可见。
const refreshTTL = time.Minute

// ChannelRepo 渠道商户配置仓储接口，repo 包实现。
type ChannelRepo interface {
	LoadAll(ctx context.Context) ([]model.Channel, error)
}

// builders 渠道构造表：channel_name → 适配器构造函数，出站 HTTP 客户端在此闭包
// 注入，NewFunc 契约保持只收 platform 原文。
// 新渠道迁移完成后在此登记一行（放 service 而非 adapter 包，避免后者反向依赖实现包）。
func builders(hc *httpc.Client) map[string]adapter.NewFunc {
	return map[string]adapter.NewFunc{
		"payapay": func(platform json.RawMessage) (adapter.Adapter, error) { return payapay.New(hc, platform) },
		"neokred": func(platform json.RawMessage) (adapter.Adapter, error) { return neokred.New(hc, platform) },
	}
}

// instance 一个渠道商户实例的运行时形态：通用元数据 + 适配器 + 补单开关。
type instance struct {
	general          adapter.General
	impl             adapter.Adapter
	reconcileEnabled bool
}

// ChannelSvc 渠道用例集。
type ChannelSvc struct {
	repo    ChannelRepo
	builder map[string]adapter.NewFunc
	logger  *slog.Logger

	mu        sync.Mutex
	loadedAt  time.Time
	instances map[string]*instance // routeKey → instance
	list      []adapter.General    // 稳定排序的元数据快照
}

// New 构造用例并完成首次路由表加载；DB 不可达当场报错（装配期暴露，起不来就死）。
func New(ctx context.Context, repo ChannelRepo, hc *httpc.Client, logger *slog.Logger) (*ChannelSvc, error) {
	s := &ChannelSvc{
		repo:      repo,
		builder:   builders(hc),
		logger:    logger,
		instances: map[string]*instance{},
	}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// routeKey 路由三元组的 map 键。
func routeKey(r adapter.Route) string {
	return r.ChannelName + "|" + r.MerchantNo + "|" + r.Currency
}

// reload 全量重建路由表。单行构建失败（渠道未迁移、platform 配置非法）只跳过该行
// 并告警——增量迁移期数据库先行是常态，不能让一行脏数据拖死整个服务。
func (s *ChannelSvc) reload(ctx context.Context) error {
	rows, err := s.repo.LoadAll(ctx)
	if err != nil {
		return err
	}

	instances := make(map[string]*instance, len(rows))
	list := make([]adapter.General, 0, len(rows))
	for _, row := range rows {
		newFunc, ok := s.builder[row.ChannelName]
		if !ok {
			s.logger.Warn("渠道未迁移，跳过", "channel", row.ChannelName, "merchant", row.MerchantNo)
			continue
		}
		impl, err := newFunc(json.RawMessage(row.Platform))
		if err != nil {
			s.logger.Warn("渠道实例构建失败，跳过", "channel", row.ChannelName,
				"merchant", row.MerchantNo, "err", err)
			continue
		}

		general, err := toGeneral(row)
		if err != nil {
			s.logger.Warn("渠道元数据解析失败，跳过", "channel", row.ChannelName,
				"merchant", row.MerchantNo, "err", err)
			continue
		}
		instances[routeKey(general.Route)] = &instance{
			general:          general,
			impl:             impl,
			reconcileEnabled: row.ReconcileEnabled,
		}
		list = append(list, general)
	}

	s.mu.Lock()
	s.instances = instances
	s.list = list
	s.loadedAt = time.Now()
	s.mu.Unlock()
	return nil
}

// ensureFresh TTL 到期则重载；重载失败保留旧表继续服务（渠道配置表坏了不该放大为全站故障）。
func (s *ChannelSvc) ensureFresh(ctx context.Context) {
	s.mu.Lock()
	stale := time.Since(s.loadedAt) >= refreshTTL
	s.mu.Unlock()
	if !stale {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.loadedAt) < refreshTTL {
		return
	}
	if err := s.reload(ctx); err != nil {
		s.logger.Error("路由表重载失败，沿用旧表", "err", err)
	}
}

// toGeneral 表行转通用元数据；回调头与代付方式的 JSON 数组在此解开。
func toGeneral(row model.Channel) (adapter.General, error) {
	g := adapter.General{
		Route: adapter.Route{
			ChannelName: row.ChannelName,
			MerchantNo:  row.MerchantNo,
			Currency:    row.Currency,
		},
		ChannelLevel:           row.ChannelLevel,
		CallbackHeaders:        []string{},
		CallbackDataSource:     row.CallbackDataSource,
		CallbackReturn:         row.CallbackReturn,
		CallbackIPWhitelist:    row.CallbackIPWhitelist,
		PayoutSupports:         []int32{},
		LimitPaymentMin:        row.LimitPaymentMin,
		LimitPaymentMax:        row.LimitPaymentMax,
		LimitPayoutMin:         row.LimitPayoutMin,
		LimitPayoutMax:         row.LimitPayoutMax,
		PaymentCommissionRate:  row.PaymentCommissionRate,
		PaymentCommissionExtra: row.PaymentCommissionExtra,
		PayoutCommissionRate:   row.PayoutCommissionRate,
		PayoutCommissionExtra:  row.PayoutCommissionExtra,
	}
	if err := json.Unmarshal([]byte(row.CallbackHeaders), &g.CallbackHeaders); err != nil {
		return adapter.General{}, err
	}
	if err := json.Unmarshal([]byte(row.PayoutSupports), &g.PayoutSupports); err != nil {
		return adapter.General{}, err
	}
	return g, nil
}

// lookup 路由解析；三元组任一为空按参数错误处理，未命中按 40001。
func (s *ChannelSvc) lookup(ctx context.Context, route adapter.Route) (*instance, error) {
	if route.ChannelName == "" || route.MerchantNo == "" || route.Currency == "" {
		return nil, errcode.ErrInvalidParameter
	}
	s.ensureFresh(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	ins, ok := s.instances[routeKey(route)]
	if !ok {
		return nil, ErrCodeChannelNotFound
	}
	return ins, nil
}

// translate 渠道适配哨兵 → 业务 errcode，原始错误挂 cause 链只进日志。
func translate(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, adapter.ErrChannelRejected):
		return errcode.Wrap(err, ErrCodeChannelRequest)
	case errors.Is(err, adapter.ErrVerifyFailed):
		return errcode.Wrap(err, ErrCodeVerifyFailed)
	case errors.Is(err, adapter.ErrUnknownCallbackStatus):
		return errcode.Wrap(err, ErrCodeUnknownStatus)
	case errors.Is(err, adapter.ErrBadResponse):
		return errcode.Wrap(err, ErrCodeBadResponse)
	case errors.Is(err, adapter.ErrCallbackUnsupported):
		return errcode.Wrap(err, errcode.ErrInvalidParameter)
	default:
		return errcode.Wrap(err, errcode.ErrInternal)
	}
}

// ListChannels 全量渠道元数据（快照）。
func (s *ChannelSvc) ListChannels(ctx context.Context) []adapter.General {
	s.ensureFresh(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list
}

// ReconcileRoutes 开启补单对账的路由清单，job 逐个轮询。
func (s *ChannelSvc) ReconcileRoutes(ctx context.Context) []adapter.Route {
	s.ensureFresh(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	routes := make([]adapter.Route, 0, len(s.instances))
	for _, ins := range s.instances {
		if ins.reconcileEnabled {
			routes = append(routes, ins.general.Route)
		}
	}
	return routes
}

// PaymentOrder 代收下单。
func (s *ChannelSvc) PaymentOrder(ctx context.Context, route adapter.Route, in adapter.PaymentOrderIn) (adapter.PaymentOrderOut, error) {
	ins, err := s.lookup(ctx, route)
	if err != nil {
		return adapter.PaymentOrderOut{}, err
	}
	out, err := ins.impl.PaymentOrder(ctx, in)
	return out, translate(err)
}

// PayoutOrder 代付下单。
func (s *ChannelSvc) PayoutOrder(ctx context.Context, route adapter.Route, in adapter.PayoutOrderIn) (adapter.PayoutOrderOut, error) {
	ins, err := s.lookup(ctx, route)
	if err != nil {
		return adapter.PayoutOrderOut{}, err
	}
	out, err := ins.impl.PayoutOrder(ctx, in)
	return out, translate(err)
}

// PaymentQuery 代收查询。
func (s *ChannelSvc) PaymentQuery(ctx context.Context, route adapter.Route, in adapter.QueryIn) (adapter.QueryOut, error) {
	ins, err := s.lookup(ctx, route)
	if err != nil {
		return adapter.QueryOut{}, err
	}
	out, err := ins.impl.PaymentQuery(ctx, in)
	return out, translate(err)
}

// PayoutQuery 代付查询。
func (s *ChannelSvc) PayoutQuery(ctx context.Context, route adapter.Route, in adapter.QueryIn) (adapter.QueryOut, error) {
	ins, err := s.lookup(ctx, route)
	if err != nil {
		return adapter.QueryOut{}, err
	}
	out, err := ins.impl.PayoutQuery(ctx, in)
	return out, translate(err)
}

// PaymentCallback 代收回调验签。
func (s *ChannelSvc) PaymentCallback(ctx context.Context, route adapter.Route, header map[string]string, data string) (adapter.CallbackOut, error) {
	ins, err := s.lookup(ctx, route)
	if err != nil {
		return adapter.CallbackOut{}, err
	}
	out, err := ins.impl.PaymentCallback(ctx, header, data)
	return out, translate(err)
}

// PayoutCallback 代付回调验签。
func (s *ChannelSvc) PayoutCallback(ctx context.Context, route adapter.Route, header map[string]string, data string) (adapter.CallbackOut, error) {
	ins, err := s.lookup(ctx, route)
	if err != nil {
		return adapter.CallbackOut{}, err
	}
	out, err := ins.impl.PayoutCallback(ctx, header, data)
	return out, translate(err)
}

// BalanceQuery 商户余额查询。
func (s *ChannelSvc) BalanceQuery(ctx context.Context, route adapter.Route) (adapter.BalanceOut, error) {
	ins, err := s.lookup(ctx, route)
	if err != nil {
		return adapter.BalanceOut{}, err
	}
	out, err := ins.impl.BalanceQuery(ctx)
	return out, translate(err)
}
