# 部署参考

> 面向把本仓服务部署到容器编排环境的人。这里只放**与骨架实现强相关、配错就出事**
> 的那些点——通用的 K8s 用法不在此列。示例用 K8s 清单表达，别的编排系统照搬概念即可。

## 镜像

```sh
make docker SVC=<svc> [TAG=<tag>]   # 等价于 docker build --build-arg SVC=<svc> ...
```

Go 版本由 Makefile 从 `go.mod` 提取后传入，不在 Dockerfile 里写死。运行层是
`distroless/static:nonroot`：无 shell、无包管理器、非 root。要 `kubectl exec` 进去
排障就把那一行换成 `alpine:3` 并显式 `USER 65532:65532`。

**配置不打进镜像**：同一个镜像要能跑所有环境（宪法第 4 条「不同环境下发不同文件」）。
容器默认读 `/etc/service/config.yaml`，由 ConfigMap 挂载到那里。

## 探针

骨架给了三个探针出口，语义不能混用：

| 出口 | 语义 | 行为 | 用途 |
|---|---|---|---|
| `GET /healthz` | liveness | **恒 200**，连停机期也是 | 进程还活着吗 |
| `GET /readyz` | readiness | 停机时转 503；`WithReadinessCheck` 注入的依赖检查失败也转 503，响应体带原因 | 现在能接流量吗 |
| gRPC `grpc.health.v1.Health` | readiness（gRPC 侧） | 停机时置 `NOT_SERVING` | 同上，给 gRPC 客户端的健康检查负载均衡用 |

两条纪律：

- **liveness 不要指向 `/readyz`**。依赖挂了让 liveness 转红 → 编排系统重启进程 →
  重启治不好一个挂掉的数据库，只会把实例打进 CrashLoop，还顺带丢掉所有在途请求。
- **readiness 要注入依赖检查**。`pkg/mysql`、`pkg/pgsql`、`pkg/redis` 都刻意不在装配期
  ping（DB 比进程晚就绪是常态，装配期 ping 会让部署顺序变成硬约束），代价由 readiness
  兜住：

  ```go
  transport.WithReadinessCheck(func(ctx context.Context) error {
      return db.WithContext(ctx).Exec("SELECT 1").Error
  })
  ```

  不注入的话，连不上库的实例照样通过就绪探针，编排系统继续给它发流量，每个请求都 500。

## 宽限期预算（最容易配错的一处）

停机是**串行相加**的两段，部署侧的宽限期要装得下两段之和再留余量：

```
terminationGracePeriodSeconds  ≥  drain_delay  +  stop_timeout  +  余量
                                  （transport）   （app）         （收尾日志、遥测 flush、进程退出）
```

| 参数 | 位置 | 默认 | 作用 |
|---|---|---|---|
| `transport.drain_delay` | `configs/<svc>.yaml` | 5s | 把就绪探针置红后等多久再关监听，给 LB 摘流留时间 |
| `app.stop_timeout` | 同上 | 25s | 全部组件逆序停止的总预算，按剩余组件数均分 |
| `terminationGracePeriodSeconds` | Pod spec | K8s 默认 30s | **不改的话会把上面两段挤爆**：5+25 已经用满 30s，收尾日志与遥测 flush 全丢 |

按默认值配，宽限期至少给 **40s**。

**为什么需要 `drain_delay`**：收到 TERM 后立刻关监听，而 LB 与 kube-proxy 摘流依赖
Endpoint 更新传播，需要数百毫秒到数秒。那段窗口里到达的新连接拿到的是 TCP 层的
`connection refused`（不是 503），客户端的重试策略往往不覆盖它——表现为每次滚动更新
都丢一批请求，而且报在客户端侧，很难归因到发布。

`preStop` 钩子是另一种做法（在容器收到 TERM 之前先 sleep）。两者取其一就够，
都配等于等两次：

```yaml
lifecycle:
  preStop:
    exec:
      command: ["/bin/sleep", "5"]   # distroless 没有 sleep，要用这条就得换基础镜像
```

骨架默认用 `drain_delay`（在进程内，不依赖基础镜像里有 shell）。用 `preStop` 的话把
`drain_delay` 配成 `-1s`（不等）。

## 退出码

| 码 | 含义 | 告警建议 |
|---|---|---|
| 0 | 正常停机 | — |
| 1 | 运行期出事：组件意外退出，或停机中有组件报错 | 要告警 |
| 2 | 停机未在宽限期内跑完 | 记录即可——这是「慢」不是「崩」 |
| 130 | 收到第二个信号，放弃排空 | 人为操作，不告警 |

