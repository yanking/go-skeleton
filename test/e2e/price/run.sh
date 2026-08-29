#!/usr/bin/env bash
# price 服务端对端测试：真正跑一遍完整的 cmd/price（instruments 子命令、
# 常驻 daemon、backfill 子命令），配合 mockd 模拟的 Binance 现货 ws/REST 两个
# 面，验证 initial.App 装配出来的整条链路——从 ws 收线 K 线到批量落库、从
# 浅层盘口到 Redis 最新值、从 SIGTERM 到停机排空落盘、从 REST 分页到补洞
# upsert——第一次真正端到端跑通（此前十二个任务只有单元/集成测试覆盖到
# 装配边界，从未有东西执行过完整的 initial.App）。
#
# 前置：本地 Postgres（configs/price.yaml 的 pgsql.write，本机 127.0.0.1:5432，
# 用户/库均为 app）、本地 Redis（127.0.0.1:6379，db 1，与 pgsql 同源配置）、
# psql、curl、go。不用 redis-cli——本机未装，改用 /dev/tcp 直接说 Redis 的
# 内联协议（见 redis_cmd），这是本脚本相对 channel 范本新增的一处踩坑记录。
#
# 用法：make e2e SVC=price；E2E_PG_DSN 可覆盖目标库；E2E_WS_ADDR/E2E_REST_ADDR
# 可覆盖 mockd 的两个监听地址（本机 9090/9092/9093 被 nginx 占用，默认值已
# 避开）——mockd 用两个地址分别模拟 exchanges.binance 的 ws_url/rest_url，
# 与真实 Binance 域名拆成 stream.binance.com/api.binance.com 两处同构。
#
# 绝对不连真实交易所：临时配置里 exchanges 段只有 binance 一项，且 ws_url/
# rest_url 全部指向 mockd；okx 整段不写（不是 enabled: false）——彻底不给
# 装配层任何机会去认识 okx，比"写了又关停"更绝对。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$ROOT"

