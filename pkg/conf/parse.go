// Package conf 提供服务配置加载：读取 YAML 文件、展开其中的 ${NAME} 环境变量引用，
// 再绑定到配置结构体。仅暴露 MustLoad，供 main 装配期调用；任何失败直接 panic——起不来就死。
//
// ${NAME} 是凭证进配置的通道（宪法第 4 条）：yaml 里写 password: "${DB_PASSWORD}"，
// 由部署侧经环境变量注入，明文不入库。展开是纯文本替换、发生在 YAML 解析之前，故：
//   - 引用处加引号，值含 : # 等 YAML 特殊字符才不会改变结构；双引号内 \ 是转义符，值含 \ 用单引号 '${NAME}'；值含引号本身或换行不支持；
//   - 文件任何位置（含注释）的 ${NAME} 都会展开，NAME 未设置即 panic，已设置为空串照常展开；
//   - 只认 ${NAME} 形式，$NAME 原样保留；其余 ${…} 写法（如 ${X:-d}、${a-b}、${}）不合文法即 panic；不提供转义。
package conf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// validator 配置结构体的可选能力：实现后 MustLoad 在绑定完成时调用，
// 用于必填项校验，返回非 nil 即视为配置不可用。
type validator interface {
	Validate() error
}

// envRef 匹配 ${...} 记号。先宽匹配再校验名字：${DB_PASSWORD:-x}、${db-pw} 这类写错的引用
// 若只因不合文法而被原样放过，字面串就成了凭证——对本地比对的 token 是静默放行。
// 只认带花括号的形式：$NAME 在 DSN、口令里可能是字面量，不展开它才不会误伤。
var envRef = regexp.MustCompile(`\$\{[^}\n]*\}`)

// envName 合法的变量名文法：字母或下划线开头，后接字母、数字、下划线。
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MustLoad 读取 YAML 配置文件 configFile，展开 ${NAME} 环境变量引用后绑定到 obj（必须是指针）。
// 文件中出现 obj 之外的未知键视为错误，以尽早暴露键名拼写问题。
// 若 obj 实现 Validate() error 则在绑定后调用。任何失败直接 panic。
func MustLoad(configFile string, obj any) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		panic(fmt.Errorf("读取配置文件 %s: %w", configFile, err))
	}
	data = expandEnv(data, configFile)

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// 空文件解码返回 io.EOF，视为「全部字段取零值」而非错误，交由 Validate 判定。
	if err := dec.Decode(obj); err != nil && !errors.Is(err, io.EOF) {
		panic(fmt.Errorf("绑定配置 %s: %w", configFile, err))
	}

	if v, ok := obj.(validator); ok {
		if err := v.Validate(); err != nil {
			panic(fmt.Errorf("校验配置 %s: %w", configFile, err))
		}
	}
}

// expandEnv 把 data 里的 ${NAME} 换成环境变量值。NAME 未设置即 panic——换成空串
// 等于把「漏配」伪装成「配了个空值」，恰是凭证类字段最危险的失败开放形态；
// 显式设成空串的变量照常展开，那是操作者的明确决定。不合文法的记号同样 panic。
func expandEnv(data []byte, configFile string) []byte {
	return envRef.ReplaceAllFunc(data, func(ref []byte) []byte {
		name := string(ref[2 : len(ref)-1])
		if !envName.MatchString(name) {
			panic(fmt.Errorf("展开配置 %s: 引用 %s 不合文法，只支持 ${NAME}（字母、数字、下划线）", configFile, ref))
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			panic(fmt.Errorf("展开配置 %s: 环境变量 %s 未设置", configFile, name))
		}
		return []byte(v)
	})
}
