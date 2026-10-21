#!/usr/bin/env bash
# new_service.sh <svc> [none|rpc|both] [module] — 渲染服务入口到仓库对应目录。
# variant: none(默认,无传输)| rpc(纯 gRPC)| both(gRPC + gateway 转译的 HTTP,各占一端口)
# module: 可选。服务承载多个业务域(单体,或已知还会加业务域)时给出首个模块名,按模块分目录:
# 五层(handler/service/repo/model/job)生成在 internal/<svc>/<module>/ 下,config 仍在 internal/<svc>/;
# 不给则六件套平铺在 internal/<svc>/ 下(只承载一个业务域的服务)。以后再加模块用 new-module 技能。
# 模板在 ../assets/，占位符 __svc__（服务名）、__Svc__（首字母大写，仅注释用）与 __scope__（包注释里的范围）。
# __scope__ 自带空格，模板里写成「是__scope__的」、不留空格：平铺时是 " <svc> "（前后各一个空格），
# 分模块时是 " <svc> 服务的 <module> 模块"（只有前导空格）。这样英文与中文之间有空格、
# 中文与中文之间没有，渲染出的包注释读起来自然。
set -euo pipefail

svc="${1:?用法: new_service.sh <svc> [none|rpc|both] [module]，如 order rpc}"
variant="${2:-none}"
module="${3:-}"
[[ "$svc" =~ ^[a-z][a-z0-9-]*$ ]] || { echo "服务名须为小写 kebab-case: ${svc}" >&2; exit 1; }
case "$variant" in
	none|rpc|both) ;;
	*) echo "变体须为 none|rpc|both: ${variant}" >&2; exit 1 ;;
esac
# 模块名规则同 new_module.sh(两处各自内联,改时一起改):小写字母开头的字母数字串;
# 与层名重名会被 test/arch 当成平铺层,与 config 重名会撞上服务级配置包;
# testdata、vendor、internal 是 Go 工具会特殊对待的目录名:./... 不匹配 testdata 与 vendor(test/arch
# 也跳过 testdata),放进去的模块 make check 看不见;路径带 vendor 元素的包引用即被拒,带 internal
# 元素的包只许其父目录之内引用,cmd/<svc>/initial 在其外,模块接不了线。
if [ -n "$module" ]; then
	[[ "$module" =~ ^[a-z][a-z0-9]*$ ]] || { echo "模块名须为小写字母开头的字母数字串: ${module}" >&2; exit 1; }
	case "$module" in
		config|handler|service|repo|model|job) echo "模块名不得与层名(handler/service/repo/model/job)或 config 重名: ${module}" >&2; exit 1 ;;
		testdata|vendor|internal) echo "模块名不能用 testdata、vendor、internal,Go 工具会特殊对待这三个目录名: ${module}" >&2; exit 1 ;;
	esac
fi

root="$(cd "$(dirname "$0")/../../../.." && pwd)"
cd "$root"
if [ -e "cmd/${svc}" ] || [ -e "configs/${svc}.yaml" ] || [ -e "internal/${svc}" ]; then
	echo "cmd/${svc}、internal/${svc} 或 configs/${svc}.yaml 已存在，拒绝覆盖" >&2
	exit 1
fi
assets=".agents/skills/new-service/assets"

# 渲染中途任何失败都回滚本次产物,不留半成品(openapi/ 只在本次创建时才回滚)。
# 用 EXIT trap + 完成标志而非 ERR trap:ERR 不会在函数体内的失败上触发(需 errtrace),
# EXIT 则任何路径都兜底。
created_openapi=0
render_done=0
cleanup() {
	[ "$render_done" = 1 ] && return
	rm -rf "cmd/${svc}" "internal/${svc}" "configs/${svc}.yaml"
	[ "$created_openapi" = 1 ] && rm -rf openapi
	echo "渲染失败,已回滚 ${svc} 的全部产物" >&2
}
trap cleanup EXIT

# 分层目录的落点与包注释里的 __scope__ 随布局定;config 两种布局都在 internal/<svc>/config。
if [ -n "$module" ]; then
	layer_dir="internal/${svc}/${module}"
	scope=" ${svc} 服务的 ${module} 模块"
else
	layer_dir="internal/${svc}"
	scope=" ${svc} "
fi

