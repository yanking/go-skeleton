#!/usr/bin/env bash
# new_module.sh <svc> <module> — 在已按模块分目录的服务里新增一个业务模块的骨架。
# 在 internal/<svc>/<module>/ 下渲染 service/repo/model/job 的 doc.go;configs/<svc>.yaml 里有
# 以 transport: 开头的行(即有传输段)时,再渲染 handler/doc.go。
# 包注释模板复用 new-service 的 ../../new-service/assets/,__scope__ 的取值同 new_service.sh 分模块时:
# " <svc> 服务的 <module> 模块"(自带前导空格,缘由见 new_service.sh 头部注释)。
# 服务还是平铺布局时拒绝:先按 AGENTS.md「服务分层」把现有代码迁成模块。
set -euo pipefail

usage="用法: new_module.sh <svc> <module>，如 mall order"
svc="${1:?$usage}"
module="${2:?$usage}"
# 模块名规则同 new_service.sh(两处各自内联,改时一起改):小写字母开头的字母数字串;
# 与层名重名会被 test/arch 当成平铺层,与 config 重名会撞上服务级配置包;
# testdata、vendor、internal 是 Go 工具会特殊对待的目录名:./... 不匹配 testdata 与 vendor(test/arch
# 也跳过 testdata),放进去的模块 make check 看不见;路径带 vendor 元素的包引用即被拒,带 internal
# 元素的包只许其父目录之内引用,cmd/<svc>/initial 在其外,模块接不了线。
[[ "$module" =~ ^[a-z][a-z0-9]*$ ]] || { echo "模块名须为小写字母开头的字母数字串: ${module}" >&2; exit 1; }
case "$module" in
	config|handler|service|repo|model|job) echo "模块名不得与层名(handler/service/repo/model/job)或 config 重名: ${module}" >&2; exit 1 ;;
	testdata|vendor|internal) echo "模块名不能用 testdata、vendor、internal,Go 工具会特殊对待这三个目录名: ${module}" >&2; exit 1 ;;
esac

root="$(cd "$(dirname "$0")/../../../.." && pwd)"
cd "$root"
if [ ! -d "internal/${svc}" ] || [ ! -f "configs/${svc}.yaml" ]; then
	echo "服务 ${svc} 不存在：internal/${svc} 与 configs/${svc}.yaml 都要在，先用 new-service 技能起服务" >&2
	exit 1
fi
for layer in handler service repo model job; do
	if [ -d "internal/${svc}/${layer}" ]; then
		echo "服务 ${svc} 是平铺布局：先按 AGENTS.md「服务分层」把现有代码迁成模块，再加新模块" >&2
		exit 1
	fi
done
dir="internal/${svc}/${module}"
if [ -e "$dir" ]; then
	echo "${dir} 已存在，拒绝覆盖" >&2
	exit 1
fi
assets=".agents/skills/new-service/assets"

# 渲染中途任何失败都回滚本次新建的模块目录,不留半成品(写法同 new_service.sh:EXIT trap + 完成标志)。
# trap 必须放在上面的存在性校验之后:模块已存在而被拒绝时,绝不能把已有的模块删掉。
render_done=0
cleanup() {
	[ "$render_done" = 1 ] && return
	rm -rf "$dir"
	echo "渲染失败,已回滚 ${dir}" >&2
}
trap cleanup EXIT

# 这五个包注释模板只含 __scope__ 占位(服务名已折进 scope 里),所以只替换它。
scope=" ${svc} 服务的 ${module} 模块"
render() { sed -e "s/__scope__/$scope/g" "$1" > "$2"; }

layers="service/repo/model/job"
mkdir -p "${dir}/service" "${dir}/repo" "${dir}/model" "${dir}/job"
render "$assets/service.doc.go.tpl" "${dir}/service/doc.go"
render "$assets/repo.doc.go.tpl"    "${dir}/repo/doc.go"
render "$assets/model.doc.go.tpl"   "${dir}/model/doc.go"
render "$assets/job.doc.go.tpl"     "${dir}/job/doc.go"
# none 变体的 yaml 里 transport 只是注释行(不以 transport: 开头),没有协议出口,也就没有 handler。
if grep -q '^transport:' "configs/${svc}.yaml"; then
	mkdir -p "${dir}/handler"
	render "$assets/handler.doc.go.tpl" "${dir}/handler/doc.go"
	layers="${layers}/handler"
fi

render_done=1
echo "已生成 ${dir}（${layers}）"
echo "后续：1) AGENTS.md 错误码表本服务那一行,给 ${module} 模块划一个未用的千位子段  2) 写 proto 契约(服务有传输段时)、填业务实体(各层 doc.go 有指引)  3) 在 cmd/${svc}/initial 的 createServer 里接线  4) 建表、make check 等见 .agents/skills/new-module/SKILL.md"