WS_ADDR="${E2E_WS_ADDR:-127.0.0.1:18190}"   # mockd ws 面，对应 exchanges.binance.ws_url
REST_ADDR="${E2E_REST_ADDR:-127.0.0.1:18191}" # mockd REST 面，对应 exchanges.binance.rest_url
PG_DSN="${E2E_PG_DSN:-$(sed -n '/^pgsql:/,/^[^[:space:]]/ s/^[[:space:]]*write:[[:space:]]*"\([^"]*\)".*/\1/p' configs/price.yaml)}"
REDIS_HOST="127.0.0.1"
REDIS_PORT="6379"
REDIS_DB="1" # 与 configs/price.yaml 的 redis.db 一致(与其它服务错开)

SYMBOL="BTCUSDT"
REDIS_DEPTH_KEY="price:binance:spot:${SYMBOL}:depth" # 拼法见 internal/price/service/route.go latestKey

# 三组开盘时间，须与 test/e2e/price/mockd/main.go 的同名常量完全一致——
# mockd 推什么、这里就断言什么，两处失配会让脚本断言一个永远不存在的值。
STREAM_OT1=1700000000000 # 常驻阶段：ws 推送的第一根已收线
STREAM_OT2=1700000060000 # 常驻阶段：ws 推送的第二根已收线（未收线的第三根开盘时间是 +120000，不应落库）
LATE_OT1=1700000600000   # SIGTERM 停机断言：/push-late-klines 追加推送的第一根
LATE_OT2=1700000660000
BACKFILL_OT1=1600000000000 # backfill 子命令：REST 历史 K 线三根
BACKFILL_OT2=1600000060000
BACKFILL_OT3=1600000120000
# 与 BACKFILL_OT* 配套的 -from/-to：REST 历史三根覆盖 [OT1, OT1+120000]，
# 下一页起点是 OT1+180000（见 rest.go nextOpenTime），-to 只要落在
# (OT1+120000, OT1+180000] 区间内就会让 Klines 判定已到终点、只翻一页——
# mockd 的 /api/v3/klines 本就不看查询参数（见其注释），选这两个值只是为了
# 让分页逻辑走一次就干净终止，不依赖 mockd 识别查询范围。
BACKFILL_FROM="2020-09-13T12:25:40Z" # BACKFILL_OT1 前 60s
BACKFILL_TO="2020-09-13T12:28:50Z"   # BACKFILL_OT1 后 130s，落在终止区间内

PASS=0 FAIL=0
TMP="$(mktemp -d)"
PIDS=()
DAEMON_PID=""

cleanup() {
	[ -n "$DAEMON_PID" ] && kill -9 "$DAEMON_PID" >/dev/null 2>&1 || true
	for pid in "${PIDS[@]:-}"; do kill "$pid" >/dev/null 2>&1 || true; done
	rm -rf "$TMP"
}
trap cleanup EXIT

# bg 后台起进程并 disown（理由同 channel 范本：避免停机 kill 时的 "Terminated"
# 噪音污染输出）；仅用于 mockd 这类"用完即杀、不关心退出码"的辅助进程——
# price 常驻进程需要观察真实退出码，走独立的 start_daemon/term_and_wait，
# 不复用这个 helper。
bg() { "$@" & PIDS+=($!); disown; }

die() { echo ">> $*" >&2; exit 1; }

# wait_tcp 探测端口可建连，理由与用法同 channel 范本 wait_tcp。
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

assert_eq() {
	local name="$1" want="$2" got="$3"
	if [ "$want" = "$got" ]; then
		pass "$name"
	else
		fail "$name" "want: $want" "got:  $got"
	fi
}

# pg 对目标库跑一条标量查询，去掉首尾空白后返回结果——psql -tAc 已经是最紧凑的
# 输出形式，这里只再 trim 一次换行，避免和期望值比较时因空白差一位而误判失败。
# 结尾 || true：本函数专供 poll_count 在超时预算内反复重试，一次连接抖动
# 不该在 set -e 下直接打死整个脚本，交由 poll_count 的超时和随后的 assert_eq
# 一起给出准确的失败原因。
pg() { psql -tAc "$1" "$PG_DSN" 2>/dev/null | tr -d '[:space:]' || true; }

# poll_count 轮询 pg 查询直到结果等于 want 或超时（每 0.25s 一次），返回最终
# 读到的值（不在此处断言，交给调用方紧接着用 assert_eq 判断，失败时能同时
# 打印期望值与实际值）。用来吸收"daemon 起来、拨号、mockd 推帧、批量落库"这
# 段不确定时长，比猜一个固定 sleep 更稳，也比死等更快出结果。
poll_count() {
	local query="$1" want="$2" timeout="${3:-10}"
	local got="" i
	for ((i = 0; i < timeout * 4; i++)); do
		got="$(pg "$query")"
		[ "$got" = "$want" ] && break
		sleep 0.25
	done
	printf '%s' "$got"
}

# redis_cmd 用 Redis 的内联命令协议裸发一行命令并取回复，不依赖 redis-cli——
# 本机未装（踩坑记录：起初以为和 psql/jq 一样是标配，实测 command -v 找不到），
# 改走 /dev/tcp（bash 内建）手搓协议：先选库，再发目标命令，各自一行以 \r\n
# 结尾，Redis 服务端原生支持这种明文内联格式，不必自己拼 RESP 数组。fd 用 9，
# 避免与 wait_tcp 的 fd 3 撞号（wait_tcp 那个开在子 shell 里不会泄漏，这里仍
# 单独选号图省心）。
#
# 回复按 RESP 类型分两种读法（实测确认，见任务报告的踩坑记录）：EXISTS/DEL
# 这类整数回复（:N）与 SELECT 的 +OK 都是单行；GET 这类批量字符串回复是两行
# ——首行 $<len> 只是长度前缀，真正数据在下一行（键不存在则是 $-1，没有第二
# 行）。首行是 $ 前缀但不是 $-1 时才再读一行，否则直接把首行当结果返回。
#
# 另一处踩坑：Redis 用 \r\n 分行，bash 的 read 只按 \n 断行，行尾的 \r 会原样
# 留在变量里——`[ "$got" = ":1" ]` 这类比较会因为实际是 ":1\r" 而静默判负，
# 终端里却因为 \r 只把光标拉回行首、后面立刻接 \n 而看不出任何异常，第一次
# 就是这么栽的。每次 read 之后都得显式 trim 掉这个尾部 \r。
redis_cmd() {
	local line reply
	exec 9<>"/dev/tcp/$REDIS_HOST/$REDIS_PORT"
	printf 'SELECT %s\r\n%s\r\n' "$REDIS_DB" "$*" >&9
	IFS= read -r -t 2 line <&9 # SELECT 的 +OK，丢弃
	IFS= read -r -t 2 line <&9 # 目标命令的首行回复
	line="${line%$'\r'}"
	case "$line" in
	'$-1') reply="" ;;
	'$'*)
		IFS= read -r -t 2 reply <&9
		reply="${reply%$'\r'}"
		;;
	*) reply="$line" ;;
	esac
	exec 9<&- 9>&-
	printf '%s' "$reply"
}

