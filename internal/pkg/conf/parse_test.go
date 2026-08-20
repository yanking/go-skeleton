// conf 只有 MustLoad 一个导出行为，按 go-style 用 _test 包做黑盒测试。
package conf_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yanking/go-skeleton/internal/pkg/conf"
)

// dbConf 测试用嵌套配置段。
type dbConf struct {
	DSN string `yaml:"dsn"`
}

// serverConf 测试用配置结构，覆盖字符串、整型、布尔与嵌套、含下划线键名。
type serverConf struct {
	Name     string `yaml:"name"`
	GRPCPort int    `yaml:"grpc_port"`
	Debug    bool   `yaml:"debug"`
	DB       dbConf `yaml:"db"`
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
	path := filepath.Join(t.TempDir(), "config.yaml")
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

const baseYAML = `
name: user
grpc_port: 9090
debug: false
db:
  dsn: postgres://localhost/dev
`

func TestMustLoad(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		want serverConf
	}{
		{
			name: "纯文件解析",
			yaml: baseYAML,
			want: serverConf{Name: "user", GRPCPort: 9090, Debug: false, DB: dbConf{DSN: "postgres://localhost/dev"}},
		},
		{
			name: "环境变量覆盖各类型与嵌套",
			yaml: baseYAML,
			env: map[string]string{
				"APP_GRPC_PORT": "8081",              // 含下划线的键：贪心匹配 grpc_port
				"APP_DEBUG":     "true",              // 布尔
				"APP_DB_DSN":    "mysql://prod/main", // 嵌套 db.dsn
			},
			want: serverConf{Name: "user", GRPCPort: 8081, Debug: true, DB: dbConf{DSN: "mysql://prod/main"}},
		},
		{
			name: "无关与未匹配的环境变量被忽略",
			yaml: baseYAML,
			env: map[string]string{
				"APP_NOT_EXIST": "1",
				"OTHERPREFIX":   "2",
			},
			want: serverConf{Name: "user", GRPCPort: 9090, DB: dbConf{DSN: "postgres://localhost/dev"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
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
