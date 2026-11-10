# syntax=docker/dockerfile:1

# 服务镜像：多阶段构建，运行层只有一个静态二进制。
# 用 `make docker SVC=<svc> [TAG=<tag>]` 构建——它会把 Go 版本与服务名一并传进来。
#
# 配置不打进镜像：同一个镜像要能跑所有环境，配置由部署侧挂载（宪法第 4 条
# 「不同环境下发不同文件」）。打进镜像就等于每个环境一个镜像，而两个镜像的
# 差异除了配置还包含构建时刻的一切，追起来是另一回事。挂载方式见 docs/deploy.md。

# GO_VERSION 由 Makefile 从 go.mod 提取后传入（make docker 总会传）。
# 这里的默认值只是给「直接 docker build 不经 Makefile」留的兜底，它确实是第二个
# 来源、会漂移——所以请走 make docker。
ARG GO_VERSION=1.27.3

# ---- 构建 ----
FROM golang:${GO_VERSION}-alpine AS builder

# SVC 指定构建哪个服务（对应 cmd/<SVC>），必须显式给。
ARG SVC
WORKDIR /src

# 先只拷依赖清单：改业务代码不会让 go mod download 这层缓存失效。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN test -n "${SVC}" || { echo ">> 缺 --build-arg SVC=<服务名>"; exit 1; }
RUN test -d "cmd/${SVC}" || { echo ">> cmd/${SVC} 不存在"; exit 1; }

# CGO_ENABLED=0 静态编译，运行层才能用 distroless/static（本仓的 DB 与 Redis
# 驱动都是纯 Go，不需要 cgo）。-trimpath 去掉构建机的绝对路径，
# -ldflags="-s -w" 去符号表与调试信息，镜像小一截。
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" -o /out/app "./cmd/${SVC}"

# ---- 运行 ----
# distroless/static：没有 shell、没有包管理器、以非 root 跑，攻击面最小。
# 代价是进不去容器排障——要 exec 进去看文件时，把这一行换成 alpine:3 并
# 显式 `USER 65532:65532`（别顺手跑成 root）。
FROM gcr.io/distroless/static:nonroot

# 证书：出站 HTTPS（三方接口、OTLP over TLS）要它，distroless/static 自带一份。
WORKDIR /
COPY --from=builder /out/app /app

USER nonroot:nonroot

# 配置路径与部署侧的挂载点约定一致，见 docs/deploy.md。
ENTRYPOINT ["/app"]
CMD ["-config", "/etc/service/config.yaml"]
