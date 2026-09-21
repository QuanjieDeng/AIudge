package gocx

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Hit 是一次检索命中，Path 相对仓库根，便于直接喂给模型定位代码。
type Hit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func (h Hit) String() string {
	return fmt.Sprintf("%s:%d: %s", h.Path, h.Line, strings.TrimSpace(h.Text))
}

// Toolbox 是给 AI 的代码库检索工具（§4.8：按需检索，不预建全量索引）。
// 所有路径都限制在 Root 内，模型给出的相对路径一律重新解析一遍。
type Toolbox struct {
	Root     string
	Language string
	Include  []string
	Exclude  []string

	symbolIdx map[string][]Hit // 首次 FindSymbol 时构建，一个审核任务内复用
}

func NewToolbox(root, language string, include, exclude []string) *Toolbox {
	return &Toolbox{Root: root, Language: language, Include: include, Exclude: exclude}
}

// symbolRe 返回识别「声明行」的正则。各语言声明形态差别很大，猜不得：
// 匹配宽了会把调用点也算成定义，匹配窄了等于查不到。
//
// 已知短板：C++ 的宏定义（#define）不在编译器 AST 里，正则也覆盖不了，
// 而「宏里已经有等价实现」恰恰是重复造轮子的常见形态。
var symbolRe = map[string]*regexp.Regexp{
	"go": regexp.MustCompile(`^\s*(?:func\s+(?:\([^)]*\)\s*)?|type\s+|var\s+|const\s+)([A-Za-z_]\w*)`),
	"cpp": regexp.MustCompile(
		`^\s*(?:template\s*<[^>]*>\s*)?` + // 模板前缀
			`(?:[\w:]+[\s*&]+)+` + // 返回类型
			`([A-Za-z_]\w*)\s*\(`), // 函数名
	"python":     regexp.MustCompile(`^\s*(?:async\s+)?(?:def|class)\s+([A-Za-z_]\w*)`),
	"typescript": regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?(?:function|class|interface|type|enum|const|let|var)\s+([A-Za-z_$][\w$]*)`),
	"java":       regexp.MustCompile(`^\s*(?:public|private|protected|static|final|abstract|synchronized|\s)*[\w<>\[\],.?\s]+\s+([A-Za-z_]\w*)\s*\(`),
	"rust":       regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:fn|struct|enum|trait|impl|const|static|type)\s+([A-Za-z_]\w*)`),
	"lua":        regexp.MustCompile(`^\s*(?:local\s+)?function\s+([A-Za-z_]\w*[.:]?[A-Za-z_]*)`),
}

// langKey 把仓库配置里的语言代码归一到 symbolRe 的键。
func langKey(lang string) string {
	switch strings.ToLower(lang) {
	case "go", "":
		return "go"
	case "cpp", "c++", "cc", "c", "cxx", "h", "hpp":
		return "cpp"
	case "python", "py":
		return "python"
	case "typescript", "ts", "tsx", "javascript", "js", "jsx":
		return "typescript"
	case "java":
		return "java"
	case "rust", "rs":
		return "rust"
	case "lua":
		return "lua"
	}
	return ""
}

// SymbolHint 给工具描述用，说明这个语言的符号声明长什么样。
func SymbolHint(lang string) string {
	if k := langKey(lang); k != "" {
		return k
	}
	return "未知语言"
}

func (t *Toolbox) resolve(rel string) (string, error) {
	p := filepath.Clean(filepath.Join(t.Root, rel))
	if p != t.Root && !strings.HasPrefix(p, t.Root+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界: %s", rel)
	}
	return p, nil
}

// Grep 在仓库里做正则检索，等价于给模型的「grep -rn」。
func (t *Toolbox) Grep(pattern string, maxHits int) ([]Hit, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("正则非法: %w", err)
	}
	return t.scan(func(path string, lineNo int, line string) bool { return re.MatchString(line) }, maxHits)
}

// FindSymbol 按语言找符号声明，用于「这个轮子是不是已经有了」。
func (t *Toolbox) FindSymbol(name string, maxHits int) ([]Hit, error) {
	if t.symbolIdx == nil {
		if err := t.buildSymbolIndex(); err != nil {
			return nil, err
		}
	}
	return t.symbolIdx[name], nil
}

func (t *Toolbox) buildSymbolIndex() error {
	t.symbolIdx = map[string][]Hit{}
	declRe := symbolRe[langKey(t.Language)]
	if declRe == nil {
		// 未知语言：索引留空，find_symbol 一律返回无匹配。
		// 返回空比返回一堆误匹配好——模型会把误匹配当成「已有实现」而漏报重复代码。
		return nil
	}
	_, err := t.scan(func(path string, lineNo int, line string) bool {
		m := declRe.FindStringSubmatch(line)
		if m == nil {
			return false
		}
		t.symbolIdx[m[1]] = append(t.symbolIdx[m[1]], Hit{Path: path, Line: lineNo, Text: line})
		return false
	}, 0)
	return err
}

// ReadFile 读指定行区间，行号超界自动夹到文件范围内。
func (t *Toolbox) ReadFile(rel string, start, end int) (string, error) {
	p, err := t.resolve(rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	if start < 1 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return "", fmt.Errorf("起始行 %d 超出文件长度 %d", start, len(lines))
	}
	var sb strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "%d\t%s\n", i, lines[i-1])
	}
	return sb.String(), nil
}

// ListDir 列目录，模型用它先摸清仓库结构再决定检索哪里。
func (t *Toolbox) ListDir(rel string) ([]string, error) {
	p, err := t.resolve(rel)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (t *Toolbox) scan(match func(path string, lineNo int, line string) bool, maxHits int) ([]Hit, error) {
	var hits []Hit
	err := filepath.WalkDir(t.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个文件读不了不该中断整次检索
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !t.isSourceFile(p) {
			return nil
		}
		rel, _ := filepath.Rel(t.Root, p)
		rel = filepath.ToSlash(rel)
		if !MatchPath(rel, t.Include, t.Exclude) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(b), "\n") {
			if match(rel, i+1, line) {
				hits = append(hits, Hit{Path: rel, Line: i + 1, Text: line})
				if maxHits > 0 && len(hits) >= maxHits {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	return hits, err
}

// isSourceFile 只扫当前语言的文件。跨语言的检索结果对模型是纯噪音。
func (t *Toolbox) isSourceFile(p string) bool {
	exts := LangExts(t.Language)
	if len(exts) == 0 {
		exts = SourceExts() // 语言没配或没识别出来，宽松扫
	}
	ext := strings.ToLower(filepath.Ext(p))
	for _, e := range exts {
		if ext == e {
			return true
		}
	}
	return false
}

func skipDir(name string) bool {
	switch name {
	case ".git", "vendor", "node_modules", "testdata", "autogen":
		return true
	}
	return false
}

// MatchPath 判定相对路径是否落在包含/排除规则内。规则写法与 .golangci.yml 的
// path 排除保持一致：含通配符按 glob 匹配，否则按子串包含（autogen、/tbl/ 都是后者）。
func MatchPath(rel string, include, exclude []string) bool {
	rel = filepath.ToSlash(rel)
	if len(include) > 0 && !anyMatch(rel, include) {
		return false
	}
	return !anyMatch(rel, exclude)
}

func anyMatch(rel string, patterns []string) bool {
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if strings.ContainsAny(pat, "*?[") {
			if ok, _ := filepath.Match(pat, rel); ok {
				return true
			}
			if ok, _ := filepath.Match(pat, filepath.Base(rel)); ok {
				return true
			}
			continue
		}
		if strings.Contains(rel, pat) {
			return true
		}
	}
	return false
}
