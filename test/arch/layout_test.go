package arch

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// layerNames 是服务内的分层目录名：AGENTS.md「服务分层」六件套里除 config 以外的五层。
// config 在两种布局里都留在服务级，所以不在其列，按共享包处理。
var layerNames = []string{"handler", "service", "repo", "model", "job"}

// check 检查 root 下 internal/ 的目录布局与模块边界，modulePath 是 root 所在 Go module 的路径
// （go.mod 的 module 行）。internal/ 下每个一级目录是一个服务，services 是识别出的服务数；
// violations 是按字符串排序的违规描述，其中的文件与目录路径都相对 root、以斜杠分隔。
// internal/ 不存在时返回 nil、0、nil；读目录或解析 Go 文件失败作为 err 返回，绝不当作「无违规」。
//
// 服务下的一级子目录分三类：名字是层名的是平铺层；自己含 service 子目录的是模块（service 是模块
// 唯一的对外入口，脚本生成的模块都有它）；其余是服务级共享包。所以共享包与平铺服务的额外子包
// 里不能建名为 service 的子目录，否则会被当成模块；叫 model 之类其他层名的子目录则无妨。
// 查三类违规：
//   - 同一服务既有平铺层又有模块；
//   - 模块 A 的文件引用了模块 B 的包，而引用的不是 B 的 service 包本身；
//   - 分模块服务的共享包引用了任何模块。
//
// 只查同一服务内部的引用，跨服务的引用不在检查范围；遍历时跳过 testdata 与以 . 或 _ 开头的目录
// （与 go 工具一致）。服务根目录下直接放的 Go 文件不属于任何一类，不受检。
func check(root, modulePath string) (violations []string, services int, err error) {
	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	for _, e := range entries {
		if !e.IsDir() || skipDir(e.Name()) {
			continue
		}
		services++
		vs, err := checkService(root, modulePath, e.Name())
		if err != nil {
			return nil, 0, err
		}
		violations = append(violations, vs...)
	}
	slices.Sort(violations)
	return violations, services, nil
}

