// Package httpc 出站 HTTP 客户端：逐 Client 独立的连接池，按体形态给方法——JSON
// （PostJSON）、urlencoded 表单（PostForm）、原文体（Post，multipart 等自拼形态
// 的出口）与 Get，
// 请求头逐调用给定，超时可逐调用覆盖（0 取 Config 缺省）。TracerProvider
// 注入后出站请求自动埋 client span 并注入 traceparent 头（显式注入，本包
// 不碰 otel 全局，与 pkg/telemetry 的约定一致；nil 即不埋点）。
// 只报网络与协议层错误；业务层失败（非 200、业务码拒绝）由调用方自行判定。
// 注入 Logger 后每次出站记一条日志（method、url、status、耗时、错误），
// url 只留 scheme://host/path、报文一概不记——理由见 sanitizeURL 与 logOutbound。
//
// 每个 Client 持有自己的 Transport（由 http.DefaultTransport 克隆而来），连接池、
// 空闲连接上限与 keep-alive 开关因此逐 Client 生效，互不干扰——共享
// DefaultTransport 时这些旋钮一调就是全进程，一个下游的调优会踩到别的下游。
// 响应体读取有大小上限（MaxBodyBytes），超限报错而非截断：截断的报文喂给调用方
// 解析会得出错误结论。
package httpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

// defaultMaxBodyBytes 缺省响应体上限。取「正常报文绝不会碰到、失控响应又撑不爆
// 内存」的量级：接口报文通常在几 KB，4MiB 留了三个数量级的余量。
const defaultMaxBodyBytes = 4 << 20

// defaultMaxIdleConnsPerHost 缺省单主机空闲连接上限。http.Transport 的默认值是 2，
// 并发出站时绝大多数连接会用完即弃、每笔请求重新握手；取 32 与常见的单下游并发
// 上限同量级，调用方按自己的并发配。
const defaultMaxIdleConnsPerHost = 32

// Config 客户端装配参数。
type Config struct {
	// Timeout 单次请求总超时缺省值（含连接、重定向与读体），零值取 15s；
	// 各方法的 timeout 参数传 0 即用此值，传正值则覆盖本次调用。
	Timeout time.Duration `yaml:"timeout"`
	// TracerProvider 链路追踪注入项，nil 则不埋出站 span、不注入 traceparent。
	TracerProvider trace.TracerProvider `yaml:"-"`
	// Logger 出站日志注入项，nil 即不打日志——通用包不该在调用方没声明的情况下
	// 往进程日志里写东西，要日志由装配根显式给。每次出站一条，字段见 logOutbound。
	Logger *slog.Logger `yaml:"-"`
	// MaxBodyBytes 单次响应体读取上限，零值取 4MiB，负值即不设上限。
	// 超限返回错误且不返回内容——截断的报文解析出来是错的结论，报错才安全。
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// MaxIdleConnsPerHost 单主机空闲连接上限，零值取 32。按调用方对该下游的并发
	// 上限配：低于并发数的部分会退化成每笔请求一次握手。
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host"`
	// DisableKeepAlives 关掉连接复用，每次请求都新建连接。
	//
	// 只为一种情形而设：**下游只接受 GET 的非幂等接口**。net/http 会在复用的
	// keep-alive 连接上自动重放 GET/HEAD/OPTIONS/TRACE，调用方毫不知情，
	// 「实现内绝不重试」挡不住它。代价是每笔请求一次握手，别的场景不要开。
	DisableKeepAlives bool `yaml:"disable_keep_alives"`
}

// Client 出站 HTTP 客户端，并发安全，一个下游域可共用一个实例。
type Client struct {
	hc           *http.Client
	timeout      time.Duration
	maxBodyBytes int64        // ≤0 即不设上限（Config 的零值已在 New 里换成缺省值）
	log          *slog.Logger // nil 即不打出站日志
}

