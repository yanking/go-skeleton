// Package secrets 用一条测试守住宪法第 4 条：凭证在配置里只能写成 ${ENV_NAME}
// 引用，明文不入库。
//
// 这条宪法原先只有人的纪律，没有任何机器判据——而仓库历史上真实的第三方渠道
// 凭证（后台口令、client_secret、商户号）正是从 migrations 的 seed_*.sql 混进来的，
// 文件注释还自称「仅本地/演示用」，直到转公开仓前逐个文件核查才发现。注释里的
// 自我声明不是判据，能跑的检查才是。
//
// 这里刻意不引入通用的 secret 扫描工具：那类工具带着几百条厂商规则与一整棵依赖
// 树（实测 gitleaks 与本仓 tools 模块里的 golangci-lint 依赖直接打架编译不过），
// 而本仓要守的只有一条很具体的规则。规则窄，误报才少，开发者才不会去关掉它。
//
// 本测试随 go test ./... 跑，于是宪法第 4 条的判据自动落在 make check（第 5 条的
// 提交门槛）里，不必另设 make 目标、也不必修订宪法。
package secrets

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot 仓库根目录相对本测试包的位置。
const repoRoot = "../.."

// scanExts 要查的文件后缀：声明式配置与数据库种子数据，凭证最容易混进来的地方。
// .tpl 是 new-service 的配置模板——模板里写错，每个派生服务都跟着错。
var scanExts = map[string]bool{
	".yaml": true, ".yml": true, ".tpl": true,
	".sql": true, ".env": true, ".toml": true, ".json": true,
}

// skipDirs 不查的目录：产物、依赖、工具二进制与 AI 工作台。
var skipDirs = map[string]bool{
	".git": true, "bin": true, "gen": true, "openapi": true,
	"node_modules": true, "vendor": true, ".superpowers": true, ".zcode": true,
}

// credentialKey 匹配「看起来在放凭证」的键名或列名。
//
// 边界不能用 \b：下划线是单词字符，于是 \bsecret\b 匹配不到 client_secret、
// app_secret、api_token 这类复合名——而本仓历史上真实泄露的字段正是 client_secret。
// 改成「前后不是字母数字」，复合名因此覆盖，而 passthrough 这类不含凭证语义的
// 词仍不会命中（pass 后面紧跟字母）。
var credentialKey = regexp.MustCompile(`(?i)(^|[^a-z0-9])(pass(word|wd)?|secret|token|api[-_]?key|access[-_]?key|private[-_]?key|credential)([^a-z0-9]|$)`)

// dsnCredential 匹配 URL 形态连接串里的明文口令：scheme://user:pass@host。
// 这类值的键名是 write / read / addr / url，凭证藏在值里，键名规则看不见它——
// 而 pkg/mysql、pkg/pgsql 的 DSN 字段正是这个形态。
var dsnCredential = regexp.MustCompile(`://[^:/@\s]+:([^@/\s]+)@`)

// envRef 合法的环境变量引用，与 pkg/conf 的文法一致（只认 ${NAME}）。
var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// placeholder 公认的占位符：明确不是真凭证，留着比留空更能说明「这里要填什么」。
var placeholder = regexp.MustCompile(`(?i)^(|-|""|''|null|nil|none|changeme|change_me|replace_me[a-z0-9_]*|placeholder|your[-_].*|xxx+|\*+|<[^>]*>)$`)

// exemptMark 行内豁免标记。宪法第 4 条允许「仅本机一次性环境（如本地容器）的默认
// 口令」写明文，但必须显式写出理由——"注释自称仅供演示"正是上次凭证泄露的掩护，
// 所以豁免要带上这个标记，评审时一眼看得到。
const exemptMark = "nosecret:"

// TestNoPlaintextCredentials 扫描声明式配置与种子数据，凡是凭证类键名的值既不是
// ${ENV} 引用、也不是占位符的，一律报违规。
func TestNoPlaintextCredentials(t *testing.T) {
	var scanned int
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !scanExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		scanned++
		return scanFile(t, path)
	})
	if err != nil {
		t.Fatalf("遍历仓库: %v", err)
	}
	if scanned == 0 {
		t.Fatal("一个文件都没扫到：检查 repoRoot 与 scanExts 是否算错了")
	}
	t.Logf("已扫 %d 个配置与种子文件", scanned)
}

// scanFile 查一个文件。SQL 另走一套判据，见 scanSQL。
func scanFile(t *testing.T, path string) error {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	rel, err := filepath.Rel(repoRoot, path)
	if err != nil {
		rel = path
	}
	if strings.EqualFold(filepath.Ext(path), ".sql") {
		return scanSQL(t, f, filepath.ToSlash(rel))
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		if msg, bad := violation(scanner.Text()); bad {
			t.Errorf("%s:%d %s（宪法第 4 条：凭证只能写 ${ENV_NAME} 引用；"+
				"仅本机一次性环境的默认口令可明文，但要在行尾写 %s<理由>）",
				filepath.ToSlash(rel), line, msg, exemptMark)
		}
	}
	return scanner.Err()
}

