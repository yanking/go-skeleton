// Package redis 构造 go-redis 客户端：DB 内嵌 UniversalClient，命令方法直接可用，
// 业务代码与部署形态解耦。
//
// 部署形态由 Mode 显式声明，不靠地址数量猜：auto（单地址单机、多地址集群）只在
// 单地址时允许，多地址必须写明 cluster 或 standalone。原因是推断会静默错判——
// 两个独立单机地址（HA 对）会被当成集群，而且不报错，只是键全落在其中一个节点上，
// 重启换人后缓存与会话凭空消失；反过来云托管的集群配置端点只有一个地址，猜成单机
// 则跨 slot 的键一律 MOVED。部署形态只有部署者知道，故由他表态。
//
// 经 Logger 注入 slog 后自动上报命令日志：执行失败进 Error（redis.Nil 是
// 查空键的正常返回，除外）、超过慢命令阈值进 Warn、其余进 Debug。只记
// 命令名不记参数——参数可能含敏感值。span 同样不带命令参数（见 New 里的
// WithDBStatement(false)）：trace 后端的权限通常比日志平台更宽、保留更久。
//
// New 还会接管 go-redis 的进程级日志出口（goredis.SetLogger），否则它的内部日志
// （建连失败、集群拓扑变更）以非结构化文本直落 stderr，JSON 采集链路会丢掉——
// 而 Redis 抖动时正是这类日志最要紧的时候。这是 go-style 硬规则 6 的又一处全局
// 例外，代价同 pkg/log：多次 New 后者覆盖前者，测试里需要时自行保存/还原。
//
// DB 同时实现 app.Component：客户端惰性建连，Start 直接返回，Stop 关闭
// 客户端。装配期不 ping，理由同 SQL 连接池：DB 晚于进程就绪是常态。
//
// 用法：
//
//	rdb := redis.New(redis.Config{Addrs: []string{"127.0.0.1:6379"}})
//	rdb.Get(ctx, "key") // 与 go-redis 手感完全一致
package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// defaultConnMaxLifetime 连接最长复用时长的缺省值，与 pkg/mysql、pkg/pgsql 同值。
// 零值（永不过期）的连接在 failover 或代理滚动重启后不会主动轮换。
const defaultConnMaxLifetime = 30 * time.Minute

// Mode Redis 的部署形态。
type Mode string

const (
	// ModeAuto 按地址数量推断，且只允许单地址（即单机）。Mode 零值即此项。
	// 多地址时 Validate 会要求显式声明形态，理由见包注释。
	ModeAuto Mode = "auto"
	// ModeStandalone 单机，addrs 只能有一个地址；多入口请在前面放 LB 或 VIP。
	ModeStandalone Mode = "standalone"
	// ModeCluster 集群。给一个地址也成立——云托管 Redis 的集群配置端点
	// （如 ElastiCache 的 configuration endpoint）正是单地址集群。
	ModeCluster Mode = "cluster"
)

// UnmarshalText 实现 encoding.TextUnmarshaler，使配置文件里的形态拼写错误在绑定
// 阶段就暴露。空串归一到 ModeAuto，与零值的含义保持一致。
func (m *Mode) UnmarshalText(text []byte) error {
	switch mode := Mode(text); mode {
	case "", ModeAuto:
		*m = ModeAuto
	case ModeStandalone:
		*m = ModeStandalone
	case ModeCluster:
		*m = ModeCluster
	default:
		return fmt.Errorf("未知部署形态 %q，只支持 %q、%q 与 %q",
			mode, ModeAuto, ModeStandalone, ModeCluster)
	}
	return nil
}

