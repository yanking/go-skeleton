package service

import (
	"context"
	"encoding/json"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

// SyncInstances 从 channel 服务拉取全量渠道商户实例元数据、映射为本地模型后整体覆盖
// （ChannelClient.ListInstances → InstanceRepo.ReplaceAll），由定时 job 周期调用。
func (s *Payment) SyncInstances(ctx context.Context) error {
	instances, err := s.deps.Channel.ListInstances(ctx)
	if err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}

	rows := make([]model.ChannelInstance, 0, len(instances))
	for _, inst := range instances {
		rows = append(rows, model.ChannelInstance{
			ChannelName:         inst.ChannelName,
			MerchantNo:          inst.MerchantNo,
			Currency:            inst.Currency,
			LimitPaymentMin:     inst.LimitPaymentMin,
			LimitPaymentMax:     inst.LimitPaymentMax,
			CallbackHeaders:     marshalCallbackHeaders(inst.CallbackHeaders),
			CallbackDataSource:  inst.CallbackDataSource,
			CallbackReturn:      inst.CallbackReturn,
			CallbackIPWhitelist: inst.CallbackIPWhitelist,
		})
	}

	if err := s.deps.Instances.ReplaceAll(ctx, rows); err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}
	return nil
}

// marshalCallbackHeaders 把 channel 服务下发的回调请求头名单序列化为 JSON 数组文本，
// 落库到 model.ChannelInstance.CallbackHeaders；nil 切片单独判空返回 "[]"（json.Marshal
// 对 nil 切片会输出 "null"，与「JSON 数组原文」的字段约定不符）；[]string 本身不会
// 序列化失败，极端情况下兜底为空数组，不阻断同步（同 callback.go 的 marshalHeaders）。
func marshalCallbackHeaders(h []string) string {
	if len(h) == 0 {
		return "[]"
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "[]"
	}
	return string(b)
}
