// Command mockd 是 channel 服务端对端测试的 payapay 下游模拟器：实现
// payapay 适配器会调用的四个接口，报文外壳（code/msg + 业务字段）与真实渠道
// 一致，但不校验请求签名——签名正确性由适配器单测与回调用例覆盖。
// 行为按 order_no 关键字驱动：SUCC 终态成功、FAIL 终态失败、ERR 渠道业务
// 拒绝（code != 200），其余处理中；用例语义靠 run.sh 的单号命名表达。
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// E2E 用断言值：查询金额与余额的固定回包值，run.sh 侧按此断言。
const (
	queryAmount  = 5000
	mockBalance  = 888888
	mockFrozen   = 77
	rejectedCode = 429
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18099", "监听地址")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/payApi/CreatePayInOrder", payIn(logger))
	mux.HandleFunc("POST /api/v1/payApi/CreatePayOutOrder", payOut(logger))
	mux.HandleFunc("POST /api/v1/payApi/QueryOrder", query(logger))
	mux.HandleFunc("POST /api/v1/payApi/QueryBalance", balance(logger))

	logger.Info("mockd 下游模拟器就绪", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		logger.Error("mockd 退出", "err", err)
	}
}

// order 读取请求里的 order_no 并留一行访问日志，供 E2E 输出佐证全链路真实走到。
func order(r *http.Request) string {
	var body struct {
		OrderNo string `json:"order_no"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.OrderNo
}

// writeJSON 统一回包。
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// payIn 代收下单：成功回支付链接与渠道单号，均回显 order_no 供端到端断言穿透性。
func payIn(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := order(r)
		logger.Info("CreatePayInOrder", "order_no", no)
		if strings.Contains(no, "ERR") {
			writeJSON(w, map[string]any{"code": rejectedCode, "msg": "mock rejected"})
			return
		}
		writeJSON(w, map[string]any{
			"code": 200, "msg": "ok",
			"pay_url":      "https://mock.local/pay/" + no,
			"dis_order_no": "PAY-" + no,
		})
	}
}

// payOut 代付下单：回渠道单号，回显 order_no。
func payOut(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := order(r)
		logger.Info("CreatePayOutOrder", "order_no", no)
		if strings.Contains(no, "ERR") {
			writeJSON(w, map[string]any{"code": rejectedCode, "msg": "mock rejected"})
			return
		}
		writeJSON(w, map[string]any{"code": 200, "msg": "ok", "dis_order_no": "PO-" + no})
	}
}

// query 订单查询：SUCC→渠道状态 2（成功）、FAIL→3（失败）、ERR→业务码拒绝、
// 其余→1（处理中）；real_price/utr2 回显单号供断言。
func query(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		no := order(r)
		logger.Info("QueryOrder", "order_no", no)
		switch {
		case strings.Contains(no, "ERR"):
			writeJSON(w, map[string]any{"code": rejectedCode, "msg": "mock rejected"})
		case strings.Contains(no, "SUCC"):
			writeJSON(w, map[string]any{
				"code": 200, "msg": "ok", "status": 2, "real_price": queryAmount,
				"order_no": no, "dis_order_no": "Q-" + no, "utr2": "UTR-" + no,
			})
		case strings.Contains(no, "FAIL"):
			writeJSON(w, map[string]any{
				"code": 200, "msg": "ok", "status": 3, "real_price": queryAmount,
				"order_no": no, "dis_order_no": "Q-" + no,
			})
		default:
			writeJSON(w, map[string]any{
				"code": 200, "msg": "ok", "status": 1, "real_price": queryAmount,
				"order_no": no, "dis_order_no": "Q-" + no,
			})
		}
	}
}

// balance 余额查询：固定值。
func balance(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger.Info("QueryBalance")
		writeJSON(w, map[string]any{
			"code": 200, "msg": "ok", "balance": mockBalance, "balance_frozen": mockFrozen,
		})
	}
}