// writeStmt 匹配会把数据写进库的语句。凭证要入库总得有一条这样的语句，
// 纯 DDL 文件（只有 CREATE TABLE 的列定义）因此不在查的范围内。
var writeStmt = regexp.MustCompile(`(?i)(^|[^a-z])(insert|update|upsert|merge|copy)([^a-z]|$)`)

// scanSQL 种子数据的判据：文件里既有写语句、又出现凭证类列名，就要求显式豁免。
//
// 为什么不逐值判断：SQL 的 INSERT 把列名与值分在两处写
// （INSERT INTO t (id, client_secret) VALUES (1, 'abc')），逐行的 k=v 规则认不出
// 它们的对应关系——而本仓历史上真实泄露的恰是这个形状。改成「有凭证列名就要
// 显式表态」：种子数据里放凭证这件事本身就该被评审看见，而不是靠猜某个字面量
// 是真凭证还是假数据。
//
// 代价是全是假值的演示数据也要写一行豁免。这是刻意的：上一次泄露的掩护正是
// 「文件注释自称仅供演示」，而那些值取自真实配置。写豁免要给理由，评审看得到。
//
// 纯 DDL 文件放过（见 writeStmt）：`client_secret VARCHAR(64) NOT NULL` 这样的
// 列定义里一个值都没有，要求它写豁免只是噪音——而噪音会让人去关掉检查。
func scanSQL(t *testing.T, f *os.File, rel string) error {
	t.Helper()
	var (
		hits     []string
		exempt   bool
		hasWrite bool
		scanner  = bufio.NewScanner(f)
	)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.Contains(text, exemptMark) {
			exempt = true
			continue
		}
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if writeStmt.MatchString(text) {
			hasWrite = true
		}
		if m := credentialKey.FindString(text); m != "" {
			hits = append(hits, fmt.Sprintf("%s:%d %q", rel, line, m))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(hits) > 0 && hasWrite && !exempt {
		t.Errorf("%s 出现凭证类列名（%s），种子数据里的凭证须显式表态："+
			"在文件里写一行注释带 %s<理由>，说明这些值为何不是真凭证"+
			"（宪法第 4 条；本仓历史上真实渠道凭证正是从种子数据混进来的）",
			rel, strings.Join(hits, "、"), exemptMark)
	}
	return nil
}

// violation 判断一行是否违规，返回给人看的说明。
func violation(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	// 整行注释不算：注释里写键名是在解释用法。SQL 的 -- 与 yaml/toml 的 # 都算。
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "--") ||
		strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
		return "", false
	}
	if strings.Contains(line, exemptMark) {
		return "", false
	}

	key, rawValue, ok := splitAssignment(trimmed)
	if !ok {
		return "", false
	}
	value := cleanValue(rawValue)

	// 先查 URL 形态连接串里内嵌的口令：它与键名无关。
	if m := dsnCredential.FindStringSubmatch(value); m != nil {
		if secret := m[1]; !placeholder.MatchString(secret) && !envRef.MatchString(secret) {
			return "连接串里内嵌了明文口令", true
		}
	}

	if !credentialKey.MatchString(key) {
		return "", false
	}
	if value == "" || placeholder.MatchString(value) || envRef.MatchString(value) {
		return "", false
	}
	return fmt.Sprintf("凭证类字段 %q 像是明文值", strings.TrimSpace(key)), true
}

// splitAssignment 把一行拆成键与值，认 yaml 的 `k: v` 与 sql/env/toml 的 `k = v`。
// 取第一个分隔符：值里再出现冒号（URL、DSN）不影响键的判定。
func splitAssignment(line string) (key, value string, ok bool) {
	colon := strings.Index(line, ":")
	equal := strings.Index(line, "=")
	switch {
	case colon >= 0 && (equal < 0 || colon < equal):
		return line[:colon], line[colon+1:], true
	case equal >= 0:
		return line[:equal], line[equal+1:], true
	}
	return "", "", false
}

// cleanValue 去掉值两侧的引号、逗号、分号与行尾注释，只留要判定的那部分。
func cleanValue(value string) string {
	value = strings.TrimSpace(value)
	// 行尾注释：引号内的 # 不算，故只在值不以引号开头时才截。
	if !strings.HasPrefix(value, `"`) && !strings.HasPrefix(value, "'") {
		if i := strings.IndexAny(value, "#"); i >= 0 {
			value = value[:i]
		}
	}
	value = strings.TrimSpace(value)
	value = strings.TrimRight(value, ",;")
	value = strings.TrimSpace(value)
	return strings.Trim(value, `"'`)
}

