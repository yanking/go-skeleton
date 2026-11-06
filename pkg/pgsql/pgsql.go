// Package pgsql 构造基于 GORM 的 PostgreSQL 连接池并接入链路追踪，读写分离
// 经官方 dbresolver 插件自动路由：查询走副本（轮询）、增删改与事务内的语句
// 一律走主库。未配副本时不注册 resolver，全部语句走主库。与 pkg/mysql 同构：
// 两个 SQL 包同一套 API 形状，模板复制到新服务时无须换心智模型。
//
// 自动路由的纪律，违反即报错或数据异常：
//   - GORM 构造器写的锁读已正确走主库：resolver 认得 clause.Locking，
//     db.Clauses(clause.Locking{Strength: "UPDATE"}).Find(...) 落主库；
//   - 但 Raw SQL 的锁读靠字符串后缀猜，只认结尾恰好是 for update 的语句。
//     SELECT ... FOR UPDATE SKIP LOCKED / NOWAIT、FOR SHARE、以及末尾带分号的
//     一律被当成只读而路由到副本，PG 当场报 cannot execute SELECT FOR UPDATE
//     in a read-only transaction——任务抢占类写法正撞这条。Raw SQL 的锁读一律
//     显式 db.Clauses(dbresolver.Write)，不要依赖后缀猜测；
//   - 反方向也会猜错：Raw SQL 的 CTE（WITH ...）与带 hint 注释的查询被判为写、
//     压到主库，只读大查询要走副本同样得显式 db.Clauses(dbresolver.Read)；
//   - 写后立读要读己之写时，放进同一事务，或 db.Clauses(dbresolver.Write)
//     显式走主库，否则可能撞上副本延迟。
//
// DB 内嵌 *gorm.DB（直接链式调用），同时实现 app.Component。连接层走 otelsql
// 包装的 database/sql 池（SQL span 在这层埋），GORM 经 dialector 的 Conn
// 乘坐其上，池参数、DSN 校验与追踪一并由本包管。装配期不 ping：DB 比进程晚
// 就绪是常态（部署顺序），建连失败由首个查询暴露；要 readiness 就在上层
// 健康检查里 ping。
//
// 用法：
//
//	db := pgsql.New(pgsql.Config{
//	    Write: "postgres://user:pass@w1:5432/app?sslmode=disable",
//	    Read:  []string{"postgres://user:pass@r1:5432/app?sslmode=disable"},
//	    TracerProvider: tel.TracerProvider(), // 可选
//	})
//	db.WithContext(ctx).Create(&user)   // 写 → 主库
//	db.WithContext(ctx).First(&user, 1) // 读 → 副本轮询
package pgsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

// Config 连接池装配参数，声明式字段由配置文件绑定，注入项标 yaml:"-"。
type Config struct {
	// Write 主库（写）DSN（postgres:// URL 或 key=value 形式），必填。
	// 两个参数是硬要求，Validate 会拦：connect_timeout（建连超时）与
	// statement_timeout（语句超时，毫秒），零值都是「无限等待」。
	Write string `yaml:"write"`
	// Read 副本（读）DSN 列表，可空：空则不注册 resolver，全部语句走主库。
	// 每个 DSN 的形态要求同 Write。
	Read []string `yaml:"read"`
	// MaxOpenConns 全库最大打开连接数，零值取 defaultMaxOpenConns。
	// 本包没有「不限制」这一档：库侧 max_connections 是全局资源，要大就配大。
	MaxOpenConns int `yaml:"max_open_conns"`
	// MaxIdleConns 空闲连接数上限，零值取 defaultMaxIdleConns；高于 MaxOpenConns 时被夹到它。
	MaxIdleConns int `yaml:"max_idle_conns"`
	// ConnMaxLifetime 连接最大复用时长，零值取 defaultConnMaxLifetime。
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	// ConnMaxIdleTime 连接最大空闲时长，零值取 defaultConnMaxIdleTime。
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	// TracerProvider 链路追踪注入项，nil 则不埋 SQL span。
	TracerProvider trace.TracerProvider `yaml:"-"`
	// MeterProvider 指标注入项，nil 则不埋连接池指标。
	// 池饱和度（在用/空闲连接数、取连接的等待次数与等待时长）是 DB 变慢时最先要看的
	// 那组数：请求堆积到底是卡在库上还是卡在等连接上，只有这组数分得清。
	MeterProvider metric.MeterProvider `yaml:"-"`
	// Logger GORM 内部日志（慢 SQL、执行失败）桥接目标，nil 时用 slog.Default()。
	Logger *slog.Logger `yaml:"-"`
}

