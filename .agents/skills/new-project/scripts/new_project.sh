#!/usr/bin/env bash
# new_project.sh <module-path> [project-name] — 就地把一份模板 clone 改造成新项目。
#
# 做四件事:换 module path、抹掉模板身份(仓库名散落在文档标题与技能描述里)、
# 生成新项目的 README、重置 git 历史并把模板仓挂成 upstream。
#
# 顺序有意为之:先改文件再自检,自检全绿才动 git——动 git 之前的任何失败都能
# 靠 git 恢复工作树(前置检查已要求工作树干净),回滚因此是平凡的。
set -euo pipefail

# 模板自身的 module path。本脚本只在「还是模板」的仓库里跑,靠它判断。
readonly TEMPLATE_MODULE="github.com/yanking/go-skeleton"

module="${1:?用法: new_project.sh <module-path> [project-name]，如 github.com/acme/acme-pay}"
[[ "$module" =~ ^[a-z0-9][a-z0-9.-]*(\.[a-z]{2,})?(/[A-Za-z0-9._-]+)+$ ]] || {
	echo "module path 须形如 host/owner/repo，如 github.com/acme/acme-pay：${module}" >&2
	exit 1
}

# 项目名默认取 module path 末段;主版本后缀(/v2)不是仓库名,再往前取一段。
name="${2:-}"
if [ -z "$name" ]; then
	name="${module##*/}"
	if [[ "$name" =~ ^v[0-9]+$ ]]; then
		rest="${module%/*}"
		name="${rest##*/}"
	fi
fi
if ! [[ "$name" =~ ^[a-z][a-z0-9-]*$ ]]; then
	if [ -n "${2:-}" ]; then
		echo "项目名须为小写 kebab-case: ${name}" >&2
	else
		# 名字是从 module path 推出来的,别把用户没输入的东西报回去——指出来源并给出改法。
		echo "从 module path 末段推导出的项目名不是小写 kebab-case: ${name}" >&2
		echo "项目名用于文档标题与 README。显式指定即可:" >&2
		echo "  make new-project MODULE=${module} NAME=<小写 kebab-case 名>" >&2
	fi
	exit 1
fi

root="$(cd "$(dirname "$0")/../../../.." && pwd)"
cd "$root"

# —— 前置检查:每条都给出可执行的下一步,不留「不知道该怎么办」的失败 ——
[ -f go.mod ] && [ -f AGENTS.md ] && [ -d pkg/app ] || {
	echo "当前目录不像 go-skeleton 仓库根(缺 go.mod / AGENTS.md / pkg/app)：${root}" >&2
	exit 1
}
grep -q "^module ${TEMPLATE_MODULE}\$" go.mod || {
	echo "本目录不是 go-skeleton 模板仓(module 是 $(sed -n 's/^module //p' go.mod))。" >&2
	echo "请先 clone 模板仓(默认分支 main 即模板层),再在那份 clone 里跑本命令。" >&2
	exit 1
}
[ "$module" != "$TEMPLATE_MODULE" ] || { echo "新 module path 不能与模板相同：${module}" >&2; exit 1; }
if [ -d cmd ] && [ -n "$(ls -A cmd 2>/dev/null)" ]; then
	echo "cmd/ 下已有服务($(ls cmd | tr '\n' ' ')),这不是一份干净的模板。" >&2
	echo "请 clone 模板仓的默认分支 main(零业务服务),别用带示例服务的 example 分支。" >&2
	exit 1
fi
git rev-parse --git-dir >/dev/null 2>&1 || { echo "当前目录不是 git 仓库,无法安全回滚,拒绝改造" >&2; exit 1; }
[ -z "$(git status --porcelain)" ] || {
	echo "工作树有未提交改动,拒绝改造(改造是全仓范围的,需要干净的回滚基线):" >&2
	git status --short >&2
	exit 1
}
git config user.email >/dev/null 2>&1 || { echo "git 身份未配置,首提交会失败。先设 git config user.name / user.email" >&2; exit 1; }

# 上游地址要在删 .git 之前取,之后就没了。
upstream_url="$(git remote get-url origin 2>/dev/null || true)"
# README 里要放可点的链接:SSH 形态(git@host:owner/repo.git)转成 https。
upstream_link="$upstream_url"
case "$upstream_link" in
git@*)
	# 先把 host:owner 的冒号换成斜杠,再加 scheme——顺序反了会改到 https:// 自己的冒号。
	upstream_link="${upstream_link#git@}"
	upstream_link="https://${upstream_link/://}"
	;;
esac
upstream_link="${upstream_link%.git}"

# 改造阶段任何失败都把工作树还原到改造前——.git 尚在,这一步是可靠的。
restored=0
rollback() {
	[ "$restored" = 1 ] && return
	git checkout -- . 2>/dev/null || true
	git clean -fdq 2>/dev/null || true
	echo "改造失败,工作树已还原到改造前" >&2
}
trap rollback EXIT

# subst 就地按 sed 表达式改写文件。不用 sed -i:GNU 与 BSD 的 -i 语义不同,
# 模板要能在两种环境下跑。
subst() {
	local f="$1"
	shift
	sed "$@" "$f" >"$f.tmp" && mv "$f.tmp" "$f"
}

