package handler

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/yanking/go-skeleton/internal/payment/service"
)

// maxCallbackBodyBytes 回调请求体读取上限，防三方异常报文撑爆内存。
const maxCallbackBodyBytes = 1 << 20 // 1MB

// callbackPathPrefix 三方回调路径固定前缀，末段为渠道实例 ID。
const callbackPathPrefix = "/callbacks/payment/"

// Callback 是三方支付回调的原生 HTTP 出口（非 gRPC/gateway：渠道报文格式各异，不走
// proto 契约）：只做协议转换——解析路径与请求信息组装后交 service 处理，按其应答写回。
type Callback struct {
	svc *service.Payment
}

// NewCallback 构造回调 HTTP 出口，处理 POST|GET /callbacks/payment/{instanceID}。
func NewCallback(svc *service.Payment) http.Handler {
	return &Callback{svc: svc}
}

// ServeHTTP 解析请求、组装 CallbackIn 交 service 处理，按 CallbackReply 写回应答。
// 路径不匹配固定形状或实例 ID 非法整数一律 404。
func (h *Callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	instanceID, err := parseCallbackPath(r.URL.Path)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	reply := h.svc.HandleChannelCallback(r.Context(), service.CallbackIn{
		InstanceID: instanceID,
		Headers:    extractHeaders(r),
		Query:      r.URL.RawQuery,
		RawBody:    readCallbackBody(r),
		IP:         callbackClientIP(r),
	})

	w.WriteHeader(reply.HTTPStatus)
	if reply.Body != "" {
		_, _ = w.Write([]byte(reply.Body))
	}
}

// parseCallbackPath 从请求路径解析末段渠道实例 ID；路径不匹配
// "/callbacks/payment/{instanceID}" 固定形状或 ID 非合法整数均返回 error。
func parseCallbackPath(path string) (int64, error) {
	if !strings.HasPrefix(path, callbackPathPrefix) {
		return 0, fmt.Errorf("不是回调路径: %s", path)
	}
	rest := path[len(callbackPathPrefix):]
	if rest == "" || strings.Contains(rest, "/") {
		return 0, fmt.Errorf("非法回调路径: %s", path)
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("非法渠道实例 ID: %w", err)
	}
	return id, nil
}

// extractHeaders 把请求头拍平为单值 map：同名多值取首个（http.Header 按 Add 顺序保留）。
func extractHeaders(r *http.Request) map[string]string {
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	return headers
}

// readCallbackBody 读取请求体原文，上限 maxCallbackBodyBytes 防撑爆内存；读取失败
// （如客户端提前断开）按已读到的部分处理，不阻断回调落痕。
func readCallbackBody(r *http.Request) string {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxCallbackBodyBytes))
	return string(body)
}

// callbackClientIP 提取回调来源 IP：优先取 X-Forwarded-For 首跳（经反向代理时最贴近
// 真实客户端，逗号分隔取第一段、去首尾空白），去掉可能带的端口——service 侧
// callbackIPAllowed 按纯 IP 逐项精确匹配；缺省回退取 RemoteAddr 去端口。
func callbackClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hop, _, _ := strings.Cut(xff, ",")
		hop = strings.TrimSpace(hop)
		if host, _, err := net.SplitHostPort(hop); err == nil {
			return host
		}
		return hop
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
