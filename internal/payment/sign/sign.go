// Package sign 实现商户签名规范：请求消息的全部标量字段（含空值/零值）按字段名升序
// 拼接为 k=v&… 规范串，用商户密钥做 HMAC-SHA256 得到十六进制小写签名。
// 空值字段必须参与拼接与验签——若允许剥离空值字段，攻击者可通过删除未设置的可选字段
// 复用旧签名发起重放请求，故本包不做「跳过空值」这类看似合理的优化。
//
// 拼接串中的 & 与 = 不做任何转义，字段值本身可能含这两个字符（如 notify_url 的
// query 串）。这不构成拼接歧义漏洞：服务端始终按固定的 proto 字段键集重算规范串，
// 不反向解析拼接串本体，值内的 & / = 无法伪造出额外字段或篡改字段边界。
//
// FieldsFromProto 只接受标量字段：repeated/map/message 属结构类型，直接跳过（不算漏签，
// 本仓库当前契约的请求消息全部由标量字段组成；新增复合类型字段需要另行设计签名规则，
// 不在本包范围）。但标量当中本包未支持的 Kind（double/float/enum/bytes 等）一律
// 拒签——返回 error，绝不静默跳过：静默跳过会让日后新增这类字段的人毫无察觉地
// 打开一个「加个字段就能绕过签名」的口子。
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Canonical 把字段集合按字段名 ASCII 升序拼接为 "k=v&k=v…" 规范串（末尾不带 &）。
// 空值字段仍以 "k=" 形式参与拼接，见包注释「为什么空值必须参与」。
func Canonical(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+fields[k])
	}
	return strings.Join(pairs, "&")
}

// HMAC 用 secret 对 canonical 规范串做 HMAC-SHA256，返回十六进制小写摘要。
func HMAC(secret, canonical string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(canonical))
	return hex.EncodeToString(h.Sum(nil))
}

// Verify 校验 fields 在 secret 下重算的签名是否等于 sig。
// 空 secret 或空 sig 一律判为验签失败：空密钥的 HMAC 是任何人都能公开算出的常量，
// 一旦放行等于对该商户完全不设防，故在比较前就地拒绝，不进入 hmac.Equal。
// 非空场景仍用 hmac.Equal 做常数时间比较，避免签名比对本身成为时序侧信道。
func Verify(secret string, fields map[string]string, sig string) bool {
	if secret == "" || sig == "" {
		return false
	}
	expected := HMAC(secret, Canonical(fields))
	return hmac.Equal([]byte(expected), []byte(sig))
}

// FieldsFromProto 用 protoreflect 遍历 m 的描述符全部字段（而非仅已设置字段），
// 取字符串形式的十进制字面量（bool 取 "true"/"false"，整数取十进制，不带 protojson
// 对 64 位整数额外加的引号；string 取原文）；proto3 未显式设置的标量字段按零值形态
// （"0"/"false"/""）计入，与商户按固定字段集拼串的结果保持一致。字段名用 proto 字段名
// （snake_case）。名为 "sign" 的字段单独作为 sig 返回，不进 fields；repeated/map/message
// 类型字段跳过（结构类型，不算漏签）；double/float/enum/bytes 等未支持的标量 Kind 返回
// error 拒签（见包注释「为什么拒签而不是跳过」）。
func FieldsFromProto(m proto.Message) (fields map[string]string, sig string, err error) {
	refl := m.ProtoReflect()
	fds := refl.Descriptor().Fields()

	fields = make(map[string]string, fds.Len())
	for i := 0; i < fds.Len(); i++ {
		fd := fds.Get(i)
		if fd.IsList() || fd.IsMap() ||
			fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			continue
		}

		val, ok := scalarString(fd.Kind(), refl.Get(fd))
		if !ok {
			return nil, "", fmt.Errorf("不支持的签名字段类型：字段 %s 的类型 %s 未纳入签名规则", fd.Name(), fd.Kind())
		}

		name := string(fd.Name())
		if name == "sign" {
			sig = val
			continue
		}
		fields[name] = val
	}
	return fields, sig, nil
}

// scalarString 把标量字段值转换为参与签名的字符串表示；
// ok 为 false 表示该 Kind 不是本包支持的标量种类（如 double/float/enum/bytes），
// 调用方（FieldsFromProto）须据此拒签返回 error，不能静默跳过。
func scalarString(kind protoreflect.Kind, v protoreflect.Value) (string, bool) {
	switch kind {
	case protoreflect.BoolKind:
		return strconv.FormatBool(v.Bool()), true
	case protoreflect.StringKind:
		return v.String(), true
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return strconv.FormatInt(v.Int(), 10), true
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(v.Uint(), 10), true
	default:
		return "", false
	}
}