// TestViolation 表驱动验证逐行判据。没有这组用例的话，整个扫描很容易变成一个
// 永远通过的空壳——而「扫过了」比「没扫」更危险。
func TestViolation(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		// 该拦住的：历史泄露与常见误用的形状。
		{name: "yaml 明文口令", line: `  password: "hunter2"`, want: true},
		{name: "yaml 明文 token", line: `  token: sk-live-abc123`, want: true},
		{name: "DSN 里的明文口令", line: `  write: "postgres://app:hunter2@db:5432/app"`, want: true},
		{name: "复合键名也要认", line: `  client_secret: abc123`, want: true},
		{name: "app_secret 同理", line: `  app_secret = "xyz"`, want: true},
		{name: "env 形态", line: `API_KEY=abc123def`, want: true},
		{name: "toml 形态", line: `client_secret = "s3cr3t"`, want: true},
		{name: "带连字符的键", line: `access-key: AKIAIOSFODNN7EXAMPLE`, want: true},

		// 该放过的。
		{name: "环境变量引用", line: `  password: "${DB_PASSWORD}"`, want: false},
		{name: "引用带前后缀也算", line: `  token: "Bearer ${SVC_TOKEN}"`, want: false},
		{name: "占位符", line: `  password: REPLACE_ME_DB_PASSWORD`, want: false},
		{name: "空值", line: `  password:`, want: false},
		{name: "尖括号占位", line: `  token: <your-token>`, want: false},
		{name: "整行注释", line: `# password: hunter2`, want: false},
		{name: "SQL 注释", line: `-- password: hunter2`, want: false},
		{name: "显式豁免", line: `  password: "localdev" # nosecret: 本地容器一次性口令`, want: false},
		{name: "不是凭证类键名", line: `  addr: "127.0.0.1:6379"`, want: false},
		{name: "DSN 口令是引用即放过", line: `  write: "postgres://app:${DB_PASSWORD}@db:5432/app"`, want: false},
		{name: "DSN 无口令段", line: `  write: "postgres://app@db:5432/app"`, want: false},
		{name: "mysql DSN 形态不带 scheme 不误报", line: `  write: "app@tcp(db:3306)/app?parseTime=true"`, want: false},
		{name: "键名含 pass 但不是单词", line: `  passthrough: true`, want: false},
		{name: "行尾注释不算值", line: `  password: "${P}" # 由部署侧注入`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, got := violation(tt.line)
			if got != tt.want {
				t.Errorf("violation(%q) = (%q, %v), want %v", tt.line, msg, got, tt.want)
			}
		})
	}
}

// TestScanSQLRequiresExemption 验证种子数据的判据：有凭证列名就要显式表态。
func TestScanSQLRequiresExemption(t *testing.T) {
	const withCredential = "INSERT INTO channels (id, client_secret) VALUES (1, 'abc123');\n"

	t.Run("无豁免即报违规", func(t *testing.T) {
		fake := &testing.T{}
		if err := scanSQL(fake, writeTemp(t, withCredential), "seed.sql"); err != nil {
			t.Fatal(err)
		}
		if !fake.Failed() {
			t.Error("种子数据出现 client_secret 却没有豁免标记，应报违规")
		}
	})

	t.Run("带豁免即放过", func(t *testing.T) {
		content := "-- nosecret: 全为假值，域名用 .invalid、密钥为占位符\n" + withCredential
		fake := &testing.T{}
		if err := scanSQL(fake, writeTemp(t, content), "seed.sql"); err != nil {
			t.Fatal(err)
		}
		if fake.Failed() {
			t.Error("带了豁免标记不应报违规")
		}
	})

	t.Run("纯 DDL 不要求豁免", func(t *testing.T) {
		// 列定义里一个值都没有，要求写豁免只是噪音。
		ddl := "CREATE TABLE channels (\n  id BIGSERIAL PRIMARY KEY,\n  client_secret VARCHAR(64) NOT NULL\n);\n"
		fake := &testing.T{}
		if err := scanSQL(fake, writeTemp(t, ddl), "create.sql"); err != nil {
			t.Fatal(err)
		}
		if fake.Failed() {
			t.Error("纯 DDL 文件不该被要求豁免")
		}
	})

	t.Run("没有凭证列名就不要求豁免", func(t *testing.T) {
		fake := &testing.T{}
		if err := scanSQL(fake, writeTemp(t, "INSERT INTO users (id, name) VALUES (1, 'a');\n"), "seed.sql"); err != nil {
			t.Fatal(err)
		}
		if fake.Failed() {
			t.Error("普通种子数据不该被要求豁免")
		}
	})
}

// writeTemp 把内容写进临时文件并返回打开的句柄。
func writeTemp(t *testing.T, content string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.sql")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
