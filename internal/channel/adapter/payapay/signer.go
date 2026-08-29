package payapay

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// createSign 生成 MD5 签名：参数按 key 字典序拼 k=v& 串，跳过 sign 键与空值，
// 尾接 &key=secret 后取 MD5 小写十六进制。
func createSign(params map[string]any, secret string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || v == nil {
			continue
		}
		if valStr := fmt.Sprintf("%v", v); valStr != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+fmt.Sprintf("%v", params[k]))
	}
	stringSignTemp := strings.Join(pairs, "&") + "&key=" + secret

	h := md5.Sum([]byte(stringSignTemp))
	return hex.EncodeToString(h[:])
}
