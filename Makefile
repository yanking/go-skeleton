# 命令契约的唯一载体，语义定义见 .agent/engineering.md「命令契约（Makefile）」。
# 工具版本在此钉死，所有开发者与 CI 用同一版本；升级须同步该文档的「工具链」表。
SHELL := /bin/bash
.DEFAULT_GOAL := all

BUF_VERSION           := v1.72.0
GOLANGCI_LINT_VERSION := v2.13.1
MIGRATE_VERSION       := v4.19.1
SQLC_VERSION          := v1.31.1
# 阅读器版本的唯一事实源是 pkg/transport/http.go 的 scalarVersion：本地离线页与服务端
# /docs 必须渲染自同一版本，两处各写一份必然漂移。读不到时 make docs 会带提示失败。
SCALAR_VERSION        := $(shell sed -n 's/^const scalarVersion = "\(.*\)"$$/\1/p' pkg/transport/http.go)

# bin/ 只放 make build 出来的服务二进制（能直接拷进镜像的东西）；
# tools/ 放钉版本的工具链与缓存资源（纯本地开发产物）。两者都不入库。
BIN   := $(CURDIR)/bin
TOOLS := $(CURDIR)/tools
# 版本号进文件名：改了版本号即换了目标路径，make 会自动重装，不会用着旧的还以为是新的。
BUF     := $(TOOLS)/buf-$(BUF_VERSION)
LINT    := $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)
MIGRATE := $(TOOLS)/migrate-$(MIGRATE_VERSION)
SQLC    := $(TOOLS)/sqlc-$(SQLC_VERSION)
SCALAR  := $(TOOLS)/scalar-$(SCALAR_VERSION).js

# make docs 的输入与产物。API_VERSION 可覆盖：make docs SERVICE=user API_VERSION=v2
API_VERSION ?= v1
SPEC      = openapi/$(SERVICE)/$(API_VERSION)/$(SERVICE).openapi.json
DOCS_DIR := $(TOOLS)/docs
DOCS_PAGE = $(DOCS_DIR)/$(SERVICE)-$(API_VERSION).html

$(BUF):
	@mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	@mv $(TOOLS)/buf $@

$(LINT):
	@mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@mv $(TOOLS)/golangci-lint $@

$(MIGRATE):
	@mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install -tags 'mysql postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)
	@mv $(TOOLS)/migrate $@

$(SQLC):
	@mkdir -p $(TOOLS)
	GOBIN=$(TOOLS) go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	@mv $(TOOLS)/sqlc $@

# 接口文档阅读器：单文件 3.8MB，首次执行时下载并按版本缓存，之后完全离线。
# 与 pkg/transport 的 /docs 用同一版本，本地看到的和服务端渲染的是同一个东西。
$(SCALAR):
	@mkdir -p $(TOOLS)
	curl -fsSL -o $@ https://cdn.jsdelivr.net/npm/@scalar/api-reference@$(SCALAR_VERSION)/dist/browser/standalone.js

## init: 安装 / 校验钉死版本的工具链
.PHONY: init
init: $(BUF) $(LINT) $(MIGRATE) $(SQLC)
	@echo "工具链就绪：$(TOOLS)"

## proto-deps: 拉取 / 更新 buf.yaml 声明的 proto 依赖，产出 buf.lock
.PHONY: proto-deps
proto-deps: $(BUF)
	$(BUF) dep update

## api: 校验并重新生成协议层产物（pb、gateway、openapi）
.PHONY: api
api: $(BUF)
	$(BUF) lint
	$(BUF) generate

## breaking: 对 main 基线做破坏性变更检查（宪法第六条）
.PHONY: breaking
breaking: $(BUF)
	$(BUF) breaking --against '.git#branch=main'

## sql: 校验并重新生成 data 层的 SQL 访问代码（sqlc）
.PHONY: sql
sql: $(SQLC)
	$(SQLC) vet
	$(SQLC) generate

## build: 编译全部 cmd/* 到 bin/
.PHONY: build
build:
	@mkdir -p $(BIN)
	@if compgen -G "cmd/*/" > /dev/null; then \
		go build -o $(BIN)/ ./cmd/...; \
	else \
		echo "(尚无 cmd/，跳过 build)"; \
	fi

## run: 起指定服务，读 configs/<SERVICE>.yaml
.PHONY: run
run: guard-SERVICE
	go run ./cmd/$(SERVICE) -conf configs/$(SERVICE).yaml

## docs: 生成可离线打开的接口文档页；不起服务、不连数据库
.PHONY: docs
docs: guard-SERVICE $(SCALAR)
	@test -n "$(SCALAR_VERSION)" || { echo "读不到 pkg/transport/http.go 的 scalarVersion 常量"; exit 1; }
	@test -f $(SPEC) || { echo "找不到 $(SPEC)，先跑 make api"; exit 1; }
	@mkdir -p $(DOCS_DIR)
	@cp -f $(SCALAR) $(DOCS_DIR)/
	@{ \
		echo '<!doctype html>'; \
		echo '<html lang="zh"><head><meta charset="utf-8">'; \
		echo '<title>$(SERVICE) $(API_VERSION) 接口文档</title></head><body>'; \
		echo '<script id="api-reference" type="application/json"' \
		     'data-configuration="{&quot;withDefaultFonts&quot;:false}">'; \
		cat $(SPEC); \
		echo '</script>'; \
		echo '<script src="scalar-$(SCALAR_VERSION).js"></script>'; \
		echo '</body></html>'; \
	} > $(DOCS_PAGE)
	@echo "接口文档已生成，用浏览器打开：file://$(DOCS_PAGE)"
	@echo "（内联 spec ＋ 本地阅读器，零外网请求；withDefaultFonts 关掉的是 Scalar 的在线字体）"

## test: 单元测试；需要真实中间件的用例在未配 DSN 时自动跳过
.PHONY: test
test:
	go test -race ./...

## test-integration: 带数据库跑全量，DSN 见 .agent/engineering.md「集成测试」
.PHONY: test-integration
test-integration: guard-MYSQL_DSN guard-POSTGRES_DSN
	go test -race -count=1 ./...

## lint: 格式检查（含 goimports）＋ 静态检查
.PHONY: lint
lint: $(LINT)
	$(LINT) fmt --diff
	$(LINT) run

## fmt: 就地格式化（gofmt + goimports）
.PHONY: fmt
fmt: $(LINT)
	$(LINT) fmt

## migrate-up / migrate-down: 对指定服务执行 / 回滚迁移
.PHONY: migrate-up
migrate-up: $(MIGRATE) guard-SERVICE guard-DATABASE_URL
	$(MIGRATE) -path migrations/$(SERVICE) -database "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: $(MIGRATE) guard-SERVICE guard-DATABASE_URL
	$(MIGRATE) -path migrations/$(SERVICE) -database "$(DATABASE_URL)" down 1

## all: api → sql → breaking → lint → test → build
.PHONY: all
all: api sql breaking lint test build

# guard-X：X 未设置就带着用法提示失败，而不是执行到一半才出错
guard-%:
	@if [ -z "$($*)" ]; then \
		echo "缺少参数 $*，例：make $(firstword $(MAKECMDGOALS)) $*=<值>"; \
		exit 1; \
	fi

## help: 列出全部目标
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
