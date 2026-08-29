#!/usr/bin/env bash
# new_service.sh <svc> [variant] — 渲染服务入口到仓库对应目录。
# variant: none(默认,无传输)| rpc(纯 gRPC)| both(gRPC + gateway 转译的 HTTP,各占一端口)
# 模板在 ../assets/，占位符 __svc__（服务名）与 __Svc__（首字母大写，仅注释用）。
set -euo pipefail

svc="${1:?用法: new_service.sh <svc> [none|rpc|both]，如 order rpc}"
variant="${2:-none}"
[[ "$svc" =~ ^[a-z][a-z0-9-]*$ ]] || { echo "服务名须为小写 kebab-case: ${svc}" >&2; exit 1; }
case "$variant" in
	none|rpc|both) ;;
	*) echo "变体须为 none|rpc|both: ${variant}" >&2; exit 1 ;;
esac

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

render() { sed -e "s/__svc__/$svc/g" -e "s/__Svc__/$Svc/g" "$1" > "$2"; }
# shellcheck disable=SC2034  # Svc 供 render 的占位符替换使用
Svc="$(echo "${svc:0:1}" | tr 'a-z' 'A-Z')${svc:1}"

mkdir -p "cmd/${svc}/initial" "internal/${svc}/config" "configs" \
	"internal/${svc}/service" "internal/${svc}/repo" \
	"internal/${svc}/model" "internal/${svc}/job"
render "$assets/main.go.tpl"   "cmd/${svc}/main.go"
render "$assets/config.go.tpl" "internal/${svc}/config/config.go"
render "$assets/service.yaml.tpl" "configs/${svc}.yaml"
# 分层骨架：包注释即契约，各层填什么见渲染出的 doc.go 注释。
render "$assets/service.doc.go.tpl" "internal/${svc}/service/doc.go"
render "$assets/repo.doc.go.tpl"    "internal/${svc}/repo/doc.go"
render "$assets/model.doc.go.tpl"   "internal/${svc}/model/doc.go"
render "$assets/job.doc.go.tpl"     "internal/${svc}/job/doc.go"
if [ "$variant" != "none" ]; then mkdir -p "internal/${svc}/handler"; fi
case "$variant" in
	none) render "$assets/init_app.go.tpl"  "cmd/${svc}/initial/init_app.go" ;;
	rpc)  render "$assets/init_rpc.go.tpl"  "cmd/${svc}/initial/init_app.go"
	      render "$assets/handler.doc.go.tpl" "internal/${svc}/handler/doc.go" ;;
	both) render "$assets/init_both.go.tpl" "cmd/${svc}/initial/init_app.go"
	      render "$assets/handler.doc.go.tpl" "internal/${svc}/handler/doc.go"
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

layers="config/service/repo/model/job"
[ "$variant" != "none" ] && layers="${layers}/handler"
render_done=1
echo "已生成 cmd/${svc}、internal/${svc}（${layers}）、configs/${svc}.yaml（变体: ${variant}）"
echo "后续：1) AGENTS.md 登记错误码分段  2) 写 proto 契约、填业务实体(各层 doc.go 有指引)  3) make check"
if [ "$variant" = both ]; then
	echo "注意：openapi 包要嵌 spec,在 api/${svc} 写好 proto 并跑过 make proto 之前 make check 编译不过,属预期。"
fi
