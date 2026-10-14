# __svc__ 服务配置：只放声明式参数；Writer、Extractors 等装配期注入项不在此出现。
# 凭证以带引号的环境变量引用注入（写法见 pkg/conf 包注释），启动时展开、未设置即起不来；明文不入库（宪法第 4 条）。

log:
  name: __svc__
  level: info
  format: json

app:
  stop_timeout: 30s # 停机总超时，须 ≤ 部署侧宽限期；省略即取 pkg/app 的 30s 默认

telemetry:
  exporter: stdout # 本地开发直接打标准输出；生产改 otlp 并配 endpoint（如 collector:4317）
