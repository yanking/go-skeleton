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
// config 在两种布局里都留在服务级、且不分模块，所以不在其列，按共享包处理。
var layerNames = []string{"handler", "service", "repo", "model", "job"}

// check 检查 root 下 internal/ 的目录布局与模块边界，modulePath 是 root 所在 Go module 的路径
// （go.mod 的 module 行）。internal/ 下每个一级目录是一个服务，services 是识别出的服务数；
// violations 是按字符串排序的违规描述，其中的文件与目录路径都相对 root、以斜杠分隔。
// internal/ 不存在时返回 nil、0、nil；读目录或解析 Go 文件失败作为 err 返回，绝不当作「无违规」。
//
// 布局是**层优先**的：internal/<svc>/<层>/ 下直接放 Go 文件即平铺，放子目录即分模块
// （internal/<svc>/<层>/<模块>/）。**模块集合由 service 层的子目录定义**——service 是模块
// 唯一的对外入口，拿它当权威来源，其余层只能与它对齐。服务一级子目录里名字是层名的是层目录，
// config 与其余名字是服务级共享包。
//
// 查五类违规：
//   - service 层既直接放 Go 文件又有子目录（平铺与分模块混用）；
//   - 服务已按模块分目录，某个层却直接放 Go 文件；
//   - 层目录下出现不是模块的子目录（层优先布局里层内不另建子包——它与模块同形，分不开）；
//   - 模块 A 的文件引用了模块 B 的包，而引用的不是 B 的 service 包本身；
//   - 服务级共享包引用了任何模块。
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

// checkService 检查 internal/<svc>/ 这一个服务：先判布局，再逐文件查模块边界。
func checkService(root, modulePath, svc string) ([]string, error) {
	svcDir := filepath.Join(root, "internal", svc)
	lay, violations, err := classify(svc, svcDir)
	if err != nil {
		return nil, err
	}
	if len(lay.modules) == 0 {
		return violations, nil // 引用规则只对分模块的服务生效
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
		top, mod, nested := lay.locate(strings.TrimPrefix(rel, svcPrefix))
		if !nested {
			return nil // 服务根目录下的散落文件不受检
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
			if v := lay.violationOf(rel, top, mod, target); v != "" {
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

// layout 是一个服务的目录布局：各层目录的内容形态，以及由 service 层定义的模块集合。
type layout struct {
	// layers 是存在的层目录名 → 其内容形态。
	layers map[string]layerContent
	// modules 是模块名，按名字排序；空即平铺服务（或还没有 service 层的骨架）。
	modules []string
}

// layerContent 一个层目录的内容形态：直接放 Go 文件即平铺，只放子目录即分模块。
type layerContent struct {
	hasGo   bool
	subdirs []string
}

// classify 判定服务布局并报告布局本身的违规。
func classify(svc, svcDir string) (layout, []string, error) {
	entries, err := os.ReadDir(svcDir)
	if err != nil {
		return layout{}, nil, err
	}
	lay := layout{layers: map[string]layerContent{}}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || skipDir(name) || !slices.Contains(layerNames, name) {
			continue
		}
		c, err := readLayer(filepath.Join(svcDir, name))
		if err != nil {
			return layout{}, nil, err
		}
		lay.layers[name] = c
	}

	// 模块集合只认 service 层：它是模块唯一的对外入口，别的层可以缺某个模块的目录，
	// 拿它们当权威来源就会把「某模块还没有 repo」误判成布局问题。
	svcLayer, ok := lay.layers["service"]
	switch {
	case !ok:
		return lay, nil, nil // 还没有 service 层（none 变体骨架），无模块可言
	case svcLayer.hasGo && len(svcLayer.subdirs) > 0:
		return lay, []string{fmt.Sprintf(
			"internal/%s/service：既直接放 Go 文件又有子目录 %v，平铺与分模块不能混用（见 AGENTS.md「服务分层」）",
			svc, svcLayer.subdirs)}, nil
	case svcLayer.hasGo || len(svcLayer.subdirs) == 0:
		return lay, nil, nil // 平铺服务
	}
	lay.modules = svcLayer.subdirs

	var violations []string
	for _, name := range layerNames {
		c, ok := lay.layers[name]
		if !ok || name == "service" {
			continue
		}
		if c.hasGo {
			violations = append(violations, fmt.Sprintf(
				"internal/%s/%s：服务已按模块分目录 %v，本层却直接放 Go 文件，同一服务不能混用两种布局",
				svc, name, lay.modules))
		}
		for _, sub := range c.subdirs {
			if !slices.Contains(lay.modules, sub) {
				violations = append(violations, fmt.Sprintf(
					"internal/%s/%s/%s：不是模块（模块由 service 层的子目录定义：%v）——层优先布局里层内不另建子包，它与模块同形",
					svc, name, sub, lay.modules))
			}
		}
	}
	return lay, violations, nil
}

// readLayer 读层目录的内容形态；子目录按名字排序（os.ReadDir 已按文件名返回，排序只为不依赖该保证）。
func readLayer(dir string) (layerContent, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return layerContent{}, err
	}
	var c layerContent
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir():
			if !skipDir(name) {
				c.subdirs = append(c.subdirs, name)
			}
		case filepath.Ext(name) == ".go":
			c.hasGo = true
		}
	}
	slices.Sort(c.subdirs)
	return c, nil
}

// locate 定位 rel（相对 internal/<svc>/ 的斜杠路径）：top 是它所在的服务一级子目录，
// mod 是它所属的模块（不在分模块的层目录下时为空串），nested 为 false 表示它是服务
// 根目录下的散落文件。
func (l layout) locate(rel string) (top, mod string, nested bool) {
	top, rest, nested := strings.Cut(rel, "/")
	if !nested {
		return top, "", false
	}
	mod, _ = l.moduleAt(top, rest)
	return top, mod, true
}

