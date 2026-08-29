// Package conf 提供服务配置加载：读取 YAML 文件并绑定到配置结构体。
// 仅暴露 MustLoad，供 main 装配期调用；任何失败直接 panic——起不来就死。
package conf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// validator 配置结构体的可选能力：实现后 MustLoad 在绑定完成时调用，
// 用于必填项校验，返回非 nil 即视为配置不可用。
type validator interface {
	Validate() error
}

// MustLoad 读取 YAML 配置文件 configFile 并绑定到 obj（必须是指针）。
// 文件中出现 obj 之外的未知键视为错误，以尽早暴露键名拼写问题。
// 若 obj 实现 Validate() error 则在绑定后调用。任何失败直接 panic。
func MustLoad(configFile string, obj any) {
	data, err := os.ReadFile(configFile)
	if err != nil {
		panic(fmt.Errorf("读取配置文件 %s: %w", configFile, err))
	}

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
