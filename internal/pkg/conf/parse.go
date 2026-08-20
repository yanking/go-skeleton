// Package conf 提供服务配置加载：YAML 文件解析＋ APP_ 前缀环境变量覆盖
// （映射规则见 .agent/engineering.md「配置」）。
// 仅暴露 MustLoad，供 main 装配期调用；任何失败直接 panic——起不来就死。
package conf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// envPrefix 参与配置覆盖的环境变量统一前缀。
const envPrefix = "APP_"

// validator 配置结构体的可选能力：实现后 MustLoad 会在绑定完成时调用，
// 用于必填项校验，返回非 nil 即视为配置不可用。
type validator interface {
	Validate() error
}

// MustLoad 读取 YAML 配置文件 configFile 并绑定到 obj（必须是指针），
// 随后应用 APP_ 前缀环境变量覆盖：APP_SERVER_GRPC_PORT 覆盖 server.grpc_port，
// 值按 YAML 标量解析（"9090"→int、"true"→bool），只覆盖文件中已存在的键，
// 因此配置样例文件应列全所有键。文件中出现 obj 之外的未知键视为错误。
// 若 obj 实现 Validate() error 则在最后调用。任何失败直接 panic。
func MustLoad(configFile string, obj any) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		panic(fmt.Errorf("读取配置文件 %s: %w", configFile, err))
	}

	// 先解析成通用 map 套用环境变量覆盖，再整体严格绑定进 obj：
	// 无需对 obj 做反射即可支持任意嵌套结构。
	raw := map[string]any{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		panic(fmt.Errorf("解析配置文件 %s: %w", configFile, err))
	}
	applyEnvOverrides(raw)

	merged, err := yaml.Marshal(raw)
	if err != nil {
		panic(fmt.Errorf("重编码配置 %s: %w", configFile, err))
	}
	dec := yaml.NewDecoder(bytes.NewReader(merged))
	dec.KnownFields(true) // 未知键即报错，尽早暴露配置文件里的拼写错误
	if err := dec.Decode(obj); err != nil && !errors.Is(err, io.EOF) {
		panic(fmt.Errorf("绑定配置 %s: %w", configFile, err))
	}

	if v, ok := obj.(validator); ok {
		if err := v.Validate(); err != nil {
			panic(fmt.Errorf("校验配置 %s: %w", configFile, err))
		}
	}
}

// applyEnvOverrides 把 APP_ 前缀环境变量写进 raw。
// 键名下划线既是层级分隔也可能是键自身的一部分（如 grpc_port），
// 用「贪心最长匹配」消歧：每层优先把剩余段整体拼成一个键查找，再逐步缩短。
func applyEnvOverrides(raw map[string]any) {
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, envPrefix) {
			continue
		}
		segs := strings.Split(strings.ToLower(strings.TrimPrefix(name, envPrefix)), "_")
		override(raw, segs, val)
	}
}

// override 在 m 中按贪心最长匹配定位 segs 对应的键并写入 val，返回是否命中；
// 未命中不改动 m（与本服务无关的 APP_ 变量被静默忽略）。
func override(m map[string]any, segs []string, val string) bool {
	for n := len(segs); n >= 1; n-- {
		key := strings.Join(segs[:n], "_")
		hit, ok := m[key]
		if !ok {
			continue
		}
		if n == len(segs) {
			m[key] = parseScalar(val)
			return true
		}
		child, ok := hit.(map[string]any)
		if !ok {
			continue // 命中的不是子表，换更短的键继续尝试
		}
		if override(child, segs[n:], val) {
			return true
		}
	}
	return false
}

// parseScalar 把环境变量值按 YAML 标量解析以获得自然类型；
// 解析失败或结果为空时按原样字符串处理。
func parseScalar(s string) any {
	var v any
	if err := yaml.Unmarshal([]byte(s), &v); err != nil || v == nil {
		return s
	}
	return v
}
