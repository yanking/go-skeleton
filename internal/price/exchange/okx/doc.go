// Package okx 是 price 服务对接 OKX 现货行情的协议翻译层：把 exchange 包
// 定义的中立类型（Sub/ConnPlan/Event/Kline/Snapshot/Instrument）与 OKX 现货
// 原生报文互译。symbol 形态、bar 拼写、限速数字、连接上限等交易所方言只在
// 本包出现，不泄漏到 exchange 包之外——这是本包存在的唯一理由，做法与
// internal/price/exchange/binance 包一致（两包互相独立，不共享代码，只共享
// exchange 包定义的契约）。
//
// 本包只做「协议翻译」：不持有连接、不做重连与心跳（见 internal/price/stream），
// 不做限速与重试（见 collector），Decode 对无法识别的帧一律返回零值 Event 与
// nil error（交易所会持续推送订阅确认、错误响应、心跳应答、本包未实现的流
// 类型等帧，它们不是错误，见 exchange.Exchange.Decode 的接口注释）。
//
// # 已核实的协议事实（核实日期：2026-08-28，来源：OKX 官方文档
// https://www.okx.com/docs-v5/en/ 全量渲染页——该页是单页 SPA，直接抓取会被
// 截断，本次核实改用 curl 拉取服务端渲染出的完整 HTML 再离线抽取文本核对，
// 并用 github.com/ccxt/ccxt 仓库 python/ccxt/okx.py 的实现与响应样例注释做
// 交叉验证，具体见下）
//
//  1. WebSocket 端点其实有三个，不是简报给的一个：
//     "Public WebSocket: wss://ws.okx.com:8443/ws/v5/public"、
//     "Private WebSocket: wss://ws.okx.com:8443/ws/v5/private"、
//     "Business WebSocket: wss://ws.okx.com:8443/ws/v5/business"（Overview
//     小节 "Production Trading Services"）。**且 candle 类频道的 URL Path
//     明确标的是 /ws/v5/business，不是 /public**（"WS / Candlesticks
//     channel" 小节："URL Path /ws/v5/business"，且未标"(required login)"，
//     即无需鉴权但必须连 business 端点）；tickers（"WS / Tickers channel"
//     小节）与 books5（"WS / Order book channel" 小节）的 URL Path 都是
//     /ws/v5/public。这是简报未提及、本次核实才发现的一处协议事实，直接
//     影响 Plan 的实现：candle 订阅与 ticker/depth 订阅不能塞进同一条连接，
//     本包按 Sub.StreamType 分流到两个不同端点（见 ws.go businessWSURL、
//     Plan）。Config 仍只保留一个 WSURL 字段（与 binance.Config 同名约定，
//     见简报），business 地址靠替换 "/public" 后缀派生。
//
//  2. 单连接订阅数上限：**未能确认到一个像 Binance「单连接最多 1024 条流」
//     那样明确的条数上限**。查了 "Connect" 小节（WebSocket 总览）与
//     "WS / Order book channel" 小节，找到的是三个不同维度的限制，没有一个
//     是「条数」：
//     - "the total length of multiple channels cannot exceed 64 KB"
//     （多个频道打包进一次 subscribe 请求时，args 序列化后的总字节数上限，
//     不是条数）；
//     - "The total number of 'subscribe'/'unsubscribe'/'login' requests per
//     connection is limited to 480 times per hour"（限的是"请求次数"，
//     不是"订阅数"——一次 subscribe 请求的 args 可以打包多条订阅，本包的
//     Plan 就是这么做的，见 ws.go）；
//     - "The limit will be set at 30 WebSocket connections per specific
//     WebSocket channel per sub-account"（这条限的是"同一账号同一频道能开
//     几条连接"，且明确只适用于私有频道：Orders/Account/Positions/
//     Balance and positions/Position risk warning/Account greeks 六个
//     channel，公共行情频道 candle/tickers/books5 不在列，本包用不上）。
//     故 configs/price.yaml 的 max_streams_per_conn: 240 没有一个可直接
//     核对"符不符"的官方数字；按 64KB 换算，一条 candle/tickers 订阅报文
//     （如 {"channel":"candle1m","instId":"BTC-USDT"}）约 40 字节，240 条
//     打包后约 10KB，远低于 64KB 上限，是一个安全的自选保守值，本次核实
//     未发现需要改动它的依据，保留现值，只把配置里的注释从「待核实」改成
//     如实记录这个结论（不是"验证通过"，是"没有一个数字可比对"）。
//
//  3. candle 频道全部 bar 拼写：**大小写不是随意的，秒/分钟固定小写，
//     小时/天/周/月固定大写**，共 27 个 channel 名（"WS / Candlesticks
//     channel" 小节 "Channel name" 单元格原文逐字抄录）：
//     candle3M candle1M candle1W candle1D candle2D candle3D candle5D
//     candle12H candle6H candle4H candle2H candle1H candle30m candle15m
//     candle5m candle3m candle1m candle1s candle3Mutc candle1Mutc
//     candle1Wutc candle1Dutc candle2Dutc candle3Dutc candle5Dutc
//     candle12Hutc candle6Hutc。
//     即 s/m 小写、H/D/W/M 大写，utc 后缀固定小写且只出现在 ≥6H 的周期上
//     （REST /api/v5/market/candles 的 bar 参数说明印证了这个分界："UTC+8
//     opening price k-line: [6H/12H/1D/2D/3D/1W/1M/3M] UTC+0 opening price
//     k-line: [6Hutc/12Hutc/1Dutc/2Dutc/3Dutc/1Wutc/1Mutc/3Mutc]"）。本包
//     的 Sub.Interval 必须原样是这份拼写（如 "1m"、"1H"、"6Hutc"），不做任何
//     大小写转换，channel 名直接拼 "candle"+Interval（见 ws.go channelFor）。
//     candle 推送与两个 REST 端点的报文都是数组，按下标取值，天生不受
//     encoding/json 大小写折叠影响（见下方"大小写折叠核对结论"）。
//
//  4. books5 推送频率与形状："books5: 5 depth levels snapshot will be
//     pushed in the initial push. Snapshot data will be pushed every
//     100 ms when there are changes in the 5 depth levels snapshot."
//     （"WS / Order book channel" 小节）——即"变化触发，限频最多 100ms 一次"，
//     不是固定周期心跳式推送；无变化时不重复推送同一快照（但超过约 60 秒无
//     更新时会补发一次最新快照，见同小节 Exceptions 说明，本包不依赖这个
//     细节）。形状是对象数组（不是像 candle 那样的裸数组）："data": [{"asks":
//     [["111.06","55154","0","2"],...], "bids": [...], "instId": "...",
//     "ts": "..."}]，每档 4 元素 [价, 量, 已废弃恒为 "0" 的占位字段, 该价位
//     订单数]（同小节 "An example of the array of asks and bids values"）；
//     ts 字段的官方描述是 "Order book generation time, Unix timestamp
//     format in milliseconds"，适用于 books5——与 Binance 的浅层盘口不同
//     （Binance 那份报文本身不带任何事件时间），本包的 depth 快照 EventTime
//     直接取这个 ts，不留 0（见 ws.go decodeDepth）。
//
//  5. REST 历史 K 线两个端点：
//     - GET /api/v5/market/candles（"GET / Candlesticks" 小节）："This
//     endpoint can retrieve the latest 1,440 data entries."——只保留最新
//     1,440 根，对 collector.max_backfill_window（30 天）远远不够，本包
//     不用这个端点做补洞。
//     - GET /api/v5/market/history-candles（"GET / Candlesticks history"
//     小节）："Retrieve history candlestick charts from recent years"，
//     本包 Klines 只用这一个端点（理由与取舍见 rest.go 的 Klines 注释）。
//     两者的 limit 参数官方原文都是 "Number of results per request. The
//     maximum is 300. The default is 100."——**均为默认 100、上限 300**（与
//     网上一些二手资料"history 只有 100"的说法不同，那多半是过期信息，本次
//     以当前官方原文为准，与 ccxt 源码 1449-1450 行注释 "regular candles
//     (recent & historical) both have 300 max" 交叉验证一致）。
//     after/before 语义原文："after: Pagination of data to return records
//     earlier than the requested ts."、"before: Pagination of data to
//     return records newer than the requested ts. The latest data will
//     be returned when using before individually."——**两者都是排他边界**
//     （"earlier than"/"newer than"，不是"and including"）：after=X 只返回
//     ts < X 的记录，before=X 只返回 ts > X 的记录。官方原文没有再显式重复
//     一次"排他"这个词，这条结论是从措辞直接推断的；用 ccxt 源码交叉验证过：
//     ccxt 在需要"从 since 起（含）"取数据时，请求参数写的是
//     `request['before'] = max(since - 1, 0)`——用 since 减 1 去凑一个排他
//     边界，恰好反过来印证了 before 是排他的（若 before 本身就是闭区间下界，
//     ccxt 不需要多减这 1）。本包用同样的手法（start-1 / windowEnd+1）模拟
//     闭区间，见 rest.go 的 Klines。
//     **排序方向：官方原文没有像 Binance 那样直接写"chronological order"，
//     本次核实无法只从文档原文断定，改用 ccxt 源码交叉验证**——ccxt
//     python/ccxt/okx.py 里 fetchOHLCV 方法上方注释给的真实响应样例是
//     `["1678928760000",...], ["1678928700000",...], ["1678928640000",...]`，
//     三个时间戳依次递减，即**服务端返回的是倒序（最新的一根在前）**；
//     结合 before 参数说明"单独使用 before 时返回最新数据"这个措辞（暗示
//     不加任何游标参数时,数据本就是从最新往回排的），两个独立来源指向同一
//     结论。**exchange.Exchange.Klines 的返回契约要求正序，本包必须内部
//     反转**，已在 rest.go 实现，并单独写了
//     TestKlines_ReversesDescendingResponseToAscending 覆盖这一步（不允许
//     编排层看到这个差异）。
//
//  6. 心跳：客户端主动发送，服务端不主动 ping。原文（"Connect" 小节）：
//     "The connection will break automatically if the subscription is not
//     established or data has not been pushed for more than 30 seconds."
//     "To keep the connection stable: 1. Set a timer of N seconds whenever
//     a response message is received, where N is less than 30. 2. If the
//     timer is triggered... send the String 'ping'. 3. Expect a 'pong' as
//     a response."——这条规则在 Connect 小节里统一给出，未按 public/
//     business/private 分别注明不同数值，本包对两类端点（public 与
//     business）的 ConnPlan 都填 ClientPing="ping"、PingEvery=20s（<30s，
//     留 10s 余量，见 ws.go clientPingInterval），与简报给出的建议值一致。
//     pong 是纯文本 "pong"（不是 JSON），Decode 必须在尝试 json.Unmarshal
//     之前特判掉，否则会被当成一次真正的解析失败报错。
//
// # 与简报不符 / 简报未提及、核实阶段才发现的几处（均已按下方结论实现）
//
//   - candle 频道走 /ws/v5/business、不是简报暗示的 /ws/v5/public（第 1 条），
//     是本轮影响最大的一处发现：不处理会导致 K 线订阅发到错误的端点，交易所
//     大概率直接不推送数据，且不一定会用一条显式的错误帧告知。
//   - "单连接订阅数上限" 简报要求核实 configs/price.yaml 现值是否与文档一致，
//     但官方文档压根没有这个维度的数字，"核实结论"因此是"无据可比"而不是
//     "符合"或"不符合"（第 2 条），如实记录，未编造一个数字去对齐 240。
//   - REST 出错时 HTTP 状态码通常仍是 200，真正的错误信号在响应体的 code
//     字段（"Error Codes" 小节所列错误码对应的 HTTP status code 均为
//     200）——与 Binance 用 HTTP 状态码传递错误不同，本包的 Instruments/
//     Klines 因此都要多检查一层响应体 code（见 rest.go decodeEnvelope），
//     否则会把业务错误响应体误当空数据处理，静默漏采集。
//
// # tickers/books5 频道大小写折叠核对结论（对应任务简报的第一条教训）
//
// encoding/json 对结构体里没有精确 tag 命中的 JSON 键会退化成大小写不敏感
// 匹配，Binance 的 kline（t/T、v/V、q/Q、l/L）与 ticker（c/C、b/B、a/A、l/L）
// 报文都踩过这个坑（同类型的字段对会被静默覆盖，不报任何错，细节见 binance
// 包 ws.go 的 eventHead/klineFrame/tickerFrame 注释）。OKX 这边逐类核对如下：
//
//   - candle 频道：推送与两个 REST 端点的报文都是**数组**，按下标（[0]=ts，
//     [1]=o……）取值，不经过 encoding/json 的字段名匹配，天生不受这个坑影响
//     （简报已指出这点，本次实现直接验证：ws.go/rest.go 的 candle 解析全程
//     没有一个 json 结构体 tag）。
//   - tickers 频道：把 "WS / Tickers channel" 小节 Push Data Example 的
//     全部字段名逐一列出并转小写去重：instType、instId、last、lastSz、
//     askPx、askSz、bidPx、bidSz、open24h、high24h、low24h、volCcy24h、
//     vol24h、sodUtc0、sodUtc8、ts——**16 个字段名互不相同，转小写后也没有
//     任何两个字段变得相同**，即不存在"仅大小写不同"的字段对，逐一核对完毕，
//     没有发现折叠撞车对，故本包的 tickerData 类型（见 ws.go）无需像 Binance
//     那样加占位字段。
//   - books5 频道：Push Data Example 的字段名为 asks、bids、instId、ts、
//     seqId（checksum 字段官方样例里只出现在 books/books-l2-tbt/
//     books50-l2-tbt，books5 样例没有）——同样是清一色的多字符完整单词，
//     两两互不相同，不存在折叠撞车对。
//
// 结论：**OKX 这三个公共行情频道（candle/tickers/books5）都不存在
// encoding/json 大小写折叠风险**，本包因此不需要 Binance 那样的占位字段
// 防御——但这是"核对过，确实没有"，不是"假设没有就不核对"，核对方法与结论
// 已如上逐字段列出，供复核。
package okx