// New 构造客户端。每个 Client 克隆一份 http.DefaultTransport 自用：连接池与
// keep-alive 旋钮因此逐 Client 生效，调一个下游不会踩到别的下游；注入了
// TracerProvider 时在其外包一层 otel transport 埋出站 span。
func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxBody := cfg.MaxBodyBytes
	switch {
	case maxBody == 0:
		maxBody = defaultMaxBodyBytes
	case maxBody < 0:
		maxBody = 0 // 负值即不设上限
	}
	idlePerHost := cfg.MaxIdleConnsPerHost
	if idlePerHost <= 0 {
		idlePerHost = defaultMaxIdleConnsPerHost
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = idlePerHost
	tr.DisableKeepAlives = cfg.DisableKeepAlives
	// 全局空闲上限不得低于单主机上限，否则单主机那档形同虚设。
	if tr.MaxIdleConns < idlePerHost {
		tr.MaxIdleConns = idlePerHost
	}

	var rt http.RoundTripper = tr
	if cfg.TracerProvider != nil {
		// 仓库不设 otel 全局，propagator 同样显式给出：出站带 W3C traceparent，
		// 同栈下游可续链，不认识该头的三方忽略即可，无副作用。
		rt = otelhttp.NewTransport(rt,
			otelhttp.WithTracerProvider(cfg.TracerProvider),
			otelhttp.WithPropagators(propagation.TraceContext{}))
	}
	return &Client{hc: &http.Client{Transport: rt}, timeout: timeout, maxBodyBytes: maxBody, log: cfg.Logger}
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
// cancel 在读完响应体后才触发，故覆盖到读体全程。出站日志在此收口：命名返回值
// 配 defer，四条返回路径共用一条记录，不会漏也不会重。
func (c *Client) do(req *http.Request, timeout time.Duration) (code int, body string, err error) {
	if timeout <= 0 {
		timeout = c.timeout
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	defer cancel()

	// 日志的 defer 后注册故先执行，此时 ctx 尚未 cancel，trace 字段仍提得到。
	start := time.Now()
	defer func() { c.logOutbound(ctx, req.Method, req.URL, code, time.Since(start), err) }()

	resp, err := c.hc.Do(req.WithContext(ctx))
	if err != nil {
		return 0, "", fmt.Errorf("发起请求: %w", err)
	}
	defer resp.Body.Close()

	// 多读一个字节来判超限：读满 max+1 就说明下游给的比上限多。
	r := io.Reader(resp.Body)
	if c.maxBodyBytes > 0 {
		r = io.LimitReader(resp.Body, c.maxBodyBytes+1)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("读取响应: %w", err)
	}
	if c.maxBodyBytes > 0 && int64(len(raw)) > c.maxBodyBytes {
		// 不返回已读到的部分：截断的报文解析出来是错的结论，比读不到更危险。
		return resp.StatusCode, "", fmt.Errorf("响应体超过 %d 字节上限", c.maxBodyBytes)
	}
	return resp.StatusCode, string(raw), nil
}

// sanitizeURL 供日志使用的 URL：只留 scheme://host/path。query 与 userinfo 是
// 下游协议决定的、本包管不着的地方，凭证（token、签名、密码）挂在那里是常见形态，
// 原样进日志就是把它们写进了日志文件；排障需要的只是「调了哪个端点」，path 已经够。
func sanitizeURL(u *neturl.URL) string {
	if u == nil {
		return ""
	}
	safe := neturl.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}
	return safe.String()
}

// logOutbound 出站收口日志：一次出站恰好一条。成功且 2xx 记 Info，网络错误或非 2xx
// 记 Warn——非 2xx 不是本包的 error，但排障时它与网络错误同样值得一眼看见。
// **请求体与响应体一律不记**：报文的内容本包无从判断，其中的个人信息与凭证一旦
// 进了日志就撤不回来；要记哪几个字段，由知道报文语义的调用方自己记。
func (c *Client) logOutbound(ctx context.Context, method string, u *neturl.URL, code int, cost time.Duration, err error) {
	if c.log == nil {
		return
	}
	attrs := []any{
		"method", method,
		"url", sanitizeURL(u),
		"status", code,
		"cost_ms", cost.Milliseconds(),
	}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	if err != nil || code < 200 || code > 299 {
		c.log.WarnContext(ctx, "出站 HTTP", attrs...)
		return
	}
	c.log.InfoContext(ctx, "出站 HTTP", attrs...)
}
