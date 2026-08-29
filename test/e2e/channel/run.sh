#!/usr/bin/env bash
# channel 服务端对端测试：起真实服务进程与 mockd 下游模拟器，用 grpcurl 走完
# gRPC → 拦截链 → handler → service → adapter → 下游渠道 的全链路并逐用例断言。
# 前置：本地 Postgres（configs/channel.yaml 的 pgsql.write）、grpcurl、jq、psql、openssl。
# 用法：make e2e SVC=channel；E2E_PG_DSN 可覆盖目标库，E2E_SVC_ADDR / E2E_MOCK_ADDR
# 可覆盖两个监听地址（端口被占的环境用，服务经临时配置起在覆盖地址上）。
# 测试商户（payapay/e2etest/INR）的 platform.base_url 指向 mockd，故全部用例只打
# 本机，绝不触网；单号关键字（SUCC/FAIL/ERR）决定 mockd 回包形态，见 mockd/main.go。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$ROOT"
PATH="$HOME/go/bin:$PATH"

SVC_ADDR="${E2E_SVC_ADDR:-127.0.0.1:9092}" # 服务监听地址，经临时配置注入
MOCK_ADDR="${E2E_MOCK_ADDR:-127.0.0.1:18099}" # mockd 监听地址，商户行 base_url 随其生成
SECRET="e2e-secret"         # 测试商户 app_secret，回调用例按 payapay 规则重算签名
PG_DSN="${E2E_PG_DSN:-$(sed -n '/^pgsql:/,/^[^[:space:]]/ s/^[[:space:]]*write:[[:space:]]*"\([^"]*\)".*/\1/p' configs/channel.yaml)}"

# 三元组的三种形态：命中（打 mockd）、未命中（40001）、缺字段（10001）。
ROUTE_HIT='{"channelName":"payapay","merchantNo":"e2etest","currency":"INR"}'
ROUTE_MISS='{"channelName":"payapay","merchantNo":"nobody","currency":"INR"}'
ROUTE_EMPTY='{"channelName":"payapay","merchantNo":"","currency":"INR"}'

PASS=0 FAIL=0
TMP="$(mktemp -d)"
PIDS=()

cleanup() {
	for pid in "${PIDS[@]:-}"; do kill "$pid" >/dev/null 2>&1 || true; done
	rm -rf "$TMP"
}
trap cleanup EXIT

# bg 后台起进程并 disown:bash 不再跟踪 job，停机 kill 时就不会往 stderr 打
# "Terminated" 噪音，用例输出保持干净。
bg() { "$@" & PIDS+=($!); disown; }

die() { echo ">> $*" >&2; exit 1; }

# wait_tcp 探测端口可建连。channel 服务起得常比 mockd 快,不等下游就绪,
# 首条打下游的用例会闪断成 40002。
wait_tcp() {
	local i
	for ((i = 0; i < 40; i++)); do
		(exec 3<>"/dev/tcp/$1/$2") 2>/dev/null && return 0
		sleep 0.25
	done
	return 1
}

pass() { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() {
	FAIL=$((FAIL + 1))
	printf '  \033[31mFAIL\033[0m %s\n' "$1"
	shift
	[ "$#" -gt 0 ] && printf '%s\n' "$@" | sed 's/^/      /'
}

# rpc 发起一次调用并合并 stdout/stderr——grpcurl 把错误细节打到 stderr，
# 成功时输出即 proto JSON，失败时含 Code/Message/Details 三段，断言统一在此文本上做。
rpc() {
	grpcurl -plaintext -max-time 10 -d "$2" "$SVC_ADDR" "channel.v1.ChannelService/$1" 2>&1 || true
}

# assert_jq 断言成功回包满足 jq 过滤器。注意 proto3 JSON 把 int64 渲染为字符串,金额断言按字符串写。
assert_jq() {
	local name="$1" filter="$2" out="$3"
	if printf '%s' "$out" | jq -e "$filter" >/dev/null 2>&1; then
		pass "$name"
	else
		fail "$name" "filter: $filter" "output: $(printf '%s' "$out" | head -c 500)"
	fi
}

# assert_err 断言业务错误:gRPC 状态码 + 面向客户端的 Message + details 里的业务码。
# grpcurl 把 details 打成缩进 JSON（"reason": "40001"，冒号后带空格），reason 匹配须容空格。
assert_err() {
	local name="$1" grpc_code="$2" message="$3" reason="$4" out="$5"
	if printf '%s' "$out" | grep -q "Code: $grpc_code" &&
		printf '%s' "$out" | grep -q "Message: $message" &&
		printf '%s' "$out" | grep -qE "\"reason\": *\"$reason\""; then
		pass "$name"
	else
		fail "$name" "expect: Code=$grpc_code Message=$message reason=$reason" "output: $(printf '%s' "$out" | head -c 500)"
	fi
}

# payapay_sign 与适配器 createSign 同构：跳过 sign 与空值、按 key 排序拼 k=v&…&key=secret 取 md5。
payapay_sign() {
	local secret="$1" kv str=""
	shift
	while IFS= read -r kv; do
		[ -n "$kv" ] && str+="$kv&"
	done <<<"$(printf '%s\n' "$@" | sort)"
	printf '%s' "${str}key=$secret" | openssl dgst -md5 | awk '{print $NF}'
}

# —— 前置检查与准备 ——————————————————————————————————————————————

for cmd in grpcurl jq psql openssl go; do
	command -v "$cmd" >/dev/null 2>&1 || die "缺 $cmd，先安装再跑 E2E"
done
for port in "${SVC_ADDR#*:}" "${MOCK_ADDR#*:}"; do
	lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 && die "端口 $port 已被占用（可用 E2E_SVC_ADDR / E2E_MOCK_ADDR 换端口）"
done

echo ">> 应用迁移（幂等）并写入测试商户"
export MIGRATE_DSN="$PG_DSN"
make -s migrate-up SVC=channel || die "迁移失败"

psql -v ON_ERROR_STOP=1 "$PG_DSN" >/dev/null <<SQL
INSERT INTO channels (channel_name, merchant_no, currency, channel_level, callback_headers,
                      callback_data_source, callback_return, callback_ip_whitelist, payout_supports,
                      limit_payment_min, limit_payment_max, limit_payout_min, limit_payout_max,
                      payment_commission_rate, payment_commission_extra,
                      payout_commission_rate, payout_commission_extra,
                      platform, reconcile_enabled)
VALUES ('payapay', 'e2etest', 'INR', 4, '[]',
        1, 'success', '', '[1]',
        20000, 5000000, 30000, 5000000,
        0, 0, 0, 0,
        '{"base_url":"http://$MOCK_ADDR","apis":{"payment":"/api/v1/payApi/CreatePayInOrder","payment_query":"/api/v1/payApi/QueryOrder","payout":"/api/v1/payApi/CreatePayOutOrder","payout_query":"/api/v1/payApi/QueryOrder","balance_query":"/api/v1/payApi/QueryBalance"},"mer_id":90001,"app_id":90002,"app_secret":"e2e-secret","pay_in_code":3,"pay_out_code":4}',
        FALSE)
ON CONFLICT (channel_name, merchant_no, currency) DO UPDATE
SET platform = EXCLUDED.platform, updated_at = now();
SQL

echo ">> 构建并启动 mockd 与 channel 服务"
go build -o "$TMP/mockd" ./test/e2e/channel/mockd || die "mockd 构建失败"
go build -o "$TMP/channel" ./cmd/channel || die "channel 构建失败"
bg "$TMP/mockd" -addr "$MOCK_ADDR" >"$TMP/mockd.log" 2>&1
wait_tcp "${MOCK_ADDR%:*}" "${MOCK_ADDR#*:}" || { tail -n 5 "$TMP/mockd.log" >&2; die "mockd 10s 内未就绪"; }
# 服务配置走临时文件：grpc_addr 换成本次的 SVC_ADDR，其余与仓库配置一致。
sed "s|grpc_addr: \":[0-9]*\"|grpc_addr: \":${SVC_ADDR#*:}\"|" configs/channel.yaml > "$TMP/channel.yaml"
bg "$TMP/channel" -config "$TMP/channel.yaml" >"$TMP/channel.log" 2>&1

for i in $(seq 1 30); do
	grpcurl -plaintext -max-time 2 "$SVC_ADDR" list >/dev/null 2>&1 && break
	[ "$i" -eq 30 ] && { tail -n 20 "$TMP/channel.log" >&2; die "服务 15s 内未就绪"; }
	sleep 0.5
done

# —— 用例 ————————————————————————————————————————————————————————

echo ">> 用例执行"

out="$(rpc ListChannels '{}')"
assert_jq "ListChannels 返回测试商户及元数据" \
	'.channels | map(select(.channelName == "payapay" and .merchantNo == "e2etest" and .currency == "INR")) | length == 1' "$out"

out="$(rpc PaymentOrder "$(jq -nc --argjson route "$ROUTE_HIT" \
	'{route:$route, orderNo:"E2E-PAY-001", amount:"5000", name:"e2e", phone:"9000000001",
	  email:"e2e@test.local", notifyUrl:"https://gw.test/notify", deeplink:false, timeout:300}')")"
assert_jq "PaymentOrder 下单成功，渠道单号与支付链接回显本方单号" \
	'.url == "https://mock.local/pay/E2E-PAY-001" and .outOrderNo == "PAY-E2E-PAY-001" and (.response | length > 0)' "$out"

out="$(rpc PaymentOrder "$(jq -nc --argjson route "$ROUTE_MISS" '{route:$route, orderNo:"E2E-X", amount:"5000"}')")"
assert_err "PaymentOrder 路由未命中 → 40001" NotFound "渠道实例不存在" 40001 "$out"

out="$(rpc PaymentOrder "$(jq -nc --argjson route "$ROUTE_EMPTY" '{route:$route, orderNo:"E2E-X", amount:"5000"}')")"
assert_err "PaymentOrder 路由缺字段 → 10001" InvalidArgument "参数错误" 10001 "$out"

out="$(rpc PayoutOrder "$(jq -nc --argjson route "$ROUTE_HIT" \
	'{route:$route, wayCode:1, orderNo:"E2E-PO-001", amount:"30000", name:"e2e",
	  phone:"9000000001", email:"e2e@test.local", bankName:"Mock Bank", bankCode:"MOCK0000001",
	  accountNo:"1234567890", notifyUrl:"https://gw.test/notify"}')")"
assert_jq "PayoutOrder 下单成功，渠道单号回显本方单号" \
	'.outOrderNo == "PO-E2E-PO-001" and (.response | length > 0)' "$out"

out="$(rpc PaymentQuery "$(jq -nc --argjson route "$ROUTE_HIT" '{route:$route, orderNo:"E2E-PAY-SUCC-001"}')")"
assert_jq "PaymentQuery 渠道终态成功映射为对外状态 2" \
	'.status == 2 and .amount == "5000" and .outOrderNo == "Q-E2E-PAY-SUCC-001" and .referenceNo == "UTR-E2E-PAY-SUCC-001"' "$out"

out="$(rpc PayoutQuery "$(jq -nc --argjson route "$ROUTE_HIT" '{route:$route, orderNo:"E2E-PO-FAIL-001"}')")"
assert_jq "PayoutQuery 渠道终态失败映射为对外状态 3" '.status == 3' "$out"

out="$(rpc PaymentQuery "$(jq -nc --argjson route "$ROUTE_HIT" '{route:$route, orderNo:"E2E-PAY-ERR-001"}')")"
assert_err "PaymentQuery 渠道业务拒绝 → 40002" Unavailable "下游渠道请求失败" 40002 "$out"

cb_req() { # body_json → 完整请求（route + 原样转发的 header 与 body）
	jq -nc --argjson route "$ROUTE_HIT" --arg data "$1" '{route:$route, header:{"X-Mock":"e2e"}, data:$data}'
}

sign="$(payapay_sign "$SECRET" "dis_order_no=PAY-CB-001" "order_no=E2E-CB-001" "order_price=5000" "real_price=5000" "status=2")"
out="$(rpc PaymentCallback "$(cb_req "$(jq -nc --arg s "$sign" \
	'{order_no:"E2E-CB-001", dis_order_no:"PAY-CB-001", status:"2", order_price:"5000", real_price:"5000", sign:$s}')")")"
assert_jq "PaymentCallback 验签通过，成功回调取 real_price" \
	'.orderNo == "E2E-CB-001" and .outOrderNo == "PAY-CB-001" and .callbackType == 1 and .amount == "5000"' "$out"

# 签名按 real_price=5000 计算、报文改成 9999：验签必失败，覆盖 40003。
out="$(rpc PaymentCallback "$(cb_req "$(jq -nc --arg s "$sign" \
	'{order_no:"E2E-CB-002", dis_order_no:"PAY-CB-002", status:"2", order_price:"5000", real_price:"9999", sign:$s}')")")"
assert_err "PaymentCallback 签名不符 → 40003" PermissionDenied "回调验签失败" 40003 "$out"

sign="$(payapay_sign "$SECRET" "dis_order_no=PAY-CB-003" "order_no=E2E-CB-003" "order_price=5000" "real_price=5000" "status=5")"
out="$(rpc PaymentCallback "$(cb_req "$(jq -nc --arg s "$sign" \
	'{order_no:"E2E-CB-003", dis_order_no:"PAY-CB-003", status:"5", order_price:"5000", real_price:"5000", sign:$s}')")")"
assert_err "PaymentCallback 签名通过但状态未知 → 40004" InvalidArgument "回调状态未知" 40004 "$out"

sign="$(payapay_sign "$SECRET" "dis_order_no=PO-CB-001" "order_no=E2E-POCB-001" "order_price=3000" "status=9")"
out="$(rpc PayoutCallback "$(cb_req "$(jq -nc --arg s "$sign" \
	'{order_no:"E2E-POCB-001", dis_order_no:"PO-CB-001", status:"9", order_price:"3000", sign:$s}')")")"
assert_jq "PayoutCallback 状态 9 映射失败回调，金额取 order_price" \
	'.orderNo == "E2E-POCB-001" and .callbackType == 2 and .amount == "3000"' "$out"

out="$(rpc BalanceQuery "$(jq -nc --argjson route "$ROUTE_HIT" '{route:$route}')")"
assert_jq "BalanceQuery 返回 mock 余额" '.balance == "888888" and .frozenBalance == "77"' "$out"

# —— 汇总 ————————————————————————————————————————————————————————

echo
if [ "$FAIL" -gt 0 ]; then
	echo ">> 失败 $FAIL 例（通过 $PASS 例），channel 服务日志末尾："
	tail -n 20 "$TMP/channel.log" 2>/dev/null | sed 's/^/   /'
	echo ">> mockd 日志末尾："
	tail -n 20 "$TMP/mockd.log" 2>/dev/null | sed 's/^/   /'
	exit 1
fi
echo ">> 全部 $PASS 例通过"
