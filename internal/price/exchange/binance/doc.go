// Package binance 是 price 服务对接 Binance 现货行情的协议翻译层：把 exchange 包
// 定义的中立类型（Sub/ConnPlan/Event/Kline/Snapshot/Instrument）与 Binance 现货
// 原生报文互译。symbol 大小写、周期拼写、限速数字、连接上限等交易所方言只在本包
// 出现，不泄漏到 exchange 包之外——这是本包存在的唯一理由。
//
// 本包只做「协议翻译」：不持有连接、不做重连与心跳（见 internal/price/stream），
// 不做限速与重试（见 collector），Decode 对无法识别的帧一律返回零值 Event 与
// nil error（交易所会持续推送订阅确认、错误响应、本包未实现的流类型等帧，
// 它们不是错误，见 exchange.Exchange.Decode 的接口注释）。
//
// # 已核实的协议事实（核实日期：2026-08-28，来源：官方仓库
// github.com/binance/binance-spot-api-docs，master 分支，
// web-socket-streams.md 与 rest-api.md，而非 binance.us 文档）
//
//  1. 单条连接最多订阅 1024 条流："A single connection can listen to a
//     maximum of 1024 streams."（web-socket-streams.md 第 62 行）——
//     与 configs/price.yaml 的 max_streams_per_conn: 1024 一致，无需改配置。
//  2. 每秒入站消息上限 5 条，PING/PONG 帧与 JSON 控制消息（订阅/退订）均计入：
//     "WebSocket connections have a limit of 5 incoming messages per
//     second."（同文件第 57 行）。超限会被断开，重复超限的 IP 可能被封禁——
//     本包不做限速，限速是 collector/调用方的职责。
//  3. 单条连接存在 24 小时强制断开："A single connection to
//     stream.binance.com is only valid for 24 hours; expect to be
//     disconnected at the 24 hour mark."（同文件第 44 行）——
//     stream.Conn 的重连循环按普通断线处理即可，见 stream 包注释。
//  4. GET /api/v3/klines：limit 缺省 500、上限 1000（"Default: 500;
//     Maximum: 1000."，rest-api.md 第 1134 行）；未见该端点专门重复标注
//     startTime/endTime 是否 INCLUSIVE（该字样只在 aggTrades 一类端点出现），
//     但 rest-api.md 顶部 General API Information 给出的通用规则覆盖所有
//     按时间翻页的端点："With startTime, returns oldest items from
//     startTime up to the limit."、"With endTime, returns most recent
//     items up to endTime and the limit."（第 93-95 行）——按此本包把
//     [startTime, endTime] 两端都当闭区间处理：下一页起点取「最后一根的
//     开盘时间 + 一个周期」，避免起点等于上一页终点导致的重复。返回数组按
//     "Data is returned in chronological order"（第 92 行）已是开盘时间正序。
//
// # 核实中发现与任务简报不符、已按文档结论改写的一项
//
// 简报给出的「已核实事实」认为服务端 ping 间隔是 3 分钟、pong 超时 10 分钟
// （标注「不是 20 秒」）。现场重新核实 web-socket-streams.md 得到相反结论：
// 当前（截至核实日）文档明确写的是"The WebSocket server will send a ping
// frame every 20 seconds. If the WebSocket server does not receive a pong
// frame back from the connection within a minute the connection will be
// disconnected."（第 46-47 行）。进一步查 CHANGELOG.md 第 1052-1056 行确认
// 了这组数字的历史："Notice: These changes will be gradually rolled out
// between February 3, 2025 and February 14, 2025 ... Our WebSocket services
// will send a ping frame every 20 seconds instead of 3 minutes. The allowed
// pong delay will be every 1 minute instead of 10 minutes."——也就是说
// 3 分钟/10 分钟是 2025 年 2 月之前的旧值，简报的「已核实」结论已经过期。
// 本包按当前文档以 20 秒/1 分钟为准记录在此，仅供排障参考；这个数字不影响
// 任何代码路径——本包产出的 ConnPlan.ClientPing 恒为 nil，心跳由服务端主动
// 发起，pong 由 coder/websocket 自动应答，无论周期具体是多少。
package binance