分开的意义是告警规则能区别对待：一次慢停机不该和进程崩溃触发同一条告警。

## 凭证

yaml 里只写 `${ENV_NAME}` 引用（宪法第 4 条），由部署侧经环境变量注入。
`pkg/conf` 在解析 YAML 之前做纯文本替换，**变量未设置即 panic**——漏配不会被伪装成
空值。`test/secrets/` 有一条测试守着这条规则，它随 `make check` 跑。

```yaml
env:
  - name: DB_PASSWORD
    valueFrom:
      secretKeyRef: { name: <svc>-secrets, key: db-password }
```

已知弱点（选它是因为通用，不是因为它最好）：环境变量能从 `/proc/<pid>/environ`
读到、会被子进程继承、可能出现在崩溃转储里。要更强的隔离就换 Secret 文件挂载 +
`${}` 以外的读取方式，那是一次结构性改动。

## 资源限制

```yaml
resources:
  requests: { cpu: 100m, memory: 128Mi }
  limits:   { cpu: "1",  memory: 512Mi }
env:
  - name: GOMEMLIMIT
    value: "450MiB"   # 略低于 memory limit
```

- **`GOMAXPROCS` 不用管**：Go 1.25 起运行时感知 cgroup 的 CPU limit，自己算对。
- **`GOMEMLIMIT` 要配**：Go 的 GC 默认只看堆增长比例，不知道容器的内存上限，
  于是容器被 OOMKill 时 Go 其实还觉得自己很宽裕。配成略低于 limit，GC 会在逼近时
  加大回收力度，把 OOMKill 换成「变慢」——后者可观测、可扩容，前者是突然死亡。

## 数据库迁移

迁移是**独立的部署步骤**，不挂在服务启动里（多副本会抢跑）：

```sh
MIGRATE_DSN="postgres://..." make migrate-up SVC=<svc>
```

放进 Job 或 initContainer，在新版本 Pod 起来之前跑完。注意多服务共用一库时当前
共用同一张 `goose_db_version`（版本号在全库范围比较），后果与改法见 AGENTS.md
「服务分层」的迁移段。

## 可观测性

| 配置 | 本地开发 | 生产 |
|---|---|---|
| `telemetry.exporter` | `stdout` | `otlp` + `endpoint`（如 `collector:4317`） |
| `telemetry.sampler` | 默认（全采） | `parentbased_traceidratio` + `sample_ratio` |
| `log.level` | `debug` | `info`（运行期可经 `LevelVar` 临时调，不必重启） |
| `log.format` | `text` 肉眼可读 | `json` |

**生产必须改这两项**：`exporter` 留 `stdout` 会把全部 span 与 metric 以 JSON 打进
标准输出（叠加默认的全采样，量按 QPS 线性涨，还会把真日志挤出采集配额）；
`sampler` 留默认全采则是导出带宽与后端存储按 QPS 线性涨。

遥测的 Provider 要显式注入给各组件（`WithTracerProvider` / `WithMeterProvider`、
`pkg/mysql` 等的 `Config.MeterProvider`）——漏注入不报错，只是那部分遥测静默消失，
所以各包在未拿到 Provider 时会记一条「未启用」的装配日志，部署后扫一眼启动日志即可确认。

## 一份最小清单

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: <svc>
spec:
  replicas: 2
  template:
    spec:
      terminationGracePeriodSeconds: 40   # ≥ drain_delay + stop_timeout + 余量
      containers:
        - name: <svc>
          image: <svc>:<tag>
          args: ["-config", "/etc/service/config.yaml"]
          ports:
            - { name: grpc, containerPort: 9090 }
            - { name: http, containerPort: 8080 }
          livenessProbe:
            httpGet: { path: /healthz, port: http }
            periodSeconds: 10
          readinessProbe:
            httpGet: { path: /readyz, port: http }
            periodSeconds: 5
          env:
            - { name: GOMEMLIMIT, value: "450MiB" }
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef: { name: <svc>-secrets, key: db-password }
          volumeMounts:
            - { name: config, mountPath: /etc/service }
          resources:
            requests: { cpu: 100m, memory: 128Mi }
            limits:   { cpu: "1",  memory: 512Mi }
      volumes:
        - name: config
          configMap: { name: <svc>-config, items: [{ key: config.yaml, path: config.yaml }] }
```

纯 gRPC 形态的服务没有 HTTP 端口，探针改用 gRPC：
`grpc: { port: 9090 }`（K8s 1.24+ 原生支持），或 `exec` 调 `grpc_health_probe`。
gRPC 端口与 HTTP 端口各自监听，正是为了让暴露范围、mesh 的协议标注与网络策略分开配。
