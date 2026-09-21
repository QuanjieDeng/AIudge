// Package gocx 从改动行反推审核上下文：改动行所在的函数体，以及它的直接调用链。
// 纯 diff 行看不到函数签名与上下文，整个文件又太贵（§附录决策 16），函数是最小可审单元。
package gocx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"aiudge/internal/gitx"
)

type Func struct {
	Name      string
	Recv      string
	StartLine int
	EndLine   int
	Doc       string
}

func (f Func) Label() string {
	if f.Recv != "" {
		return f.Recv + "." + f.Name
	}
	return f.Name
}

// Source 从整份文件文本里切出函数源码。用行号切比 ast 打印更保真，
// 注释、格式、空行都原样保留。
func (f Func) Source(fileSrc string) string {
	lines := strings.Split(fileSrc, "\n")
	start := max(0, f.StartLine-1)
	end := min(len(lines), f.EndLine)
	return strings.Join(lines[start:end], "\n")
}

// FuncsForLines 返回与改动行相交的函数，按出现顺序去重。
// 改动落在函数外（包级变量、import 等）时不返回任何结果，由调用方回退到整个改动块。
func FuncsForLines(fset *token.FileSet, file *ast.File, ranges []gitx.LineRange) []Func {
	var out []Func
	seen := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			return true
		}
		start := fset.Position(fd.Pos()).Line
		end := fset.Position(fd.End()).Line
		if !overlaps(start, end, ranges) {
			return true
		}
		f := Func{Name: fd.Name.Name, StartLine: start, EndLine: end}
		if fd.Doc != nil {
			f.Doc = strings.TrimSpace(fd.Doc.Text())
			f.StartLine = fset.Position(fd.Doc.Pos()).Line
		}
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			f.Recv = receiverName(fd.Recv.List[0].Type)
		}
		key := f.Label() + ":" + itoa(start)
		if !seen[key] {
			seen[key] = true
			out = append(out, f)
		}
		return false // 函数体内部不再下探，避免嵌套字面量里的重复命中
	})
	return out
}

func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexExpr: // 泛型接收者
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.IndexListExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// Callees 抽出函数体内直接调用的名字（含方法调用），用于拼「直接调用链」。
// 只取一层：再深的调用链会让上下文膨胀到不可控，而 N+1/锁滥用这类问题看一层足够。
func Callees(fd *ast.FuncDecl) []string {
	if fd.Body == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if name == "" || seen[name] || isBuiltin(name) {
			return true
		}
		seen[name] = true
		out = append(out, name)
		return true
	})
	return out
}

// isBuiltin 过滤掉类型转换与内建函数，它们在代码库里没有定义，检索只会白跑。
func isBuiltin(name string) bool {
	switch name {
	case "len", "cap", "make", "new", "append", "copy", "delete", "panic", "recover",
		"print", "println", "close", "complex", "real", "imag", "min", "max", "clear",
		"int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "string", "bool", "byte", "rune", "error", "any",
		"Errorf", "Sprintf", "Sprint", "Sprintln", "Error", "Info", "Debug", "Warn", "Fatal":
		return true
	}
	return false
}

// Parse 解析单个 Go 文件。解析失败（语法不全、构建标签外的文件）返回 error，
// 调用方降级为「整个改动块」上下文即可，不因此让审核失败。
func Parse(path string, src []byte) (*token.FileSet, *ast.File, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	return fset, f, err
}

func overlaps(start, end int, ranges []gitx.LineRange) bool {
	for _, r := range ranges {
		if r.Start <= end && r.End >= start {
			return true
		}
	}
	return false
}

// SliceLines 按行号区间切文本，供「改动块」回退路径使用。
func SliceLines(src string, ranges []gitx.LineRange, pad int) string {
	lines := strings.Split(src, "\n")
	keep := make([]bool, len(lines))
	for _, r := range ranges {
		for i := r.Start - pad; i <= r.End+pad; i++ {
			if i >= 1 && i <= len(lines) {
				keep[i-1] = true
			}
		}
	}
	var sb strings.Builder
	prev := false
	for i, l := range lines {
		if keep[i] {
			if !prev && sb.Len() > 0 {
				sb.WriteString("\t// ...\n")
			}
			sb.WriteString(l)
			sb.WriteString("\n")
			prev = true
		} else {
			prev = false
		}
	}
	return sb.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