# term_and_wait 向 pid 发 SIGTERM 并等待其退出，最多等 timeout 秒——看门狗
# 到期会 SIGKILL 强杀，但仍然只会真正触发一次（进程已经死了 kill -9 是
# no-op）。返回进程的真实退出码：pkg/app 的约定是 ctx 取消属正常退出，
# Run 应返回 nil，main.go 据此以 0 退出（见 cmd/price/main.go runDaemon），
# 非 0 说明停机路径出了错，不能被脚本当成"反正进程退出了就算数"悄悄放过。
term_and_wait() {
	local pid="$1" timeout="${2:-15}"
	kill -TERM "$pid" 2>/dev/null || true
	(
		sleep "$timeout"
		kill -9 "$pid" 2>/dev/null || true
	) &
	local watchdog=$!
	wait "$pid"
	local ec=$?
	kill "$watchdog" 2>/dev/null || true
	wait "$watchdog" 2>/dev/null || true
	return $ec
}

# —— 前置检查与准备 ——————————————————————————————————————————————

for cmd in psql curl go; do
	command -v "$cmd" >/dev/null 2>&1 || die "缺 $cmd，先安装再跑 E2E"
done
for addr in "$WS_ADDR" "$REST_ADDR"; do
	port="${addr#*:}"
	lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 && die "端口 $port 已被占用（可用 E2E_WS_ADDR / E2E_REST_ADDR 换端口）"
done
wait_tcp "$REDIS_HOST" "$REDIS_PORT" || die "本机 Redis($REDIS_HOST:$REDIS_PORT) 不可达"

echo ">> 应用迁移（幂等）并清空本服务三张表，保证脚本可重复跑"
export MIGRATE_DSN="$PG_DSN"
make -s migrate-up SVC=price || die "迁移失败"
psql -v ON_ERROR_STOP=1 "$PG_DSN" >/dev/null <<SQL
TRUNCATE TABLE price_klines, price_instruments, price_subscriptions RESTART IDENTITY;
SQL
redis_cmd "DEL $REDIS_DEPTH_KEY" >/dev/null

echo ">> 构建并启动 mockd"
go build -o "$TMP/mockd" ./test/e2e/price/mockd || die "mockd 构建失败"
bg "$TMP/mockd" -ws-addr "$WS_ADDR" -rest-addr "$REST_ADDR" >"$TMP/mockd.log" 2>&1
wait_tcp "${WS_ADDR%:*}" "${WS_ADDR#*:}" || { tail -n 5 "$TMP/mockd.log" >&2; die "mockd ws 面 10s 内未就绪"; }
wait_tcp "${REST_ADDR%:*}" "${REST_ADDR#*:}" || { tail -n 5 "$TMP/mockd.log" >&2; die "mockd REST 面 10s 内未就绪"; }

echo ">> 构建 price"
go build -o "$TMP/price" ./cmd/price || die "price 构建失败"

# 临时配置：exchanges 段只写 binance 一项，ws_url/rest_url 全指向 mockd；
# enabled 显式写 true——这是评审给第 13 个任务留的第一条要求：
# config.Exchange.Enabled 是 Go 零值 false，临时配置若漏写这一行，binance
# 会被装配层静默跳过（见 cmd/price/initial/init_app.go createServer 对
# cfg.Enabled 的判断与其注释），daemon 照常起来、一条 ws 连接都不建、一根
# K 线都不采，而本脚本只会在最后的行数断言上失败，把排查方向引向完全
# 错误的地方——出现过一次就足够记住，这里显式留了这条注释防止日后照抄本
# 文件时又漏掉。okx 整段不写，不是写 enabled: false：不给装配层任何机会
# 认识 okx，比"配置了又关停"更彻底地保证不会有代码路径尝试连接真实交易所。
cat >"$TMP/price.yaml" <<YAML
log:
  name: price-e2e
  level: info
  format: json

