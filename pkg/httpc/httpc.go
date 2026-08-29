// Package httpc 出站 HTTP 客户端：共享默认连接池，按体形态给方法——JSON
// （PostJSON）、urlencoded 表单（PostForm）、原文体（Post，multipart 等自拼形态
// 的出口）与 Get，
// 请求头逐调用给定，超时可逐调用覆盖（0 取 Config 缺省）。TracerProvider
// 注入后出站请求自动埋 client span 并注入 traceparent 头（显式注入，本包
// 不碰 otel 全局，与 pkg/telemetry 的约定一致；nil 即不埋点）。
// 只报网络与协议层错误；业务层失败（非 200、业务码拒绝）由调用方自行判定。
package httpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// defaultTimeout 缺省单次请求总超时，取一个对多数下游都够用的保守值；
// 下游明显更慢或更快的，逐调用传 timeout 覆盖。
const defaultTimeout = 15 * time.Second

// Config 客户端装配参数。
type Config struct {
	// Timeout 单次请求总超时缺省值（含连接、重定向与读体），零值取 15s；
	// 各方法的 timeout 参数传 0 即用此值，传正值则覆盖本次调用。
	Timeout time.Duration `yaml:"timeout"`
	// TracerProvider 链路追踪注入项，nil 则不埋出站 span、不注入 traceparent。
	TracerProvider trace.TracerProvider `yaml:"-"`
}

// Client 出站 HTTP 客户端，并发安全，一个下游域可共用一个实例。
type Client struct {
	hc      *http.Client
	timeout time.Duration
}

// New 构造客户端。连接池走 http.DefaultTransport（进程内共享）；注入了
// TracerProvider 时在其外包一层 otel transport 埋出站 span。
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	var rt http.RoundTripper = http.DefaultTransport
	if cfg.TracerProvider != nil {
		// 仓库不设 otel 全局，propagator 同样显式给出：出站带 W3C traceparent，
		// 同栈下游可续链，不认识该头的三方忽略即可，无副作用。
		rt = otelhttp.NewTransport(rt,
			otelhttp.WithTracerProvider(cfg.TracerProvider),
			otelhttp.WithPropagators(propagation.TraceContext{}))
	}
	return &Client{hc: &http.Client{Transport: rt}, timeout: timeout}
}

// PostJSON 发 JSON POST：body 序列化后发出，Content-Type 置 application/json
// （header 里显式给出可覆盖）；body 为 nil 时发空体。返回 HTTP 状态码与响应体原文。
func (c *Client) PostJSON(ctx context.Context, url string, header map[string]string, body any, timeout time.Duration) (int, string, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, "", fmt.Errorf("序列化请求体: %w", err)
		}
	}
	h := make(map[string]string, len(header)+1)
	h["Content-Type"] = "application/json"
	for k, v := range header {
		h[k] = v
	}
	return c.Post(ctx, url, h, payload, timeout)
}

// PostForm 发 application/x-www-form-urlencoded 表单 POST：form 编码后发出，
// Content-Type 置表单（header 里显式给出可覆盖）。
func (c *Client) PostForm(ctx context.Context, url string, header map[string]string, form neturl.Values, timeout time.Duration) (int, string, error) {
	h := make(map[string]string, len(header)+1)
	h["Content-Type"] = "application/x-www-form-urlencoded"
	for k, v := range header {
		h[k] = v
	}
	return c.Post(ctx, url, h, []byte(form.Encode()), timeout)
}

// Post 发原文体 POST：body 原样发出，Content-Type 由调用方经 header 给定
// （不给则不设）。表单、XML、预签名报文等非 JSON 形态用这个。
func (c *Client) Post(ctx context.Context, url string, header map[string]string, body []byte, timeout time.Duration) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("构建请求: %w", err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return c.do(req, timeout)
}

// Get 发 GET，错误语义同 PostJSON。
func (c *Client) Get(ctx context.Context, url string, header map[string]string, timeout time.Duration) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", fmt.Errorf("构建请求: %w", err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return c.do(req, timeout)
}

// do 发出请求并读回响应体原文；timeout ≤0 取 Config 缺省。超时经 ctx 实现，
// cancel 在读完响应体后才触发，故覆盖到读体全程。
func (c *Client) do(req *http.Request, timeout time.Duration) (int, string, error) {
	if timeout <= 0 {
		timeout = c.timeout
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()

	resp, err := c.hc.Do(req.WithContext(ctx))
	if err != nil {
		return 0, "", fmt.Errorf("发起请求: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("读取响应: %w", err)
	}
	return resp.StatusCode, string(raw), nil
}