render() { sed -e "s/__svc__/$svc/g" -e "s/__Svc__/$Svc/g" -e "s/__scope__/$scope/g" "$1" > "$2"; }
# shellcheck disable=SC2034  # Svc 供 render 的占位符替换使用
Svc="$(echo "${svc:0:1}" | tr 'a-z' 'A-Z')${svc:1}"

mkdir -p "cmd/${svc}/initial" "internal/${svc}/config" "configs" \
	"${layer_dir}/service" "${layer_dir}/repo" \
	"${layer_dir}/model" "${layer_dir}/job"
render "$assets/main.go.tpl"   "cmd/${svc}/main.go"
render "$assets/config.go.tpl" "internal/${svc}/config/config.go"
render "$assets/service.yaml.tpl" "configs/${svc}.yaml"
# 分层骨架：包注释即契约，各层填什么见渲染出的 doc.go 注释。
render "$assets/service.doc.go.tpl" "${layer_dir}/service/doc.go"
render "$assets/repo.doc.go.tpl"    "${layer_dir}/repo/doc.go"
render "$assets/model.doc.go.tpl"   "${layer_dir}/model/doc.go"
render "$assets/job.doc.go.tpl"     "${layer_dir}/job/doc.go"
if [ "$variant" != "none" ]; then mkdir -p "${layer_dir}/handler"; fi
case "$variant" in
	none) render "$assets/init_app.go.tpl"  "cmd/${svc}/initial/init_app.go" ;;
	rpc)  render "$assets/init_rpc.go.tpl"  "cmd/${svc}/initial/init_app.go"
	      render "$assets/handler.doc.go.tpl" "${layer_dir}/handler/doc.go" ;;
	both) render "$assets/init_both.go.tpl" "cmd/${svc}/initial/init_app.go"
	      render "$assets/handler.doc.go.tpl" "${layer_dir}/handler/doc.go"
	      # openapi 聚合包(spec embed + /docs 阅读页)仓内缺失时一并落地;
	      # 无占位符,原样复制。make proto 产出首个 spec 前该包不编译,按清单顺序先契约后验证。
	      if [ ! -e openapi/openapi.go ]; then
	        created_openapi=1
	        mkdir -p openapi
	        cp "$assets/openapi/openapi.go" openapi/openapi.go
	        cp "$assets/openapi/docs.html" openapi/docs.html
	      fi ;;
esac

# 传输段按变体追加,一份 yaml 只出现一处 transport(模板里不带这段,免得与实段并存):
# none 只留说明性注释,rpc/both 写实段。grpc_addr 是业务协议出口,http_addr 是 gateway 转译出口;
# both 的 http_addr 先注释——模板里 WithGateway 尚未放开,配了端口会在装配期报错。
case "$variant" in
	none) printf '\n# transport: # 本服务无对外出口;要出口就放开本段(整段缺失即无传输组件)\n#   grpc_addr: ":9090" # 业务协议出口,rpc/both 必填\n#   http_addr: ":8080" # gateway 转译出口,both 变体与代码里的 WithGateway 一并放开\n' >> "configs/${svc}.yaml" ;;
	rpc)  printf '\ntransport:\n  grpc_addr: ":9090" # 业务协议出口\n' >> "configs/${svc}.yaml" ;;
	both) printf '\ntransport:\n  grpc_addr: ":9090" # 业务协议出口\n  # http_addr: ":8080" # gateway 转译出口;与 initial 里的 WithGateway 一起放开\n' >> "configs/${svc}.yaml" ;;
esac

layers="service/repo/model/job"
[ "$variant" != "none" ] && layers="${layers}/handler"
render_done=1
if [ -n "$module" ]; then
	echo "已生成 cmd/${svc}、internal/${svc}/config、internal/${svc}/${module}（${layers}）、configs/${svc}.yaml（变体: ${variant}；分模块布局,首个模块: ${module}）"
	echo "后续：1) AGENTS.md 登记错误码分段,并在本服务的段内给 ${module} 模块划一个千位子段  2) 写 proto 契约、填业务实体(各层 doc.go 有指引)  3) make check"
else
	echo "已生成 cmd/${svc}、internal/${svc}（config/${layers}）、configs/${svc}.yaml（变体: ${variant}）"
	echo "后续：1) AGENTS.md 登记错误码分段  2) 写 proto 契约、填业务实体(各层 doc.go 有指引)  3) make check"
fi
if [ "$variant" = both ]; then
	echo "注意：openapi 包要嵌 spec,在 api/${svc} 写好 proto 并跑过 make proto 之前 make check 编译不过,属预期。"
fi
