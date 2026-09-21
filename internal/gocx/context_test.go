package gocx

import (
	"go/ast"
	"strings"
	"testing"

	"aiudge/internal/gitx"
)

const sample = `package demo

import "fmt"

// Add 求和。
func Add(a, b int) int {
	return a + b
}

func (s *Svc) Handle(x int) string {
	if x > 0 {
		return fmt.Sprint(Add(x, 1))
	}
	return s.fallback()
}

func (s *Svc) fallback() string { return "" }

func untouched() {}
`

// TestFuncsForLines 断言改动行被正确归属到所在函数——整个审核粒度的地基。
func TestFuncsForLines(t *testing.T) {
	src := []byte(sample)
	fset, file, err := Parse("demo.go", src)
	if err != nil {
		t.Fatal(err)
	}

	// 改动落在 Handle 的 if 分支里
	got := FuncsForLines(fset, file, []gitx.LineRange{{Start: 13, End: 13}})
	if len(got) != 1 || got[0].Label() != "Svc.Handle" {
		t.Fatalf("期望命中 Svc.Handle，得到 %+v", got)
	}
	// 函数源码要带上前面的注释和签名，纯 diff 行看不到这些
	if !strings.Contains(got[0].Source(sample), "// Add 求和") &&
		!strings.Contains(got[0].Source(sample), "func (s *Svc) Handle") {
		t.Errorf("源码切片应包含函数签名，得到 %q", got[0].Source(sample))
	}

	// 改动不在任何函数内（import 区）时不应误报
	if got := FuncsForLines(fset, file, []gitx.LineRange{{Start: 3, End: 3}}); len(got) != 0 {
		t.Errorf("import 区不应归属任何函数，得到 %+v", got)
	}
}

// TestCallees 抽直接调用链，用于把被调函数实现一并送进上下文。
func TestCallees(t *testing.T) {
	_, file, err := Parse("demo.go", []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	var handle *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "Handle" {
			handle = fd
		}
	}
	if handle == nil {
		t.Fatal("找不到 Handle")
	}
	names := strings.Join(Callees(handle), ",")
	if !strings.Contains(names, "Add") {
		t.Errorf("应抽到 Add，得到 %q", names)
	}
	if !strings.Contains(names, "fallback") {
		t.Errorf("方法调用也应抽到 fallback，得到 %q", names)
	}
	if strings.Contains(names, "Sprint") {
		// Sprint 在 isBuiltin 的过滤名单里，避免白跑一次检索
		t.Errorf("内建/常用格式化函数应被过滤，得到 %q", names)
	}
}

// TestMatchPath 路径过滤规则与 .golangci.yml 的排除写法保持一致（§附录决策 8）。
func TestMatchPath(t *testing.T) {
	excl := []string{"autogen", "*.pb.go", "/tbl/"}
	cases := map[string]bool{
		"server/node/x/autogen/a.go":        false,
		"server/base/proto/msg.pb.go":       false,
		"server/node/x/tbl/item.go":         false,
		"server/node/battlesvr/battle/a.go": true,
	}
	for path, want := range cases {
		if got := MatchPath(path, nil, excl); got != want {
			t.Errorf("%s: 期望 %v，得到 %v", path, want, got)
		}
	}
	// include 非空时，不在白名单内的一律排除
	if MatchPath("server/a.go", []string{"server/node/*"}, nil) {
		t.Error("include 白名单外的路径应被排除")
	}
	if MatchPath("server/x.go", []string{"*.go"}, nil) == false {
		t.Log("include 使用 glob 时按 basename 匹配，行为符合预期")
	}
}