// Validate 实现 conf 的校验钩子：缺 DSN、池参数为负即报错。New 必调（装配期）；
// 要提前到绑定阶段，由服务 Config 的 Validate 委托到此（conf 只调顶层）。
func (c Config) Validate() error {
	if c.Write == "" {
		return errors.New("write DSN 不能为空")
	}
	if err := validateDSN("write", c.Write); err != nil {
		return err
	}
	for i, dsn := range c.Read {
		if dsn == "" {
			return errors.New("read 列表出现空 DSN")
		}
		if err := validateDSN(fmt.Sprintf("read[%d]", i), dsn); err != nil {
			return err
		}
	}
	if c.MaxOpenConns < 0 || c.MaxIdleConns < 0 ||
		c.ConnMaxLifetime < 0 || c.ConnMaxIdleTime < 0 {
		return errors.New("连接池参数不能为负")
	}
	return nil
}

// DB 内嵌 *gorm.DB，同时实现 app.Component。
type DB struct {
	*gorm.DB
	// pools 全部底层连接池（主库在前、副本随后）。resolver 只做路由不管池的
	// 生命周期，故由本包持有引用、Stop 时统一关闭。
	pools []*sql.DB
	// metricRegs 连接池指标的注册句柄，Stop 时注销：留着的话回调会在池关闭后
	// 仍被周期性调用。
	metricRegs []metric.Registration
}

// New 按 cfg 构造连接池并注册读写分离。装配期错误（校验不过、DSN 解析失败、
// GORM 初始化失败）直接 panic；密码等敏感字段不进错误文本。
func New(cfg Config) *DB {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配 PostgreSQL: %w", err))
	}

	otOpts := []otelsql.Option{otelsql.WithAttributes(semconv.DBSystemPostgreSQL)}
	if cfg.TracerProvider != nil {
		otOpts = append(otOpts, otelsql.WithTracerProvider(cfg.TracerProvider))
	}
	drv := otelsql.WrapDriver(stdlib.GetDefaultDriver(), otOpts...)

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	gormCfg := &gorm.Config{
		Logger:               gormLogger{logger: logger},
		DisableAutomaticPing: true,
	}

	pools := []*sql.DB{openPool(drv, poolLabel(0), cfg.Write, cfg)}
	gdb, err := gorm.Open(gormpostgres.New(gormpostgres.Config{Conn: pools[0]}), gormCfg)
	if err != nil {
		panic(fmt.Errorf("装配 PostgreSQL: 初始化 write GORM: %w", err))
	}

	if len(cfg.Read) > 0 {
		replicas := make([]gorm.Dialector, 0, len(cfg.Read))
		for i, dsn := range cfg.Read {
			sqlDB := openPool(drv, poolLabel(i+1), dsn, cfg)
			pools = append(pools, sqlDB)
			replicas = append(replicas, gormpostgres.New(gormpostgres.Config{Conn: sqlDB}))
		}
		// 副本轮询与此前显式双池的 Round Robin 行为保持一致。
		if err := gdb.Use(dbresolver.Register(dbresolver.Config{
			Replicas: replicas,
			Policy:   dbresolver.RoundRobinPolicy(),
		})); err != nil {
			panic(fmt.Errorf("装配 PostgreSQL: 注册读写分离: %w", err))
		}
	}

	return &DB{DB: gdb, pools: pools, metricRegs: registerPoolMetrics(pools, otOpts, cfg, logger)}
}

