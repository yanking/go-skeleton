package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	channelclient "github.com/yanking/go-skeleton/internal/payment/channel_client"
	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/repo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// callbackDataSourceQuery 表示回调验签数据取自 URL query（对应
// channel_instances.callback_data_source=2）；其余取值（1=body）走默认取 body 分支。
const callbackDataSourceQuery int32 = 2

// CallbackIn 三方回调的原文入参，由 handler（原生 HTTP 面）从请求解析后传入；
// IP 为经可信代理透传的来源地址。
type CallbackIn struct {
	InstanceID int64
	Headers    map[string]string
	Query      string
	RawBody    string
	IP         string
}

// CallbackReply 对三方的应答，由 handler 直接写回 HTTP：HTTPStatus 为状态码，
// Body 为渠道约定的应答串（实例快照里的 callback_return）。
type CallbackReply struct {
	HTTPStatus int
	Body       string
}

// HandleChannelCallback 处理三方回调（spec §7 七步），不返回 error——应答语义已内化到
// CallbackReply：正常处理与业务无效（验签失败/状态机标无效/订单不存在）均回 200 + 渠道应答串
// 以止发重试，IP 拒绝回 403，实例不存在回 404，我方基础设施错误回 500 等三方重发。
// 无论后续是否被拒，回调原文都先无条件落库（宁重不丢，攻击流量同样留痕可查）。
func (s *Payment) HandleChannelCallback(ctx context.Context, in CallbackIn) CallbackReply {
	// 步骤 1、2：原文无条件落库（status=已收到），落库失败即 500。
	cb := &model.Callback{
		ChannelInstanceID: in.InstanceID,
		Source:            model.CallbackSourceHTTP,
		Headers:           marshalHeaders(in.Headers),
		Query:             in.Query,
		Body:              in.RawBody,
		IP:                in.IP,
		Status:            model.CallbackStatusReceived,
	}
	if err := s.deps.Callbacks.Create(ctx, cb); err != nil {
		s.logger.Error("回调原文落库失败", "instance", in.InstanceID, "err", err)
		return CallbackReply{HTTPStatus: http.StatusInternalServerError}
	}

	// 步骤 3：查渠道实例；不存在回 404（回调行已留痕）。
	inst, err := s.deps.Instances.FindByID(ctx, in.InstanceID)
	if err != nil {
		if errors.Is(err, repo.ErrRowNotFound) {
			s.logger.Warn("回调指向的渠道实例不存在", "instance", in.InstanceID)
			return CallbackReply{HTTPStatus: http.StatusNotFound}
		}
		s.logger.Error("查询渠道实例失败", "instance", in.InstanceID, "err", err)
		return CallbackReply{HTTPStatus: http.StatusInternalServerError}
	}

	// 步骤 4：回调 IP 白名单精确匹配（空即不校验）；不符则标无效 + 告警 + 403。
	if !callbackIPAllowed(inst.CallbackIPWhitelist, in.IP) {
		s.logger.Warn("回调来源 IP 不在白名单", "instance", in.InstanceID, "ip", in.IP)
		s.markCallback(ctx, cb.ID, model.CallbackStatusInvalid, "", "IP 不在回调白名单")
		return CallbackReply{HTTPStatus: http.StatusForbidden}
	}

	// 步骤 5：按实例快照抽取指定头、按 data_source 取 body/query 为 data。
	header, data := extractCallbackData(inst, in)

	// 步骤 6：调 channel 验签并映射。
	out, err := s.deps.Channel.VerifyCallback(ctx, channelclient.Route{
		ChannelName: inst.ChannelName,
		MerchantNo:  inst.MerchantNo,
		Currency:    inst.Currency,
	}, header, data)
	if err != nil {
		// channel 业务错（验签失败/参数错/未识别状态）→ 标无效 + 200 止发；
		// 其余（下游不可用等基础设施错）→ 500 等三方重发。
		if isChannelBusinessError(err) {
			s.logger.Warn("回调验签业务失败", "instance", in.InstanceID, "err", err)
			s.markCallback(ctx, cb.ID, model.CallbackStatusInvalid, "", "回调验签失败")
			return CallbackReply{HTTPStatus: http.StatusOK, Body: inst.CallbackReturn}
		}
		s.logger.Error("回调验签基础设施错误", "instance", in.InstanceID, "err", err)
		return CallbackReply{HTTPStatus: http.StatusInternalServerError}
	}

	// 步骤 7：验签结果进状态机（§5）。
	converged, err := s.ApplyChannelResult(ctx, ChannelResult{
		InstanceID:   in.InstanceID,
		OrderNo:      out.OrderNo,
		OutOrderNo:   out.OutOrderNo,
		CallbackType: out.CallbackType,
		Amount:       out.Amount,
		ReferenceNo:  out.ReferenceNo,
	})
	if err != nil {
		// 验签通过但订单不存在：重发也无法收敛，标无效 + 200 止发（异常已留痕）；
		// 其余基础设施错误 → 500 等三方重发。
		if errors.Is(err, repo.ErrRowNotFound) {
			s.logger.Error("回调验签通过但订单不存在", "instance", in.InstanceID, "order", out.OrderNo, "out_order_no", out.OutOrderNo)
			s.markCallback(ctx, cb.ID, model.CallbackStatusInvalid, out.OrderNo, "订单不存在")
			return CallbackReply{HTTPStatus: http.StatusOK, Body: inst.CallbackReturn}
		}
		s.logger.Error("回调进入状态机失败", "instance", in.InstanceID, "order", out.OrderNo, "err", err)
		return CallbackReply{HTTPStatus: http.StatusInternalServerError}
	}

	if !converged {
		// 金额不符/矛盾态：ApplyChannelResult 已告警留人工，此处标无效并 200 止发。
		s.markCallback(ctx, cb.ID, model.CallbackStatusInvalid, out.OrderNo, "状态机不可收敛，留人工")
		return CallbackReply{HTTPStatus: http.StatusOK, Body: inst.CallbackReturn}
	}

	s.markCallback(ctx, cb.ID, model.CallbackStatusVerified, out.OrderNo, "")
	return CallbackReply{HTTPStatus: http.StatusOK, Body: inst.CallbackReturn}
}