// moduleAt 判断「层目录 top 下的 rest」属于哪个模块：mod 为空即不属于任何模块
// （top 不是层目录、该层平铺、或首段不是模块名）。sub 是模块内的剩余路径，
// 空串表示引用的就是该模块在该层的包本身。
func (l layout) moduleAt(top, rest string) (mod, sub string) {
	c, isLayer := l.layers[top]
	if !isLayer || c.hasGo {
		return "", ""
	}
	mod, sub, _ = strings.Cut(rest, "/")
	if !slices.Contains(l.modules, mod) {
		return "", ""
	}
	return mod, sub
}

// violationOf 判断 rel 文件（位于服务的 top 目录下、属于模块 mod）引用 target 是否违规，
// 合规返回空串。target 是被引用包相对 internal/<svc>/ 的路径。
func (l layout) violationOf(rel, top, mod, target string) string {
	layer, rest, nested := strings.Cut(target, "/")
	if !nested {
		return ""
	}
	dst, sub := l.moduleAt(layer, rest)
	switch {
	case dst == "":
		return "" // 引用的不是模块的包（共享包、config 等），不归这两条规则管
	case mod == "" && !slices.Contains(layerNames, top):
		return fmt.Sprintf("%s：服务级共享包 %s 引用了模块 %s，共享包不能反过来依赖模块", rel, top, dst)
	case mod == "":
		return "" // 层目录却定不出模块：布局本身已报违规，不再叠加文件级违规
	case dst != mod && !(layer == "service" && sub == ""):
		return fmt.Sprintf("%s：模块 %s 引用了模块 %s 的 %s，跨模块只能经对方的 service 包", rel, mod, dst, target)
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
		// 平铺服务带 sign、adapter/x 这类额外共享包：它们不是层目录，层内子包的禁令不及于它们；
		// adapter 下还有个叫 model 的目录，但 adapter 本身不是层名，照样不受影响。
		{name: "flat", services: 1},
		// 分模块服务的合法写法：模块引用对方 service 层的包、引用自己的其他层、引用共享包，共享包引用共享包。
		// 共享包 common 下有层名目录 model，但 common 不在服务一级的层目录之列，不参与模块判定。
		// handler/model/repo 各缺一个模块的目录（不是每个模块都有每层），不该因此报违规。
		{name: "modular", services: 1},
		// service 层分了模块，repo 层却直接放 Go 文件。
		{
			name:     "mixed",
			services: 1,
			wants:    []string{"internal/shop/repo：服务已按模块分目录 [item]，本层却直接放 Go 文件"},
		},
		// 层内出现不是模块的子目录。
		{
			name:     "layersub",
			services: 1,
			wants:    []string{"internal/shop/repo/shared：不是模块"},
		},
		// 引用 repo、model 越界；_test.go 同样受检；service 的子包不在放行之列。
		{
			name:     "cross",
			services: 1,
			wants: []string{
				"internal/shop/handler/order/handler.go：模块 order 引用了模块 item 的 model/item",
				"internal/shop/service/order/a.go：模块 order 引用了模块 item 的 repo/item",
				"internal/shop/service/order/b_test.go：模块 order 引用了模块 item 的 repo/item",
				"internal/shop/service/order/c.go：模块 order 引用了模块 item 的 service/item/sub",
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
	// 没被跳过的话，这份引用会被当成 order 模块越界引用 item 的 repo。
	crossing := importing("b/repo/item")
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		// 平铺服务 a：testdata 若参与层目录识别，service 层就会被判成既有 Go 文件又有子目录。
		"internal/a/service/x.go":               empty,
		"internal/a/service/testdata/item/x.go": empty,
		// 分模块服务 b：order 下被跳过的目录引用 item 的 repo，不该报。
		"internal/b/doc.go":                      empty,
		"internal/b/service/item/x.go":           empty,
		"internal/b/repo/item/x.go":              empty,
		"internal/b/service/order/x.go":          empty,
		"internal/b/service/order/testdata/x.go": crossing,
		"internal/b/service/order/_draft/x.go":   crossing,
		"internal/b/service/order/.cache/x.go":   crossing,
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

// 布局违规与文件级违规同时存在时都要报；结果按字符串排序，与发现顺序无关。
func TestCheckMixedService(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"internal/shop/service/item/x.go":  "package service\n",
		"internal/shop/service/order/x.go": importing("shop/repo/item"),
		"internal/shop/repo/item/x.go":     "package repo\n",
		"internal/shop/handler/x.go":       "package handler\n",
	})
	got, _, err := check(root, fixtureModule)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	wants := []string{
		"internal/shop/handler：服务已按模块分目录 [item order]，本层却直接放 Go 文件",
		"internal/shop/service/order/x.go：模块 order 引用了模块 item 的 repo/item",
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

// service 层既直接放 Go 文件又有子目录：布局无从判定，报混用并停在这里（不再查引用）。
func TestCheckServiceLayerAmbiguous(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"internal/shop/service/x.go":      "package service\n",
		"internal/shop/service/item/x.go": "package item\n",
	})
	got, _, err := check(root, fixtureModule)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	want := "internal/shop/service：既直接放 Go 文件又有子目录 [item]"
	if len(got) != 1 || !strings.HasPrefix(got[0], want) {
		t.Fatalf("应只有一条以 %q 开头的违规，实际 %q", want, got)
	}
}

// 解析失败要作为 error 返回，不能当作「没有违规」。语法错误的文件进不了 testdata
// （gofmt -l 会报），所以现场生成。
func TestCheckParseError(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"internal/shop/service/item/bad.go": "package service\n\nimport (\n",
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
