// Package mysql 构造基于 GORM 的 MySQL 连接池并接入链路追踪，读写分离经
// 官方 dbresolver 插件自动路由：查询走副本（轮询）、增删改与事务内的语句
// 一律走主库。未配副本时不注册 resolver，全部语句走主库（本地开发单实例够用）。
//
// 自动路由的两条纪律，违反即报错或数据异常：
//   - 锁读（SELECT ... FOR UPDATE）必须包进事务——路由只看操作类型、不识别
//     锁语义，裸的锁读会被路由到只读副本上；
//   - 写后立读要读己之写时，放进同一事务，或 db.Clauses(dbresolver.Write)
//     显式走主库，否则可能撞上副本延迟。
//
// DB 内嵌 *gorm.DB（直接链式调用），同时实现 app.Component。连接层走 otelsql
// 包装的 database/sql 池（SQL span 在这层埋），GORM 经 dialector 的 Conn
// 乘坐其上，池参数、DSN 校验与追踪一并由本包管。装配期不 ping、不探版本：
// DB 比进程晚就绪是常态（部署顺序），建连失败由首个查询暴露；要 readiness
// 就在上层健康检查里 ping。
//
// 用法：
//
//	db := mysql.New(mysql.Config{
//	    Write: "user:pass@tcp(w1:3306)/app?parseTime=true",
//	    Read:  []string{"user:pass@tcp(r1:3306)/app?parseTime=true"},
//	    TracerProvider: tel.TracerProvider(), // 可选
//	})
//	db.WithContext(ctx).Create(&user)   // 写 → 主库
//	db.WithContext(ctx).First(&user, 1) // 读 → 副本轮询
package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/XSAM/otelsql"
	sqlmysql "github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

// Config 连接池装配参数，声明式字段由配置文件绑定，注入项标 yaml:"-"。
type Config struct {
	// Write 主库（写）DSN，必填。须带 parseTime=true，否则时间列扫不进 time.Time。
	Write string `yaml:"write"`
	// Read 副本（读）DSN 列表，可空：空则不注册 resolver，全部语句走主库。
	Read []string `yaml:"read"`
	// MaxOpenConns 全库最大打开连接数，零值不限制。
	MaxOpenConns int `yaml:"max_open_conns"`
	// MaxIdleConns 空闲连接数上限，零值取 database/sql 默认（2）。
	MaxIdleConns int `yaml:"max_idle_conns"`
	// ConnMaxLifetime 连接最大复用时长，零值不限制；建议按部署侧超时配置。
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
	// ConnMaxIdleTime 连接最大空闲时长，零值不限制。
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time"`
	// TracerProvider 链路追踪注入项，nil 则不埋 SQL span。
	TracerProvider trace.TracerProvider `yaml:"-"`
	// Logger GORM 内部日志（慢 SQL、执行失败）桥接目标，nil 时用 slog.Default()。
	Logger *slog.Logger `yaml:"-"`
}

// Validate 实现 conf 的校验钩子，缺 DSN、池参数为负在绑定阶段即报错。
func (c Config) Validate() error {
	if c.Write == "" {
		return errors.New("write DSN 不能为空")
	}
	for _, dsn := range c.Read {
		if dsn == "" {
			return errors.New("read 列表出现空 DSN")
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
}

// New 按 cfg 构造连接池并注册读写分离。装配期错误（校验不过、DSN 解析失败、
// GORM 初始化失败）直接 panic；密码等敏感字段不进错误文本。
func New(cfg Config) *DB {
	if err := cfg.Validate(); err != nil {
		panic(fmt.Errorf("装配 MySQL: %w", err))
	}

	otOpts := []otelsql.Option{otelsql.WithAttributes(semconv.DBSystemMySQL)}
	if cfg.TracerProvider != nil {
		otOpts = append(otOpts, otelsql.WithTracerProvider(cfg.TracerProvider))
	}
	drv := otelsql.WrapDriver(&sqlmysql.MySQLDriver{}, otOpts...)

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	gormCfg := &gorm.Config{
		Logger:               gormLogger{logger: logger},
		DisableAutomaticPing: true,
	}

	// SkipInitializeWithVersion 跳过 gorm.Open 时的 SELECT VERSION() 探测、
	// DisableAutomaticPing 跳过隐式 ping：两者都是装配期的隐式建连，会打破
	// 「装配期不触网」的契约。代价是方言按 MySQL 8+ 假设，MariaDB / 5.x 的
	// 差异适配由使用方自行确认。副本的 resolver 内部 gorm.Open 复制此配置，
	// 同样不触网。
	pools := []*sql.DB{openPool(drv, "write", cfg.Write, cfg)}
	gdb, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      pools[0],
		SkipInitializeWithVersion: true,
	}), gormCfg)
	if err != nil {
		panic(fmt.Errorf("装配 MySQL: 初始化 write GORM: %w", err))
	}

	if len(cfg.Read) > 0 {
		replicas := make([]gorm.Dialector, 0, len(cfg.Read))
		for i, dsn := range cfg.Read {
			sqlDB := openPool(drv, fmt.Sprintf("read[%d]", i), dsn, cfg)
			pools = append(pools, sqlDB)
			replicas = append(replicas, gormmysql.New(gormmysql.Config{
				Conn:                      sqlDB,
				SkipInitializeWithVersion: true,
			}))
		}
		// 副本轮询与此前显式双池的 Round Robin 行为保持一致。
		if err := gdb.Use(dbresolver.Register(dbresolver.Config{
			Replicas: replicas,
			Policy:   dbresolver.RoundRobinPolicy(),
		})); err != nil {
			panic(fmt.Errorf("装配 MySQL: 注册读写分离: %w", err))
		}
	}

	return &DB{DB: gdb, pools: pools}
}

// openPool 建单个带追踪的连接池并套用池参数；label 只用于错误定位，不含 DSN 内容。
func openPool(drv driver.Driver, label, dsn string, cfg Config) *sql.DB {
	dctx, ok := drv.(driver.DriverContext)
	if !ok {
		panic(fmt.Errorf("装配 MySQL: %s: 被包装的驱动不支持 DriverContext", label))
	}
	// OpenConnector 只解析 DSN、不建连，格式错误在此报出。
	connector, err := dctx.OpenConnector(dsn)
	if err != nil {
		panic(fmt.Errorf("装配 MySQL: 解析 %s DSN: %w", label, err))
	}
	sqlDB := sql.OpenDB(connector)
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	return sqlDB
}

// Name 实现 app.Component。
func (d *DB) Name() string { return "mysql" }

// Start 实现 app.Component：连接池惰性建连，无需启动动作。
func (d *DB) Start(context.Context) error { return nil }

// Stop 实现 app.Component：关闭主库与全部副本的底层连接池，错误聚合并返回。
func (d *DB) Stop(context.Context) error {
	errs := make([]error, len(d.pools))
	for i, p := range d.pools {
		errs[i] = p.Close()
	}
	return errors.Join(errs...)
}