app:
  stop_timeout: 10s

telemetry:
  exporter: stdout

pgsql:
  write: "$PG_DSN"
  max_open_conns: 20
  conn_max_lifetime: 30m

redis:
  addrs: ["$REDIS_HOST:$REDIS_PORT"]
  db: $REDIS_DB

collector:
  reload_interval: 5m
  max_backfill_window: 720h
  backfill_concurrency: 2
  kline_queue_size: 1024
  snapshot_queue_size: 256

exchanges:
  binance:
    enabled: true
    ws_url: "ws://$WS_ADDR/ws"
    rest_url: "http://$REST_ADDR"
    max_streams_per_conn: 1024
    rest_per_second: 10
    rest_burst: 20
    dial_timeout: 5s
    reconnect_backoff_min: 1s
    reconnect_backoff_max: 60s
    import_quotes: ["USDT"]
YAML

# —— 用例 ————————————————————————————————————————————————————————

echo
echo ">> 用例执行"

echo ">> price instruments：全量导入 mockd 的 exchangeInfo（两个 USDT 交易对）"
# if 包住整条命令：set -e 对 if 的条件部分不生效，非零退出码才能在这里被
# 正常捕获断言，而不是让脚本在这一行直接被 errexit 打死。
if "$TMP/price" instruments -config "$TMP/price.yaml" -exchange=binance >"$TMP/instruments.log" 2>&1; then
	pass "price instruments 退出码为 0"
else
	fail "price instruments 退出码为 0" "$(tail -n 20 "$TMP/instruments.log")"
fi
assert_eq "price_instruments 落库 2 行（BTCUSDT/ETHUSDT，均 TRADING+USDT）" "2" "$(pg "SELECT count(*) FROM price_instruments WHERE exchange='binance'")"

echo ">> 写入订阅：只订 depth 一条（不订 kline）——这样常驻阶段收到的两根已收线"
echo "   K 线纯粹来自 mockd 的固定 ws 脚本，不会和 OnReady 触发的自动补洞（只对"
echo "   kline 类型订阅生效，见 internal/price/service/backfill.go backfillOne）"
echo "   混在一起，price_klines 的行数断言才干净。"
psql -v ON_ERROR_STOP=1 "$PG_DSN" >/dev/null <<SQL
INSERT INTO price_subscriptions (exchange, market, native_symbol, stream_type, interval, enabled)
VALUES ('binance', 'spot', '$SYMBOL', 'depth', '', TRUE);
SQL

echo ">> 起常驻 daemon"
"$TMP/price" -config "$TMP/price.yaml" >"$TMP/price-daemon.log" 2>&1 &
DAEMON_PID=$!
PIDS+=("$DAEMON_PID")

got="$(poll_count "SELECT count(*) FROM price_klines WHERE exchange='binance'" "2" 10)"
assert_eq "常驻若干秒后 price_klines 只有已收线那两根" "2" "$got"
assert_eq "已收线两根的开盘时间与 mockd 脚本一致（未收线那根不在其中）" \
	"${STREAM_OT1},${STREAM_OT2}" \
	"$(pg "SELECT string_agg(open_time::text, ',' ORDER BY open_time) FROM price_klines WHERE exchange='binance' AND native_symbol='$SYMBOL'")"
assert_eq "已收线两根 source 均为实时流(1)" "1,1" \
	"$(pg "SELECT string_agg(source::text, ',' ORDER BY open_time) FROM price_klines WHERE exchange='binance' AND native_symbol='$SYMBOL'")"

