# __svc__ 服务配置：只放声明式参数；Writer、Extractors 等装配期注入项不在此出现。
# 凭证以带引号的环境变量引用注入（写法见 pkg/conf 包注释），启动时展开、未设置即起不来；明文不入库（宪法第 4 条）。
# 部署相关的取值（探针、宽限期预算、生产必须改的几项）见 docs/deploy.md。

log:
  name: __svc__
  level: info # 生产留 info；临时开 debug 由 cmd 里的 LevelVar 在运行期调，不必重启、不丢现场
  format: json

app:
  # 停机总超时，按剩余组件数均分。它与 transport.drain_delay 是串行相加的两段，
  # 两段之和要装进部署侧的宽限期（K8s 默认 30s 装不下 5s + 25s，见 docs/deploy.md）。
  stop_timeout: 25s

telemetry:
  # 本地开发打标准输出。**生产必须改成 otlp 并配 endpoint**——留 stdout 的话全部
  # span 与 metric 会以 JSON 打进标准输出，量按 QPS 涨，还会把真日志挤出采集配额。
  exporter: stdout
  # endpoint: "collector:4317" # exporter 为 otlp 时必填
  # insecure: true             # 本地 collector 无证书时才开，生产走 TLS
  #
  # 采样：默认全采（尊重上游决定，无上游时都采）。高 QPS 服务改成比例采样，
  # 否则导出带宽与后端存储按 QPS 线性涨。
  # sampler: parentbased_traceidratio
  # sample_ratio: 0.05 # 比例采样时必填，取 (0,1]——留 0 的话一条都不采，且毫无征兆

# 用到 PostgreSQL 时放开。DSN 的两个超时是硬要求（装配期校验）：零值即无限等待，
# 网络分区且对端不发 RST 时，请求 goroutine 会连着连接一起卡到进程重启。
# pgsql:
#   write: "postgres://app:${DB_PASSWORD}@127.0.0.1:5432/app?sslmode=disable&connect_timeout=5&statement_timeout=10000"
#   read: # 副本，可空；空则全部语句走主库
#     - "postgres://app:${DB_PASSWORD}@127.0.0.1:5433/app?sslmode=disable&connect_timeout=5&statement_timeout=10000"
#   max_open_conns: 20 # 库侧 max_connections 是全局资源，按 (库上限 - 预留) / (服务数 × 副本数) 调
#
# MySQL 版的硬要求多一项 parseTime=true：
# mysql:
#   write: "app:${DB_PASSWORD}@tcp(127.0.0.1:3306)/app?parseTime=true&timeout=5s&readTimeout=10s&writeTimeout=10s"

# 用到 Redis 时放开。多地址必须显式声明 mode——按地址数推断会把两个独立的单机
# 地址当成集群，而且不报错，只是键全落在其中一个节点上，重启换人即丢。
# redis:
#   addrs: ["127.0.0.1:6379"]
#   password: "${REDIS_PASSWORD}"
#   mode: standalone # auto（仅单地址）/ standalone / cluster
#   tls: false       # 托管 Redis 常强制加密传输，不开就连不上

# 用到异步任务时放开。asynq 的 Redis 须是专用实例（maxmemory-policy noeviction），
# 理由见 pkg/queue 包注释。
# queue:
#   addr: "127.0.0.1:6379"
#   password: "${REDIS_PASSWORD}"
#   concurrency: 10
#   pool_size: 20 # 并发数远高于池大小会大面积撞 ErrPoolTimeout
