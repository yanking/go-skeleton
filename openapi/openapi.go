// Package openapi 把 buf 生成的 OpenAPI 文档嵌进二进制，供服务在运行期对外提供。
//
// 本目录下 {service}/v{n}/*.openapi.json 是生成物、禁手改（宪法第一条）；
// 只有本文件是手写的，它不修改任何生成物，只提供一个读取入口。
package openapi

import (
	"embed"
	"fmt"
)

//go:embed */v*/*.openapi.json
var specs embed.FS

// MustSpec 取指定服务与版本的 OpenAPI 文档。取不到即 panic——
// 文档是构建期就该存在的产物，运行期缺失说明构建链没跑全，属装配期错误。
func MustSpec(service, version string) []byte {
	name := fmt.Sprintf("%s/%s/%s.openapi.json", service, version, service)
	data, err := specs.ReadFile(name)
	if err != nil {
		panic(fmt.Errorf("读取 OpenAPI 文档 %s: %w", name, err))
	}
	return data
}