// checkService 检查 internal/<svc>/ 这一个服务：先判布局有没有混用，再逐文件查模块边界。
func checkService(root, modulePath, svc string) ([]string, error) {
	svcDir := filepath.Join(root, "internal", svc)
	flat, modules, err := classify(svcDir)
	if err != nil {
		return nil, err
	}
	var violations []string
	if len(flat) > 0 && len(modules) > 0 {
		violations = append(violations, fmt.Sprintf(
			"internal/%s：平铺层 %v 与模块 %v 混用，先把平铺的代码迁进模块（见 AGENTS.md「服务分层」）",
			svc, flat, modules))
	}
	if len(modules) == 0 {
		return violations, nil // 两条引用规则只对分模块的服务生效
	}

	svcPrefix := "internal/" + svc + "/"
	importPrefix := modulePath + "/" + svcPrefix
	fset := token.NewFileSet()
	err = filepath.WalkDir(svcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// top 是文件所在的服务一级子目录：模块、平铺层或共享包。
		top, _, nested := strings.Cut(strings.TrimPrefix(rel, svcPrefix), "/")
		if !nested || slices.Contains(flat, top) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			target, ok := strings.CutPrefix(p, importPrefix)
			if !ok {
				continue
			}
			if v := violationOf(rel, top, target, modules); v != "" {
				violations = append(violations, v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return violations, nil
}

// classify 返回 svcDir 下的平铺层与模块，各自按名字排序（os.ReadDir 按文件名返回）；
// 其余一级子目录是服务级共享包，不单列。
func classify(svcDir string) (flat, modules []string, err error) {
	entries, err := os.ReadDir(svcDir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || skipDir(name) {
			continue
		}
		if slices.Contains(layerNames, name) {
			flat = append(flat, name)
			continue
		}
		isModule, err := hasServiceDir(filepath.Join(svcDir, name))
		if err != nil {
			return nil, nil, err
		}
		if isModule {
			modules = append(modules, name)
		}
	}
	return flat, modules, nil
}

// hasServiceDir 报告 dir 的一级子目录里有没有名为 service 的，有就是模块。只认 service、不认别的层名：
// 每个模块都有 service（唯一的对外入口），而平铺服务的 adapter/model、共享包下的 model 这类
// 自然写法也带层名目录，不能因此被当成模块。
func hasServiceDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(entries, func(e fs.DirEntry) bool {
		return e.IsDir() && e.Name() == "service"
	}), nil
}

// violationOf 判断 rel 文件（位于服务的 top 目录下，非平铺层）引用 target 是否违规，合规返回空串。
// target 是被引用包相对 internal/<svc>/ 的路径，modules 是该服务的全部模块。
func violationOf(rel, top, target string, modules []string) string {
	dst, sub, _ := strings.Cut(target, "/")
	switch {
	case !slices.Contains(modules, dst):
		return "" // 引用的不是模块（共享包等），不归这两条规则管
	case !slices.Contains(modules, top):
		return fmt.Sprintf("%s：服务级共享包 %s 引用了模块 %s，共享包不能反过来依赖模块", rel, top, dst)
	case dst != top && sub != "service":
		return fmt.Sprintf("%s：模块 %s 引用了模块 %s 的 %s，跨模块只能经对方的 service 包", rel, top, dst, target)
	}
	return ""
}

// skipDir 报告遍历时是否跳过名为 name 的目录：testdata 与以 . 或 _ 开头的目录，go 工具同样不理会它们。
func skipDir(name string) bool {
	return name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// fixtureModule 是 testdata 夹具的 module 路径：夹具不带 go.mod，其中的 import 都以它为前缀。
const fixtureModule = "example.com/m"

func TestCheck(t *testing.T) {
	tests := []struct {
		name     string   // 夹具目录名，位于 testdata/ 下
		services int      // 期望识别出的服务数
		wants    []string // 期望的违规，各为一条违规的开头部分（路径、类别、关键标识），不要求顺序；nil 表示合规
	}{
		// 平铺服务带 sign、adapter/x 这类额外子包：它们不是模块，不能误报。adapter 下还有个叫 model 的
		// 层名目录，但 adapter 没有 service 子目录，照样不是模块。
		{name: "flat", services: 1},
		// 分模块服务的合法写法：模块引用对方的 service、引用共享包，共享包引用共享包，模块引用自己的其他层。
		// 共享包 common 下有层名目录 model，但 common 没有 service 子目录，仍是共享包，order 引用它不算跨模块。
		{name: "modular", services: 1},
		{
			name:     "mixed",
			services: 1,
			wants:    []string{"internal/shop：平铺层 [service] 与模块 [item] 混用"},
		},
		// 引用 repo、model 越界；_test.go 同样受检；service 的子包不在放行之列。
		{
			name:     "cross",
			services: 1,
			wants: []string{
				"internal/shop/order/handler/handler.go：模块 order 引用了模块 item 的 item/model",
				"internal/shop/order/service/a.go：模块 order 引用了模块 item 的 item/repo",
				"internal/shop/order/service/b_test.go：模块 order 引用了模块 item 的 item/repo",
				"internal/shop/order/service/c.go：模块 order 引用了模块 item 的 item/service/sub",
			},
		},
		{
			name:     "shared",
			services: 1,
			wants:    []string{"internal/shop/auth/auth.go：服务级共享包 auth 引用了模块 item"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, services, err := check(filepath.Join("testdata", tt.name), fixtureModule)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if services != tt.services {
				t.Errorf("服务数 = %d，期望 %d", services, tt.services)
			}
			// 一条期望认领一条违规；认领完还剩下的，就是多报的。
			rest := slices.Clone(got)
			for _, want := range tt.wants {
				i := slices.IndexFunc(rest, func(v string) bool { return strings.HasPrefix(v, want) })
				if i < 0 {
					t.Errorf("没有违规以 %q 开头，实际：%q", want, got)
					continue
				}
				rest = slices.Delete(rest, i, i+1)
			}
			for _, v := range rest {
				t.Errorf("多报的违规：%s", v)
			}
		})
	}
}

func TestCheckWithoutInternal(t *testing.T) {
	violations, services, err := check(t.TempDir(), fixtureModule)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(violations) != 0 || services != 0 {
		t.Errorf("没有 internal/ 时应是 0 条违规、0 个服务，实际 %q、%d", violations, services)
	}
}

// 散落在 internal/ 与服务目录下的文件不是服务也不是子目录；testdata 与 . _ 开头的目录同样不算，
// 其下的内容既不参与目录识别，也不参与引用检查。
func TestCheckIgnoredEntries(t *testing.T) {
	const empty = "package x\n"
	// 没被跳过的话，这份引用会被当成 order 模块越界引用 item/repo。
	crossing := importing("b/item/repo")
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		// 平铺服务 a：testdata 若被识别，其下的 service 会被当成模块，与平铺层混用。
		"internal/a/service/x.go":          empty,
		"internal/a/testdata/service/x.go": empty,
		// 分模块服务 b：order 下被跳过的目录引用 item/repo，不该报。
		"internal/b/doc.go":              empty,
		"internal/b/item/service/x.go":   empty,
		"internal/b/order/service/x.go":  empty,
		"internal/b/order/testdata/x.go": crossing,
		"internal/b/order/_draft/x.go":   crossing,
		"internal/b/order/.cache/x.go":   crossing,
		// internal/ 下的散落文件与被跳过的目录都不是服务，服务数只能是 a 与 b。
		"internal/doc.go":        empty,
		"internal/testdata/x.go": empty,
		"internal/_old/x.go":     empty,
		"internal/.tmp/x.go":     empty,
	})
	violations, services, err := check(root, fixtureModule)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if services != 2 || len(violations) != 0 {
		t.Errorf("应是 2 个服务、0 条违规，实际 %d 个服务、违规 %q", services, violations)
	}
}

// 混用的服务里，平铺层不按共享包查（它引用模块，已经算在混用里），其余违规照常报告；
// 结果按字符串排序，与发现顺序无关：混用那条最先发现，字符串序却排在文件级违规之后。
func TestCheckMixedService(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"internal/shop/service/x.go":       importing("shop/item/service"),
		"internal/shop/item/service/x.go":  "package service\n",
		"internal/shop/item/repo/x.go":     "package repo\n",
		"internal/shop/order/service/x.go": importing("shop/item/repo"),
	})
	got, _, err := check(root, fixtureModule)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	wants := []string{
		"internal/shop/order/service/x.go：模块 order 引用了模块 item 的 item/repo",
		"internal/shop：平铺层 [service] 与模块 [item order] 混用",
	}
	if len(got) != len(wants) {
		t.Fatalf("应有 %d 条违规，实际 %d 条：%q", len(wants), len(got), got)
	}
	for i, want := range wants {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("第 %d 条违规 = %q，期望以 %q 开头", i+1, got[i], want)
		}
	}
}