// markCallback 回填回调处理结果；失败只记日志——对三方的应答已定，原文亦已落库，
// 留痕失败可事后修复，不改变应答姿态。
func (s *Payment) markCallback(ctx context.Context, id int64, status int32, orderNo, note string) {
	if err := s.deps.Callbacks.Mark(ctx, id, status, orderNo, note); err != nil {
		s.logger.Error("回填回调处理结果失败", "callback", id, "status", status, "err", err)
	}
}

// marshalHeaders 把回调请求头序列化为 JSON 落库；map[string]string 不会序列化失败，
// 极端情况下兜底为空对象，不阻断留痕。
func marshalHeaders(h map[string]string) string {
	b, err := json.Marshal(h)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// extractCallbackData 按实例快照抽取验签所需数据：仅保留 callback_headers 指定的头，
// 按 callback_data_source 取 body 或 query 为 data。callback_headers 解析失败按空头处理
// （配置应由同步侧保证为合法 JSON 数组，异常时验签自会失败并留痕）。
func extractCallbackData(inst *model.ChannelInstance, in CallbackIn) (map[string]string, string) {
	header := make(map[string]string)
	if inst.CallbackHeaders != "" {
		var names []string
		if err := json.Unmarshal([]byte(inst.CallbackHeaders), &names); err == nil {
			for _, name := range names {
				if v, ok := in.Headers[name]; ok {
					header[name] = v
				}
			}
		}
	}

	data := in.RawBody
	if inst.CallbackDataSource == callbackDataSourceQuery {
		data = in.Query
	}
	return header, data
}

// callbackIPAllowed 校验回调来源 IP 是否在实例白名单内。白名单为逗号分隔的 IP 列表
// （对齐 channel 侧 callback_ip_whitelist 存储格式，与商户的 JSON 数组白名单不同），
// 逐项精确匹配（禁子串匹配，避免 "1.2.3.4" 误放行 "1.2.3.41"）；空白名单视为不限制来源。
func callbackIPAllowed(rawWhitelist, ip string) bool {
	rawWhitelist = strings.TrimSpace(rawWhitelist)
	if rawWhitelist == "" {
		return true
	}
	for _, w := range strings.Split(rawWhitelist, ",") {
		if strings.TrimSpace(w) == ip {
			return true
		}
	}
	return false
}

// isChannelBusinessError 判定 channel 返回的错误是业务无效还是基础设施错误：
// PermissionDenied（验签失败）/ InvalidArgument（报文非法）/ NotFound（未识别）视为业务无效，
// 对三方应 200 止发；其余（Unavailable 等）视为基础设施错误，应 500 等三方重发。
func isChannelBusinessError(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.InvalidArgument, codes.NotFound:
		return true
	default:
		return false
	}
}
