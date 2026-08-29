---
name: new-project
description: 用 go-skeleton 模板起一个全新项目：一条命令换 module path、抹掉模板身份（散落在文档标题与技能描述里的仓库名）、生成 README、重置 git 历史并把模板挂成 upstream，随后按清单补项目定位与第一个服务。用户说「用这个模板起个新项目 / 基于 go-skeleton 建个 xxx / 初始化一个新仓库 / bootstrap 一个新项目 / 拿这个骨架起个 xxx」时使用。
---

# 从模板起新项目

改造是全仓范围的（module path 出现在 30 多处，仓库名还散在文档标题、技能描述、
`/docs` 页标题里），手工做必漏；脚本一次做完并自检。

## 前置：先拿到一份干净的模板

```bash
git clone <模板仓地址> <项目目录>
cd <项目目录>
```

默认分支 `main` 就是零业务服务的纯模板层，直接 clone 即可。若误克隆了 `example`
（带三个示例服务），脚本会拒绝改造。

## 一条命令改造

```bash
make new-project MODULE=github.com/acme/acme-pay [NAME=acme-pay]
```

- `MODULE` 是新项目的 Go module path，形如 `host/owner/repo`。
- `NAME` 是项目名，默认取 `MODULE` 末段（`/v2` 这类主版本后缀会跳过）；用于文档标题与
  README，须是小写 kebab-case。
- 等价的直接调用：`bash .agents/skills/new-project/scripts/new_project.sh <module> [name]`。

脚本按序做五件事，**自检全绿才动 git**——在那之前任何失败都会把工作树还原：

1. 换 module path（`go.mod`、`tools/go.mod`、全部 `.go` 与技能模板 `.tpl`），随后 `gofmt -w .`
2. 抹掉模板身份：`CONSTITUTION.md` 标题、`Makefile` 首行、`AGENTS.md` 项目定位与分支约定表
   （去掉 `template` 一行，派生项目不再是模板仓）、`new-service` 技能描述、`/docs` 页标题
3. 生成 `README.md`
4. `make check` 自检
5. `rm -rf .git` → `git init -b main` → 首提交，并把原 `origin` 挂成 `upstream`

`errDomain`（业务错误码在 gRPC status details 里的归属域）不在改造清单里——它从
main module path 推导，换了 module 自动跟随。

## 生成后必做（顺序即检查清单）

1. **补项目定位**：`README.md` 顶部留了一行待补的「这个项目解决什么问题、服务谁」；
   `AGENTS.md` 的项目定位段同样值得按实际业务改写。
2. **裁剪公共包**：`pkg/` 是全量模板（两种数据库、Redis、任务队列、出站 HTTP……）。
   用不到的直接删目录并 `make tidy`——留着的每个包都是要维护的面。
3. **接上远端**：`git remote add origin <新仓地址> && git push -u origin main`。
4. **起第一个服务**：用 `new-service` 技能，别手抄骨架。
5. **登记错误码分段**：见 `AGENTS.md`「错误处理约定」，通用码占 10000–19999，业务码从
   20000 起自行分段。

## 约束

- 只在模板 clone 上跑一次。脚本靠「当前 module 是否仍是模板的」判断，改造过的仓库再跑会被拒绝。
- 工作树必须干净——改造覆盖全仓，脚本要靠 git 做回滚基线。
- 不要 `git merge upstream`：module path 已改，逐文件冲突。模板层有改进时 `git fetch upstream`
  后手工挑。