// 解析失败要作为 error 返回，不能当作「没有违规」。语法错误的文件进不了 testdata
// （gofmt -l 会报），所以现场生成。
func TestCheckParseError(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"internal/shop/item/service/bad.go": "package service\n\nimport (\n",
	})
	_, _, err := check(root, fixtureModule)
	if err == nil || !strings.Contains(err.Error(), "bad.go") {
		t.Fatalf("应返回指明 bad.go 的 error，实际 %v", err)
	}
}

// TestLayout 检查真实仓库。夹具测试证明规则拦得住违规，但干净的仓库本身证明不了什么，
// 所以另设防空跑检查：internal/ 存在却一个服务都没识别出来，多半是根目录或路径算错了。
func TestLayout(t *testing.T) {
	root := filepath.Join("..", "..")
	modulePath, err := readModulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	violations, services, err := check(root, modulePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Error(v)
	}
	if _, err := os.Stat(filepath.Join(root, "internal")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return // 还没有 internal/ 的仓库（如模板主干），无事可查
		}
		t.Fatal(err)
	}
	if services == 0 {
		t.Fatal("internal/ 存在，却没识别出任何服务：检查根目录是否算错")
	}
}

// readModulePath 读 goMod（go.mod 的路径）的 module 行，返回模块路径。
func readModulePath(goMod string) (string, error) {
	data, err := os.ReadFile(goMod)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("%s 里没有 module 行", goMod)
}

// importing 返回只空白导入 pkg（相对 internal/ 的路径）的 Go 源文件内容。
func importing(pkg string) string {
	return "package x\n\nimport _ \"" + fixtureModule + "/internal/" + pkg + "\"\n"
}

// writeTree 按 files（相对 root 的斜杠路径 → 文件内容）在 root 下建出一棵目录树。
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, src := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