got="$(redis_cmd "EXISTS $REDIS_DEPTH_KEY")"
if [ "$got" = ":1" ]; then
	# 与 mockd 推送的 payload 对照：只留 bids/asks（见 exchange.Snapshot 类型
	# 注释规定的归一化形状），不透传 lastUpdateId。
	body="$(redis_cmd "GET $REDIS_DEPTH_KEY")"
	if printf '%s' "$body" | grep -q '"bids":\[\["49999.50","1.2"\]\]' &&
		printf '%s' "$body" | grep -q '"asks":\[\["50000.50","0.8"\]\]'; then
		pass "Redis 盘口 key 存在且 payload 是归一化后的 bids/asks"
	else
		fail "Redis 盘口 key 存在但 payload 形状不对" "got: $body"
	fi
else
	fail "Redis 盘口 key 存在" "EXISTS 回复: $got"
fi

echo ">> 验证停机排空/flush 路径：SIGTERM 前推送在途已收线 K 线，进程退出后应已落盘"
echo "   （本条验证的是 RunWriters 的 ctx.Done() 排空 + 独立 shutdownCtx flush 这条"
echo "   路径本身可用（在途已收线 K 线不因进程退出而丢失）。明确不是「writer 必须"
echo "   先于全部 stream.Manager 注册」那条顺序不变量的回归防护——本脚本的 klineCh"
echo "   容量(1024)远大于这里的在途数据量，route() 往 klineCh 发送从未真正阻塞过，"
echo "   颠倒注册顺序本脚本依然全过（已实测确认，见任务报告）。该顺序不变量唯一"
echo "   的确定性防线是白盒单测 TestCreateServer_WriterRegisteredBeforeAllStreamManagers"
echo "   （cmd/price/initial/init_app_test.go），不是本脚本。）"
curl -sf -X POST "http://$WS_ADDR/push-late-klines" >/dev/null || die "触发 mockd 推送在途 K 线失败"
sleep 0.2 # 留一点时间让帧真正被 daemon 的 ws 读循环收到、路由进 klineCh，同时
# 远小于 klineBatchInterval（1s）——不能等到定时器自己把这批数据刷盘，
# 否则这条用例就退化成"验证批量写入"，而不是本条要验证的"验证停机排空"。
# if 包住：term_and_wait 内部 wait "$pid" 拿到的非零退出码不该在这里被
# set -e 直接打死脚本，本条恰恰就是要断言"退出码是不是 0"这件事。
if term_and_wait "$DAEMON_PID" 15; then
	daemon_ec=0
else
	daemon_ec=$?
fi
assert_eq "daemon 收到 SIGTERM 后正常退出（Run 返回 nil）" "0" "$daemon_ec"
DAEMON_PID="" # 已经 wait 过，cleanup 不必再 kill

assert_eq "在途的两根已收线 K 线经停机排空落盘" "${LATE_OT1},${LATE_OT2}" \
	"$(pg "SELECT string_agg(open_time::text, ',' ORDER BY open_time) FROM price_klines WHERE exchange='binance' AND native_symbol='$SYMBOL' AND open_time >= $LATE_OT1")"

echo ">> price backfill：显式区间补历史 K 线（mockd 的三根固定历史）"
if "$TMP/price" backfill -config "$TMP/price.yaml" -exchange=binance -market=spot \
	-symbol="$SYMBOL" -interval=1m -from="$BACKFILL_FROM" -to="$BACKFILL_TO" \
	>"$TMP/backfill.log" 2>&1; then
	pass "price backfill 退出码为 0"
else
	fail "price backfill 退出码为 0" "$(tail -n 20 "$TMP/backfill.log")"
fi
assert_eq "历史三根已 upsert 且 source=2（补洞回填）" "${BACKFILL_OT1},${BACKFILL_OT2},${BACKFILL_OT3}" \
	"$(pg "SELECT string_agg(open_time::text, ',' ORDER BY open_time) FROM price_klines WHERE exchange='binance' AND native_symbol='$SYMBOL' AND source=2")"

# —— 汇总 ————————————————————————————————————————————————————————

echo
if [ "$FAIL" -gt 0 ]; then
	echo ">> 失败 $FAIL 例（通过 $PASS 例），daemon 日志末尾："
	tail -n 30 "$TMP/price-daemon.log" 2>/dev/null | sed 's/^/   /'
	echo ">> mockd 日志末尾："
	tail -n 20 "$TMP/mockd.log" 2>/dev/null | sed 's/^/   /'
	exit 1
fi
echo ">> 全部 $PASS 例通过"