// registerPoolMetrics 给每个池注册连接池指标；未注入 MeterProvider 时记一条装配日志。
// 不报错而只是记日志，是因为不接遥测是本地开发的合法选择；但「指标静默消失」必须
// 在启动日志里看得见，否则等到排障时才发现没数据就太晚了。
func registerPoolMetrics(pools []*sql.DB, otOpts []otelsql.Option, cfg Config, logger *slog.Logger) []metric.Registration {
	if cfg.MeterProvider == nil {
		logger.Info("PostgreSQL 连接池指标未启用", "reason", "Config.MeterProvider 为 nil")
		return nil
	}
	regs := make([]metric.Registration, 0, len(pools))
	for i, p := range pools {
		label := poolLabel(i)
		// pool 属性区分主库与各副本；取值个数等于池数、固定不变，不会撑爆指标基数。
		//
		// 这里不能直接 append 到 otOpts：循环各轮会共享同一个底层数组，后一轮的
		// 写入可能覆盖前一轮（当前 otelsql 立刻消费掉选项所以没出事，但这是个
		// 不该留着的别名陷阱）。显式拷一份。
		opts := make([]otelsql.Option, 0, len(otOpts)+2)
		opts = append(opts, otOpts...)
		opts = append(opts,
			otelsql.WithMeterProvider(cfg.MeterProvider),
			otelsql.WithAttributes(attribute.String("pool", label)))
		reg, err := otelsql.RegisterDBStatsMetrics(p, opts...)
		if err != nil {
			panic(fmt.Errorf("装配 PostgreSQL: 注册 %s 连接池指标: %w", label, err))
		}
		regs = append(regs, reg)
	}
	return regs
}

// poolLabel 池的定位名：下标 0 是主库，其余按 Read 列表的次序编号。
func poolLabel(i int) string {
	if i == 0 {
		return "write"
	}
	return fmt.Sprintf("read[%d]", i-1)
}

// validateDSN 校验单个 DSN 的形态。两项都属于「不配就出事、而且出事很晚」：零值
// 即无限等待，网络分区且对端不发 RST（云上 NAT / LB 静默回收连接的常态）时，
// 请求 goroutine 会连着连接一起卡到进程重启。装配期拦住，错误文本直接给出该加什么。
//
// 与 pkg/mysql 查的参数不同名但同作用：PG 没有驱动层的 socket 读写超时，
// 对等物是服务端的 statement_timeout。
func validateDSN(label, dsn string) error {
	c, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("解析 %s DSN: %w", label, err)
	}
	if c.ConnectTimeout <= 0 {
		return fmt.Errorf("%s DSN 须带建连超时，如 connect_timeout=5（秒，零值即无限等待）", label)
	}
	if c.RuntimeParams["statement_timeout"] == "" {
		return fmt.Errorf("%s DSN 须带语句超时，如 statement_timeout=10000（毫秒，不配则语句可无限期执行）", label)
	}
	return nil
}

// openPool 建单个带追踪的连接池并套用池参数；label 只用于错误定位，不含 DSN 内容。
func openPool(drv driver.Driver, label, dsn string, cfg Config) *sql.DB {
	// 先显式解析：stdlib 驱动的 OpenConnector 是惰性的，不解析的话 DSN 错误
	// 要拖到首个查询才暴露。pgx 的错误文本自带密码掩码，可安全进日志。
	if _, err := pgx.ParseConfig(dsn); err != nil {
		panic(fmt.Errorf("装配 PostgreSQL: 解析 %s DSN: %w", label, err))
	}

	dctx, ok := drv.(driver.DriverContext)
	if !ok {
		panic(fmt.Errorf("装配 PostgreSQL: %s: 被包装的驱动不支持 DriverContext", label))
	}
	connector, err := dctx.OpenConnector(dsn)
	if err != nil {
		panic(fmt.Errorf("装配 PostgreSQL: 解析 %s DSN: %w", label, err))
	}
	sqlDB := sql.OpenDB(connector)
	applyPoolLimits(sqlDB, cfg)
	return sqlDB
}