echo ">> 1/5 换 module path：${TEMPLATE_MODULE} → ${module}"
# 本脚本自身不参与替换:TEMPLATE_MODULE 记的是上游模板的身份,不是本项目的 import 路径。
# 连它一起改掉,派生项目里「已改造过就拒绝」的护栏就会拿新 module 跟自己比,永远通过
# ——同一个仓能被反复改造。
readonly self_path="./.agents/skills/new-project/scripts/new_project.sh"
while IFS= read -r f; do
	if [ "$f" != "$self_path" ]; then
		subst "$f" -e "s#${TEMPLATE_MODULE}#${module}#g"
	fi
done < <(grep -rl --binary-files=without-match "$TEMPLATE_MODULE" . --exclude-dir=.git)
gofmt -w . # 本仓 import 自成一组,重命名本不该引发漂移;这一步是保险

echo ">> 2/5 抹掉模板身份"
subst CONSTITUTION.md -e "1s#.*#\# ${name} 项目宪法#"
subst Makefile -e "1s#.*#\# ${name} 开发常用命令。#"
subst .agents/skills/new-service/SKILL.md -e "s#在 go-skeleton 仓库新增微服务#在 ${name} 仓库新增微服务#"
subst .agents/skills/new-service/assets/openapi/docs.html -e "s#go-skeleton API 文档#${name} API 文档#g"
# 派生项目不再是模板仓:定位改写,分支约定去掉 template 一行。
subst AGENTS.md \
	-e "s#^它首先是\*\*模板仓库\*\*.*#本仓基于 go-skeleton 模板骨架起步。模板层(\`pkg/\`、\`Makefile\`、\`.agents/\`)的改动仍要经得起一个问题——\"这个模式复制到第 10 个服务时还成立吗?\"#" \
	-e "/^拿本仓起新项目用/d" # 派生项目不是模板仓,这条动线不再适用(new-project 技能留着,跑到会自己拒绝)
branch_table=$(
	cat <<'TABLE'
分支约定:

| 分支 | 内容 |
|---|---|
| `main` | 主干:模板层 + 已完工服务 |
| `feature/*` | 在建服务,业务层允许是半成品;模板层改动照样受"第 10 个服务"检验 |
TABLE
)
awk -v repl="$branch_table" '
	/^分支约定\(/ { print repl; print ""; skipping = 1; next }
	/^文档分布:/  { skipping = 0 }
	!skipping
' AGENTS.md >AGENTS.md.tmp && mv AGENTS.md.tmp AGENTS.md

echo ">> 3/5 生成 README.md"
# 上游那段随「有没有 origin」两说——没有 remote 却宣称已挂 upstream,README 就是在撒谎。
if [ -n "$upstream_url" ]; then
	upstream_section="本仓基于 [go-skeleton](${upstream_link}) 模板骨架起步,已挂为 \`upstream\` remote。
模板层有改进时 \`git fetch upstream\` 后手工挑——module path 已改,不要直接 merge。"
else
	upstream_section="本仓基于 go-skeleton 模板骨架起步。这份模板是从无 remote 的副本(压缩包等)拿到的,
上游地址未记录;要跟进模板层改进,自行 \`git remote add upstream <模板仓地址>\`,
之后 \`git fetch upstream\` 手工挑——module path 已改,不要直接 merge。"
fi
cat >README.md <<EOF
# ${name}

Go 微服务 monorepo。契约先行(proto + gRPC/grpc-gateway),结构化日志与可观测性开箱即用。

> 一句话定位待补:这个项目解决什么问题、服务谁。

## 快速开始

\`\`\`sh
make help    # 全部可用目标
make check   # 提交门槛:build / vet / test 全绿 + gofmt 零漂移
\`\`\`

新增服务用 \`.agents/skills/new-service\` 技能渲染入口模板,不要手抄:

\`\`\`sh
bash .agents/skills/new-service/scripts/new_service.sh <svc> [none|rpc|both]
\`\`\`

跑起来:\`make run SVC=<svc>\`;有 HTTP 出口的服务在 \`/docs\` 看接口文档。

## 文档

| 位置 | 内容 |
|---|---|
| [AGENTS.md](AGENTS.md) | 项目导航与约束入口(给 AI 助手与新人),含分层约定与开发动线 |
| [CONSTITUTION.md](CONSTITUTION.md) | 仓库最高约束,与其冲突的一律以它为准 |
| \`.agents/go-style.md\` | 编码规范 |
| \`docs/\` | 业务文档 |

## 上游模板

${upstream_section}
EOF

echo ">> 4/5 自检(make check)"
make check

echo ">> 5/5 重置 git 历史"
restored=1 # 自检已过,后面不再回滚工作树
rm -rf .git
git init -q -b main
git add -A
git commit -q -F - <<EOF
init: ${name} 项目骨架

基于 go-skeleton 模板骨架起步,module ${module}。

模板层:pkg/ 公共包(生命周期、配置、日志、错误码、遥测、存储、传输、任务队列)、
Makefile 工具链、buf 契约工具链、.agents/ 约束与技能。零业务服务。
EOF
if [ -n "$upstream_url" ]; then
	git remote add upstream "$upstream_url"
fi

echo
echo "已改造为 ${name}(module ${module}),git 历史重置为单提交 main。"
[ -n "$upstream_url" ] && echo "模板仓已挂为 upstream：${upstream_url}"
echo "下一步："
echo "  1) 补 README.md 的一句话定位,按需删掉 pkg/ 里用不到的包(如只用 pgsql 就删 mysql)"
echo "  2) git remote add origin <你的新仓地址> && git push -u origin main"
echo "  3) 用 new-service 技能起第一个服务：bash .agents/skills/new-service/scripts/new_service.sh <svc> [none|rpc|both]"
