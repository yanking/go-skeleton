package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yanking/go-skeleton/internal/payment/model"
	"github.com/yanking/go-skeleton/internal/payment/sign"
	"github.com/yanking/go-skeleton/pkg/errcode"
)

// notifyTimeout 商户通知单次 HTTP 请求的超时时间。
const notifyTimeout = 30 * time.Second

// notifyResponseBodyLimit order_notifications.response_body 落库截断长度（字节），
// 避免异常大响应体撑爆存储。
const notifyResponseBodyLimit = 500

// SendNotify 是 TaskNotify 任务的 asynq 处理函数：解析 payload 取订单号→取单（非待通知
// 状态直接幂等返回）→取商户→组装签名通知体→POST 给商户 notify_url→无论成败均落一条
// order_notifications 留痕→成功（HTTP 200 且 body 忽略大小写与首尾空白等于 "success"）
// 则把订单 notify_status 置为已通知，否则返回 error 交 asynq 按内置退避策略重试
// （见 pkg/queue 包注释「错误语义」）。
func (s *Payment) SendNotify(ctx context.Context, payload []byte) error {
	var in struct {
		OrderNo string `json:"order_no"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}

	o, err := s.deps.Orders.FindByOrderNo(ctx, in.OrderNo)
	if err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}
	if o.NotifyStatus != model.NotifyStatusPending {
		return nil // 已处理（成功/跳过）或非待通知状态，幂等直接返回，不重复发送
	}

	merchant, err := s.deps.Merchants.FindByID(ctx, o.MerchantID)
	if err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}

	body := notifyBody(o, merchant.AppSecret)
	code, respBody, sendErr := s.deps.HTTP.PostJSON(ctx, o.NotifyURL, nil, body, notifyTimeout)

	// 无论 HTTP 调用成败都先留痕：网络错误时 code=0、respBody 为空，同样是一次有意义的尝试记录。
	if err := s.recordNotifyAttempt(ctx, o.OrderNo, code, respBody); err != nil {
		return err
	}

	if sendErr != nil {
		return errcode.Wrap(sendErr, errcode.ErrInternal)
	}
	if code != http.StatusOK || !strings.EqualFold(strings.TrimSpace(respBody), "success") {
		return errcode.Wrap(fmt.Errorf("商户通知未成功：code=%d body=%q", code, respBody), errcode.ErrInternal)
	}

	return s.deps.Orders.Transition(ctx, o.OrderNo, func(cur *model.PaymentOrder) (*model.PaymentOrder, error) {
		now := time.Now()
		cur.NotifyStatus = model.NotifyStatusDone
		cur.NotifiedAt = &now
		return cur, nil
	})
}

// notifyBody 组装商户通知体：字段集固定，签名算法与商户请求鉴权同一套规范
// （sign.Canonical + sign.HMAC）；sign 字段单独放入 body，不参与自身的签名计算。
func notifyBody(o *model.PaymentOrder, secret string) map[string]string {
	fields := map[string]string{
		"order_no":     o.MchOrderNo,
		"sys_order_no": o.OrderNo,
		"status":       strconv.FormatInt(int64(model.OutStatus(o.Status)), 10),
		"amount":       strconv.FormatInt(o.Amount, 10),
		"fee":          strconv.FormatInt(o.Fee, 10),
		"reference_no": o.ReferenceNo,
		"timestamp":    strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	fields["sign"] = sign.HMAC(secret, sign.Canonical(fields))
	return fields
}

// recordNotifyAttempt 落一条通知尝试记录：attempt 取该订单已有记录数 + 1，
// response_body 截断到 notifyResponseBodyLimit 字节。
func (s *Payment) recordNotifyAttempt(ctx context.Context, orderNo string, code int, body string) error {
	count, err := s.deps.Notifications.CountByOrder(ctx, orderNo)
	if err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}

	if len(body) > notifyResponseBodyLimit {
		body = body[:notifyResponseBodyLimit]
	}
	n := &model.OrderNotification{
		OrderNo:      orderNo,
		Attempt:      int32(count) + 1,
		ResponseCode: int32(code),
		ResponseBody: body,
	}
	if err := s.deps.Notifications.Create(ctx, n); err != nil {
		return errcode.Wrap(err, errcode.ErrInternal)
	}
	return nil
}