// 连接池参数的缺省值。database/sql 的零值在服务端全是危险的默认，故本包一律给
// 显式缺省——模板的默认值就是第 10 个服务的生产配置。与 pkg/mysql 同值。
const (
	// defaultMaxOpenConns 打开连接数上限。库侧 max_connections 是全局资源
	// （PG 默认 100），不限制的话库一变慢、请求一堆积就会把它打穿，
	// 连运维自己都登不上去。按 (库上限 - 预留) / (服务数 × 副本数) 调。
	defaultMaxOpenConns = 20
	// defaultMaxIdleConns 空闲连接数上限，取 open 的一半：高到足以复用，
	// 低到不长期占着库的连接配额。
	//
	// 这里有个容易踩的坑：SetMaxIdleConns(0) 不是「取 database/sql 的默认 2」，
	// 而是「一条空闲连接都不留」——标准库里 n > 0 才采用，否则置 -1。于是每条
	// SQL 都要重新握手、认证、用完即关（pgx 的预编译语句缓存也因此永不命中，
	// 每条语句多一次往返）。这正是本包必须给缺省、而不能把零值原样传给 Setter 的原因。
	defaultMaxIdleConns = 10
	// defaultConnMaxLifetime 连接最长复用时长。永不过期的连接在 DB failover
	// （地址不变、后端换人）或 pgbouncer / proxy 滚动重启后不会主动轮换，
	// 可能长期指着已降级为只读的旧主。配得比部署侧的空闲回收窗口短。
	defaultConnMaxLifetime = 30 * time.Minute
	// defaultConnMaxIdleTime 空闲连接的回收时长：低峰期把连接还给库，
	// 不让闲着的池占住别人要用的配额。
	defaultConnMaxIdleTime = 5 * time.Minute
)

// poolLimits 算出生效的池参数：零值取缺省，空闲上限夹进打开上限之内。
//
// 单独成函数是为了可验算：SetMaxIdleConns 的生效值无法从 sql.DB 读回
// （Stats 只暴露 MaxOpenConnections），只能在计算这一侧钉住——而「零值
// 原样传下去」正是本包修掉的那个缺陷，必须有测试守着。
func poolLimits(cfg Config) (maxOpen, maxIdle int, lifetime, idleTime time.Duration) {
	maxOpen = cfg.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpenConns
	}
	maxIdle = cfg.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = defaultMaxIdleConns
	}
	// database/sql 会把高于 maxOpen 的 idle 上限静默截到 maxOpen；显式夹一次，
	// 免得配置看着生效其实没有。
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	lifetime = cfg.ConnMaxLifetime
	if lifetime <= 0 {
		lifetime = defaultConnMaxLifetime
	}
	idleTime = cfg.ConnMaxIdleTime
	if idleTime <= 0 {
		idleTime = defaultConnMaxIdleTime
	}
	return maxOpen, maxIdle, lifetime, idleTime
}

// applyPoolLimits 套用池参数。
func applyPoolLimits(sqlDB *sql.DB, cfg Config) {
	maxOpen, maxIdle, lifetime, idleTime := poolLimits(cfg)
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(lifetime)
	sqlDB.SetConnMaxIdleTime(idleTime)
}

// Name 实现 app.Component。
func (d *DB) Name() string { return "pgsql" }

// Start 实现 app.Component：连接池惰性建连，无需启动动作。
func (d *DB) Start(context.Context) error { return nil }

// Stop 实现 app.Component：注销连接池指标、关闭主库与全部副本的底层连接池，
// 错误聚合并返回。顺序不可换：指标回调读池的 stats，池关了再回调就没意义。
func (d *DB) Stop(context.Context) error {
	errs := make([]error, 0, len(d.metricRegs)+len(d.pools))
	for _, reg := range d.metricRegs {
		errs = append(errs, reg.Unregister())
	}
	for _, p := range d.pools {
		errs = append(errs, p.Close())
	}
	return errors.Join(errs...)
}