// Config 客户端装配参数，声明式字段由配置文件绑定，注入项标 yaml:"-"。
type Config struct {
	// Addrs 节点地址列表。集群要给种子节点（客户端自动发现其余分片），
	// 单机只给一个。多地址时必须同时声明 Mode。
	Addrs []string `yaml:"addrs"`
	// Mode 部署形态，零值取 ModeAuto（只允许单地址）。
	Mode Mode `yaml:"mode"`
	// Username ACL 用户名（Redis 6+），无则留空走默认用户。
	Username string `yaml:"username"`
	// Password 密码，无则留空。
	Password string `yaml:"password"`
	// DB 单机模式选库序号；集群不支持选库，必须为 0。
	DB int `yaml:"db"`
	// TLS 是否经 TLS 连接。托管 Redis 常强制加密传输（ElastiCache 的
	// in-transit encryption、Azure Cache 默认），不开就连不上。
	// 用系统根证书校验服务端，证书不受信即建连失败——不提供跳过校验的开关。
	TLS bool `yaml:"tls"`
	// PoolSize 每节点连接池大小，零值取 go-redis 默认（10×GOMAXPROCS）。
	PoolSize int `yaml:"pool_size"`
	// MaxRetries 单条命令的重试次数，零值取 go-redis 默认 3，负值即不重试。
	//
	// 注意重试语义：go-redis 在网络错误时重试整条命令，于是「服务端已执行、
	// 响应在回程丢了」的写命令（INCR、LPUSH、扣库存的 Lua）会被执行第二遍。
	// 非幂等的写路径要么配 -1 自己管重试，要么把命令写成幂等的。
	// 这与 pkg/httpc「实现内绝不重试」的取舍不同，故在此写明。
	MaxRetries int `yaml:"max_retries"`
	// DialTimeout 建连超时，零值取 go-redis 默认 5s。
	DialTimeout time.Duration `yaml:"dial_timeout"`
	// ReadTimeout 读超时，零值取 go-redis 默认；负值即无超时（阻塞命令需要）。
	ReadTimeout time.Duration `yaml:"read_timeout"`
	// WriteTimeout 写超时，零值取 go-redis 默认。
	WriteTimeout time.Duration `yaml:"write_timeout"`
	// ConnMaxLifetime 连接最长复用时长，零值取 defaultConnMaxLifetime。
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	// TracerProvider 链路追踪注入项，nil 则不埋 DB span。
	TracerProvider trace.TracerProvider `yaml:"-"`
	// MeterProvider 指标注入项，nil 则不埋客户端指标（连接池命中/超时、命令耗时）。
	MeterProvider metric.MeterProvider `yaml:"-"`
	// Logger 命令日志（执行失败、慢命令）与 go-redis 内部日志的输出，
	// nil 时用 slog.Default()。
	Logger *slog.Logger `yaml:"-"`
}

// mode 取归一后的部署形态。直接在 Go 里构造 Config 时 Mode 是空串（不经
// UnmarshalText），若各处拿 c.Mode 直接比对，空串会既不等于 auto、也不等于
// cluster / standalone，于是所有校验分支一并跳过——零值绕过全部检查正是
// 本仓反复出现的失败开放形态，故一律经本方法取值。
func (c Config) mode() Mode {
	if c.Mode == "" {
		return ModeAuto
	}
	return c.Mode
}

// isCluster 判断生效的形态是否为集群。
func (c Config) isCluster() bool {
	return c.mode() == ModeCluster
}

// Validate 实现 conf 的校验钩子：地址缺失、形态与地址数不匹配、集群选库即报错。
// New 必调（装配期）；要提前到绑定阶段，由服务 Config 的 Validate 委托到此
// （conf 只调顶层）。
func (c Config) Validate() error {
	if len(c.Addrs) == 0 {
		return errors.New("addrs 不能为空")
	}
	// 多地址 + auto 是静默错判的入口（见包注释），要求部署方表态。
	if c.mode() == ModeAuto && len(c.Addrs) > 1 {
		return errors.New("多地址时须显式声明 mode 为 cluster 或 standalone：" +
			"按地址数推断会把独立的单机地址当成集群，不报错但键只落在其中一个节点上")
	}
	if c.mode() == ModeStandalone && len(c.Addrs) != 1 {
		return fmt.Errorf("mode 为 standalone 时 addrs 只能有一个地址，当前 %d 个"+
			"（多入口请在前面放 LB 或 VIP）", len(c.Addrs))
	}
	if c.isCluster() && c.DB != 0 {
		return errors.New("集群模式不支持选库, db 须为 0")
	}
	if c.PoolSize < 0 {
		return errors.New("pool_size 不能为负")
	}
	if c.DialTimeout < 0 || c.WriteTimeout < 0 || c.ConnMaxLifetime < 0 {
		return errors.New("dial_timeout、write_timeout、conn_max_lifetime 不能为负")
	}
	return nil
}

