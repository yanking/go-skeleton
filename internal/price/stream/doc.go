// Package stream 管理一条 WebSocket 连接的完整生命周期：拨号、心跳、指数退避
// 重连、订阅集变更时的连接重建。具体交易所（binance/okx 等）只产出
// exchange.ConnPlan 与实现 Decoder，怎么连、断线怎么办统一由本包处理，
// 使后续每接入一家交易所都复用同一套重连纪律。
package stream
