# go-skeleton 开发常用命令。
# 新增服务只须加 cmd/<svc>/ 与 configs/<svc>.yaml,本文件无须改。

BIN_DIR := bin
# 工具链装在项目内而非 GOPATH/bin:版本随 tools/go.mod 的 tool 段走,不受各人全局环境影响。
# 声明与二进制分开放:tools/ 是独立 module(只有 go.mod/go.sum,入库),
# bin/tools/ 是装出来的二进制(产物,随 make clean 一并清掉)。
TOOLS_DIR := $(CURDIR)/$(BIN_DIR)/tools
TOOLS_MOD := $(CURDIR)/tools
# 自动发现 cmd/ 下的全部服务;SVC 未显式给定时取第一个(零服务时为空,相关目标会给指引)。
CMDS := $(notdir $(wildcard cmd/*))
SVC ?= $(firstword $(CMDS))

.DEFAULT_GOAL := help
.PHONY: help new-project build run vet test check lint vuln fmt tidy tools proto proto-lint migrate-create migrate-up migrate-down migrate-status e2e clean

help: ## 显示帮助
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

new-project: ## 用本模板起新项目(只在模板 clone 上跑一次): make new-project MODULE=github.com/acme/acme-pay [NAME=acme-pay]
	@test -n "$(MODULE)" || { echo ">> 缺 MODULE,如:make new-project MODULE=github.com/acme/acme-pay"; exit 1; }
	@bash .agents/skills/new-project/scripts/new_project.sh "$(MODULE)" $(NAME)

build: ## 编译全部服务到 bin/
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo ">> build $$cmd"; \
		go build -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit 1; \
	done

run: ## 本地运行服务(默认 cmd/ 下首个):make run 或 make run SVC=<name>
	@test -n "$(SVC)" -a -d "cmd/$(SVC)" || { echo ">> 服务不存在。用法: make run SVC=<name>;现有: $(if $(CMDS),$(CMDS),无——先用 new-service 技能生成)"; exit 1; }
	go run ./cmd/$(SVC)

vet: ## 静态检查 go vet ./...
	go vet ./...

test: ## 全量测试 go test ./...
	go test ./...

# 四步各占一行:make 逐行执行、非零即停,失败时报出的就是失败的那一步。
# 不要合成 `a && b && c || d` ——那种写法会把任何一步的失败都归因到最后的 d。
check: ## 提交门槛(宪法第 5 条):build/vet/test 全绿 + gofmt 零漂移
	go build ./...
	go vet ./...
	go test ./...
	@drift=$$(gofmt -l .); test -z "$$drift" || { echo "$$drift"; echo ">> 存在未格式化文件,运行 make fmt"; exit 1; }

# lint / vuln 不并进 check:宪法第 5 条把提交门槛定义为 build/vet/test + gofmt,
# 改它的语义属于宪法修订(须单独提交、单独评审)。二者在 CI 里各占一个 job。
lint: tools ## golangci-lint 全量检查(linter 选取与例外见 .golangci.yml)
	$(TOOLS_DIR)/golangci-lint run ./...

vuln: tools ## 依赖漏洞扫描(govulncheck,比对 Go 官方漏洞库)
	$(TOOLS_DIR)/govulncheck ./...

fmt: ## 格式化全部 Go 代码
	gofmt -w .

tidy: ## 整理依赖 go mod tidy(主 module 与 tools/ 各一次)
	go mod tidy
	cd $(TOOLS_MOD) && go mod tidy

tools: ## 安装/更新工具链(proto、goose、golangci-lint、govulncheck)到 bin/tools/(版本钉在 tools/go.mod 的 tool 段)
	@mkdir -p $(TOOLS_DIR)
	@cd $(TOOLS_MOD) && for pkg in $$(go list tool); do \
		bin=$$(basename $$pkg); \
		if [ -x "$(TOOLS_DIR)/$$bin" ]; then echo ">> 更新 $$bin"; else echo ">> 安装 $$bin"; fi; \
		GOBIN=$(TOOLS_DIR) go install "$$pkg" || exit 1; \
	done

# TOOLS_DIR 放 PATH 最前:项目钉的版本优先于 GOPATH/bin 里可能存在的旧插件。
proto: tools ## 从 api/ 生成 pb、gRPC stub、gateway 与 OpenAPI v3 spec(buf)
	@ls api/*/ >/dev/null 2>&1 || { echo ">> api/ 下还没有契约:按 AGENTS.md「服务开发流程」先写 proto"; exit 1; }
	PATH="$(TOOLS_DIR):$$PATH" buf generate api

# 破坏性变更的比较基线:契约「已发布」的那一版,即当前分支自己的远端。
# 不写死 origin/main——本仓的 main 是纯模板层、根本没有 api/,拿它当基线会得到
# 「一切都是新增、永远不算破坏」的假通过。基线缺失时宁可跳过并说明,也不给假绿。
BREAKING_BASE ?= origin/$(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)

proto-lint: ## buf lint + 破坏性变更检查(基线默认为当前分支的远端,可用 BREAKING_BASE 覆盖)
	@ls api/*/ >/dev/null 2>&1 || { echo ">> api/ 下还没有契约,无可检查"; exit 1; }
	buf lint
	@if git rev-parse --verify --quiet '$(BREAKING_BASE)' >/dev/null; then \
		echo ">> 破坏性变更检查基线: $(BREAKING_BASE)"; \
		buf breaking --against '.git#branch=$(BREAKING_BASE)'; \
	else \
		echo ">> 基线 $(BREAKING_BASE) 不存在(分支尚未推送),跳过破坏性变更检查;"; \
		echo ">> 要比对别的基线: make proto-lint BREAKING_BASE=origin/<分支>"; \
	fi

# 数据库迁移(goose):迁移文件按服务放 migrations/<svc>/,序号统一用 migrate-create 生成的
# 年月日时间戳(手写小序号与其混用会打乱应用顺序)。DSN 默认从 configs/<svc>.yaml 的 pgsql.write
# 提取(值须带引号)——服务与库天然对应;部署侧/CI 必须显式注入 MIGRATE_DSN 环境变量覆盖,防止
# 拿仓库里的开发库地址去打生产。迁移是显式部署步骤,不挂服务启动(多副本抢跑);共库时各配 -table。
GOOSE       := $(TOOLS_DIR)/goose
MIGRATE_DIR := migrations/$(SVC)
MIGRATE_DSN ?= $(shell sed -n '/^pgsql:/,/^[^[:space:]]/ s/^[[:space:]]*write:[[:space:]]*"\([^"]*\)".*/\1/p' configs/$(SVC).yaml 2>/dev/null)

migrate-create: ## 新建迁移文件: make migrate-create SVC=<svc> NAME=create_xxx
	@test -n "$(SVC)" || { echo ">> 无服务可迁移,先用 new-service 技能生成"; exit 1; }
	@test -n "$(NAME)" || { echo ">> 缺 NAME,如:make migrate-create SVC=$(SVC) NAME=create_xxx"; exit 1; }
	@test -x "$(GOOSE)" || $(MAKE) --no-print-directory tools
	$(GOOSE) -dir $(MIGRATE_DIR) create $(NAME) sql

migrate-up: ## 应用全部待迁移: make migrate-up SVC=user(DSN 默认抄 configs/<svc>.yaml 的 pgsql.write)
	@test -n "$(MIGRATE_DSN)" || { echo ">> 缺 MIGRATE_DSN:默认取 configs/$(SVC).yaml 的 pgsql.write(该服务无 pgsql 段或值未加引号时取不到),也可显式传入"; exit 1; }
	@test -x "$(GOOSE)" || $(MAKE) --no-print-directory tools
	$(GOOSE) -dir $(MIGRATE_DIR) pgx "$(MIGRATE_DSN)" up

migrate-down: ## 回滚最近一条迁移: make migrate-down SVC=user
	@test -n "$(MIGRATE_DSN)" || { echo ">> 缺 MIGRATE_DSN:默认取 configs/$(SVC).yaml 的 pgsql.write(该服务无 pgsql 段或值未加引号时取不到),也可显式传入"; exit 1; }
	@test -x "$(GOOSE)" || $(MAKE) --no-print-directory tools
	$(GOOSE) -dir $(MIGRATE_DIR) pgx "$(MIGRATE_DSN)" down

migrate-status: ## 查看迁移状态: make migrate-status SVC=user
	@test -n "$(MIGRATE_DSN)" || { echo ">> 缺 MIGRATE_DSN:默认取 configs/$(SVC).yaml 的 pgsql.write(该服务无 pgsql 段或值未加引号时取不到),也可显式传入"; exit 1; }
	@test -x "$(GOOSE)" || $(MAKE) --no-print-directory tools
	$(GOOSE) -dir $(MIGRATE_DIR) pgx "$(MIGRATE_DSN)" status

# 端对端测试(grpcurl 打真实进程):脚本按服务放 test/e2e/<svc>/run.sh,自带迁移、
# 测试数据写入、被测服务与下游 mock 的起停。SVC 与 run 一致(自动取首个服务)。
e2e: ## 端对端测试: make e2e SVC=<svc>(前置:本地 Postgres、grpcurl、jq)
	@test -x test/e2e/$(SVC)/run.sh || { echo ">> 暂无 $(SVC) 的端对端测试(test/e2e/$(SVC)/run.sh 不存在)"; exit 1; }
	@test -x "$(GOOSE)" || $(MAKE) --no-print-directory tools
	test/e2e/$(SVC)/run.sh

clean: ## 清理构建产物
	rm -rf $(BIN_DIR)
