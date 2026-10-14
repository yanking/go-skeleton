// conf 只有 MustLoad 一个导出行为，按 go-style 用 _test 包做黑盒测试。
package conf_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yanking/go-skeleton/pkg/conf"
)

// dbConf 测试用嵌套配置段。
type dbConf struct {
	DSN         string `yaml:"dsn"`
	MaxOpenConn int    `yaml:"max_open_conn"`
}

// serverConf 测试用配置结构，覆盖字符串、整型、布尔、切片与嵌套结构。
type serverConf struct {
	Name     string   `yaml:"name"`
	GRPCPort int      `yaml:"grpc_port"`
	Debug    bool     `yaml:"debug"`
	Tags     []string `yaml:"tags"`
	DB       dbConf   `yaml:"db"`
}

// validatingConf 带必填校验的配置，验证 MustLoad 对 Validate 的调用。
type validatingConf struct {
	Name string `yaml:"name"`
}

// Validate 校验必填项 name。
func (c *validatingConf) Validate() error {
	if c.Name == "" {
		return errors.New("name 必填")
	}
	return nil
}

// writeFile 把内容写进临时目录并返回路径。
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hello.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时配置: %v", err)
	}
	return path
}

// mustPanic 断言 fn 会 panic。
func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("期望 panic，实际正常返回")
		}
	}()
	fn()
}

func TestMustLoad(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want serverConf
	}{
		{
			name: "各类型与嵌套结构绑定",
			yaml: `
name: user
grpc_port: 9090
debug: true
tags:
  - core
  - beta
db:
  dsn: postgres://localhost/dev
  max_open_conn: 20
`,
			want: serverConf{
				Name:     "user",
				GRPCPort: 9090,
				Debug:    true,
				Tags:     []string{"core", "beta"},
				DB:       dbConf{DSN: "postgres://localhost/dev", MaxOpenConn: 20},
			},
		},
		{
			name: "缺省字段取零值",
			yaml: "name: user\n",
			want: serverConf{Name: "user"},
		},
		{
			name: "空文件全部取零值",
			yaml: "",
			want: serverConf{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got serverConf
			conf.MustLoad(writeFile(t, tt.yaml), &got)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("配置不符 (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMustLoadPanic(t *testing.T) {
	tests := []struct {
		name string
		fn   func()
	}{
		{
			name: "文件不存在",
			fn: func() {
				var c serverConf
				conf.MustLoad(filepath.Join(t.TempDir(), "absent.yaml"), &c)
			},
		},
		{
			name: "非法 YAML",
			fn: func() {
				var c serverConf
				conf.MustLoad(writeFile(t, "name: [\n"), &c)
			},
		},
		{
			name: "类型不匹配",
			fn: func() {
				var c serverConf
				conf.MustLoad(writeFile(t, "grpc_port: not-a-number\n"), &c)
			},
		},
		{
			name: "文件含未知键",
			fn: func() {
				var c serverConf
				conf.MustLoad(writeFile(t, "name: user\ntypo_key: 1\n"), &c)
			},
		},
		{
			name: "Validate 必填校验失败",
			fn: func() {
				var c validatingConf
				conf.MustLoad(writeFile(t, "name: \"\"\n"), &c)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mustPanic(t, tt.fn)
		})
	}
}

// TestMustLoadValidateOK 验证 Validate 通过时正常返回。
func TestMustLoadValidateOK(t *testing.T) {
	var got validatingConf
	conf.MustLoad(writeFile(t, "name: user\n"), &got)
	if want := "user"; got.Name != want {
		t.Errorf("Name = %q, want %q", got.Name, want)
	}
}

// envConf 覆盖字符串与整型两种目标：展开是纯文本替换，数字也能从环境变量来。
type envConf struct {
	Password string `yaml:"password"`
	Port     int    `yaml:"port"`
	Literal  string `yaml:"literal"`
}

// TestMustLoadExpandsEnv 守住 ${NAME} 的展开规则：带引号的值里含 YAML 特殊字符也安全，
// 已设置为空串的变量照常展开，$NAME 形式原样保留。
func TestMustLoadExpandsEnv(t *testing.T) {
	t.Setenv("CONF_TEST_PASSWORD", "s3cret:#x")
	t.Setenv("CONF_TEST_PORT", "9090")
	t.Setenv("CONF_TEST_EMPTY", "")

	tests := []struct {
		name string
		yaml string
		want envConf
	}{
		{
			name: "带引号的字符串与裸数字都展开",
			yaml: "password: \"${CONF_TEST_PASSWORD}\"\nport: ${CONF_TEST_PORT}\n",
			want: envConf{Password: "s3cret:#x", Port: 9090},
		},
		{
			name: "已设置为空串的变量展开成空串而非 panic",
			yaml: "password: \"${CONF_TEST_EMPTY}\"\n",
			want: envConf{},
		},
		{
			name: "不带花括号的 $NAME 原样保留",
			yaml: "literal: \"$CONF_TEST_PASSWORD\"\n",
			want: envConf{Literal: "$CONF_TEST_PASSWORD"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got envConf
			conf.MustLoad(writeFile(t, tt.yaml), &got)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("配置不符 (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMustLoadPanicsOnUnsetEnv 守住失败关闭：未设置的变量不能静默展开成空串——
// 那等于把「漏配」伪装成「配了个空值」，凭证类字段最危险的形态；panic 信息须点名变量。
func TestMustLoadPanicsOnUnsetEnv(t *testing.T) {
	var c envConf
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("未设置的环境变量应 panic，实际正常返回")
		}
		if !strings.Contains(fmt.Sprint(r), "CONF_TEST_UNSET_VAR") {
			t.Fatalf("panic 信息未点名变量: %v", r)
		}
	}()
	conf.MustLoad(writeFile(t, "password: \"${CONF_TEST_UNSET_VAR}\"\n"), &c)
}