// DB 内嵌 redis.UniversalClient（命令方法直接可用），同时实现 app.Component。
type DB struct {
	goredis.UniversalClient
}

// New 按 cfg 构造客户端。装配期错误（校验不过、追踪或指标注入失败）直接 panic。
func New(cfg Config) *DB {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配 DB: %w", err))
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// go-redis 的内部日志接管到 slog（包注释交代了这个全局例外的代价）。
	goredis.SetLogger(internalLogger{logger: logger})

	lifetime := cfg.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = defaultConnMaxLifetime
	}
	opts := &goredis.UniversalOptions{
		Addrs:           cfg.Addrs,
		IsClusterMode:   cfg.isCluster(),
		Username:        cfg.Username,
		Password:        cfg.Password,
		DB:              cfg.DB,
		PoolSize:        cfg.PoolSize,
		MaxRetries:      cfg.MaxRetries,
		DialTimeout:     cfg.DialTimeout,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		ConnMaxLifetime: lifetime,
		// ctx 的期限要能真的收紧 socket 期限。关着的话（go-redis 的默认）
		// baseClient 用 context.Background() 取代调用方的 ctx：客户端早已断开、
		// 上游 deadline 早已到点，命令还在跑，goroutine 连着连接一起堆积；
		// 阻塞命令（BLPOP 等）更是无限期挂住。
		ContextTimeoutEnabled: true,
	}
	if cfg.TLS {
		// 只给 ServerName 之外的默认值：系统根证书、默认版本下限。
		// 不暴露 InsecureSkipVerify——"连不上就跳过校验"是最常见的降级误用。
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := goredis.NewUniversalClient(opts)

	if cfg.TracerProvider != nil {
		if err := redisotel.InstrumentTracing(client,
			redisotel.WithTracerProvider(cfg.TracerProvider),
			// 命令参数不进 span：redisotel 默认把整条命令（含 session token、
			// 验证码、缓存的用户资料）写进 db.statement，而它只对 CONFIG SET
			// 的几个参数脱敏。与本包日志通道同一尺度。
			redisotel.WithDBStatement(false),
			// 每条命令一次 runtime.Caller 的栈遍历，换来三个定位属性；
			// 高 QPS 下不值，排障靠 trace 本身的 span 树即可。
			redisotel.WithCallerEnabled(false),
		); err != nil {
			panic(fmt.Errorf("装配 DB: 注入链路追踪: %w", err))
		}
	}
	if cfg.MeterProvider != nil {
		if err := redisotel.InstrumentMetrics(client,
			redisotel.WithMeterProvider(cfg.MeterProvider)); err != nil {
			panic(fmt.Errorf("装配 DB: 注入指标: %w", err))
		}
	} else {
		// 漏注入只会让指标静默消失（instrumentation 回落到 otel 的 no-op 全局），
		// 故在启动日志里留一条，别等排障时才发现没数据。
		logger.Info("Redis 客户端指标未启用", "reason", "Config.MeterProvider 为 nil")
	}

	client.AddHook(cmdLogger{logger: logger})

	return &DB{UniversalClient: client}
}

// Name 实现 app.Component。
func (r *DB) Name() string { return "redis" }

// Start 实现 app.Component：客户端惰性建连，无需启动动作。
func (r *DB) Start(context.Context) error { return nil }

// Stop 实现 app.Component：关闭客户端与连接池。
func (r *DB) Stop(context.Context) error { return r.Close() }
