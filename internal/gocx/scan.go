package gocx

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DirCandidate 是扫描仓库目录结构得到的子系统规则候选。
type DirCandidate struct {
	Pattern string `json:"pattern"` // 路径子串，直接可用作规则
	Files   int    `json:"files"`   // 该目录下的源文件数，用来排序与判断重要性
	Sample  string `json:"sample"`  // 一个示例路径，帮人回忆这是干什么的
}

// ScanDirs 遍历仓库工作副本，找出「装了源文件的目录」作为归类规则的候选。
//
// 这是在替人省掉「照着目录树手打一遍」的活，但目录名离「战斗系统」这种业务名词
// 还差一层人工判断，所以只给候选不给名称。按文件数从多到少排，先把大块头挑出来。
func ScanDirs(root string, exts []string, exclude []string) ([]DirCandidate, error) {
	extSet := map[string]bool{}
	for _, e := range exts {
		extSet[e] = true
	}
	counts := map[string]int{}
	sample := map[string]string{}

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(d.Name())
		if !extSet[ext] {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !MatchPath(rel, nil, exclude) {
			return nil
		}
		// 逐层累计：文件同时计入它的每一级父目录，
		// 这样人既能看到粗粒度的大模块，也能下钻到具体子目录
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}
		for dir != "" {
			counts[dir]++
			if sample[dir] == "" {
				sample[dir] = rel
			}
			parent := filepath.ToSlash(filepath.Dir(dir))
			if parent == "." || parent == dir {
				break
			}
			dir = parent
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]DirCandidate, 0, len(counts))
	for dir, n := range counts {
		// 只有一两个文件的目录不值得单列一档，多半是工具脚本或配置
		if n < 3 {
			continue
		}
		out = append(out, DirCandidate{Pattern: dir, Files: n, Sample: sample[dir]})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Files != out[b].Files {
			return out[a].Files > out[b].Files
		}
		return out[a].Pattern < out[b].Pattern
	})
	const maxCandidates = 60
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out, nil
}

// DocExts 返回可以挂作审核规范文档的文本文件扩展名。
func DocExts() []string {
	return []string{".md", ".markdown", ".rst", ".txt", ".adoc"}
}

// ScanDocs 列出仓库工作副本里可挂作规范文档的文件，返回相对工作副本根的路径。
//
// 只列文档类文本文件——挂的是「给人看的规范」，不是源码。目录用 skipDir 过滤，
// 不套仓库的 include/exclude glob：管线读规范文档时也不套，列出来的路径都能真的挂上。
func ScanDocs(root string) ([]string, error) {
	exts := map[string]bool{}
	for _, e := range DocExts() {
		exts[e] = true
	}
	out := []string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// 只挡 skipDir 里那几个（.git/vendor/...）。别的点目录不挡：
			// 规范文档常常正好放在 .claude/ 这类目录下
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 浅的排前面：要挂的规范文档基本都在根目录或 docs/ 下，深层的（.claude/、
	// 各种 wiki 树）排在后面。这里不截断——截断会让过滤框搜不到被砍掉的那些文件
	sort.Slice(out, func(a, b int) bool {
		da, db := strings.Count(out[a], "/"), strings.Count(out[b], "/")
		if da != db {
			return da < db
		}
		return out[a] < out[b]
	})
	return out, nil
}

// LangExts 返回某语言的源文件扩展名。语言只影响「扫哪些文件」与上下文提取的精度，
// 不影响管线本身——克隆、diff、指纹、报表都是语言无关的。
func LangExts(lang string) []string {
	switch strings.ToLower(lang) {
	case "go":
		return []string{".go"}
	case "cpp", "c++", "cc", "c":
		return []string{".cpp", ".cc", ".cxx", ".c", ".h", ".hpp", ".hxx", ".inl"}
	case "python", "py":
		return []string{".py"}
	case "typescript", "ts":
		return []string{".ts", ".tsx"}
	case "javascript", "js":
		return []string{".js", ".jsx", ".mjs", ".cjs"}
	case "java":
		return []string{".java"}
	case "rust", "rs":
		return []string{".rs"}
	case "lua":
		return []string{".lua"}
	case "csharp", "cs":
		return []string{".cs"}
	default:
		if lang == "" {
			return []string{".go"}
		}
		return nil
	}
}

// SourceExts 返回全部已知语言的扩展名，用于语言未知时的宽松扫描。
func SourceExts() []string {
	var all []string
	seen := map[string]bool{}
	for _, l := range []string{"go", "cpp", "python", "typescript", "javascript", "java", "rust", "lua", "csharp"} {
		for _, e := range LangExts(l) {
			if !seen[e] {
				seen[e] = true
				all = append(all, e)
			}
		}
	}
	return all
}
