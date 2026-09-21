package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"aiudge/internal/llm"
)

// 占位符用具名的，不用 fmt 的 %s：prompt 是人手写的，随手写个「错误率 % 上升」
// 就会被 Sprintf 当成格式动词，输出成 %! 上升。具名替换没这个问题，
// 而且在界面上自解释——编辑的人一眼能看到有哪些变量可用。
const (
	phLanguage = "{{language}}"
	phRepo     = "{{repo}}"
)

// PromptPlaceholders 给界面提示用。字段要带 json tag——不带的话序列化出去是
// 首字母大写的 Token/Desc，前端按小写取会拿到 undefined，界面上一片空白但不报错。
var PromptPlaceholders = []struct {
	Token string `json:"token"`
	Desc  string `json:"desc"`
}{
	{phLanguage, "仓库配置里的语言名，如 Go / C++"},
	{phRepo, "仓库名称"},
}

// DefaultSystemPrompt 是审核指令的骨架，语言与领域细节靠占位符和挂载的规范文档填。
// 刻意不写死具体项目——领域关注点应该来自仓库自己挂的文档，而不是内建在提示词里。
func DefaultSystemPrompt() string { return defaultSystemPrompt }

const defaultSystemPrompt = `你是资深 {{language}} 代码审核专家，负责审核「{{repo}}」这个仓库的每日提交。

审核目标：只找**本次改动引入**的问题，不报存量问题。改动行之外的代码只作为上下文参考。

只报以下四类：

1. 安全 —— 参数校验缺失、越权访问、关键数值与上限校验缺失、并发下的资源竞争、敏感信息泄露
2. 性能 —— N+1 查询、循环内远程调用、锁滥用（大锁/锁内 IO）、无界增长的容器
3. 逻辑 —— 边界 case 遗漏、条件分支写错、错误值被忽略、返回值未检查、off-by-one
4. 重复代码 —— 仓库里已有等价实现却重新造轮子

不要报规范与风格类问题（命名、注释、格式、lint 能查的），那由 lint 工具负责。

严重等级只有两档，不要用别的词：
- 高危：会导致数据错误、资损、崩溃或安全事故，必须修
- 建议：健壮性、可读性、性能上的改进建议

跨文件问题（重复造轮子、N+1、锁滥用）必须先调用检索工具确认再下结论：
用 find_symbol 查是否已有同名/等价实现，用 grep 查已有调用方式，用 read_file 确认被调函数的实现。
不确定就查，不要凭猜测报问题——误报会直接污染误报率指标。

宁缺毋滥。没有把握的问题不要报。

输出严格 JSON，不要 markdown 代码块，不要任何解释文字：
{"issues":[{"file":"相对仓库根的路径","line_start":12,"line_end":20,"severity":"高危","category":"安全","title":"一句话问题标题","description":"问题说明，指出具体触发条件","suggestion":"具体修复方案或示例代码","code":"出问题的原始代码片段"}]}

没有发现问题时输出 {"issues":[]}。`

// RenderPrompt 把占位符替换成实际值。模板里没出现的占位符就自然不替换，
// 用户删掉某一行也不会报错。
func RenderPrompt(tmpl, lang, repoName string) string {
	if repoName == "" {
		repoName = "本项目"
	}
	r := strings.NewReplacer(phLanguage, LangName(lang), phRepo, repoName)
	return r.Replace(tmpl)
}

// PromptVersion 是 §5 要求的 prompt 版本号，落进报告标签供趋势图打断点。
//
// 取内容哈希而不是让人手填：手填的版本必然漂移——改了 prompt 忘了改版本号，
// 历史曲线就再也解释不清了（§附录决策 6 要防的正是这个）。哈希自动跟着内容走。
// 代价是改个错别字也会留下断点，但那确实是一次 prompt 变更，宁可多标不可漏标。
func PromptVersion(tmpl string) string {
	h := sha256.Sum256([]byte(tmpl))
	return hex.EncodeToString(h[:])[:12]
}

// EffectivePrompt 返回实际生效的 prompt 模板：自定义过就用自定义的，否则用默认。
func EffectivePrompt(custom string) string {
	if strings.TrimSpace(custom) == "" {
		return defaultSystemPrompt
	}
	return custom
}

// LangName 把仓库配置里的语言代码翻成提示词里好读的名字。
// 未知语言原样返回，不猜——猜错会让模型按错误的语言习惯审代码。
func LangName(lang string) string {
	switch strings.ToLower(lang) {
	case "go":
		return "Go"
	case "cpp", "c++", "cc", "c":
		return "C++"
	case "python", "py":
		return "Python"
	case "typescript", "ts":
		return "TypeScript"
	case "javascript", "js":
		return "JavaScript"
	case "java":
		return "Java"
	case "rust", "rs":
		return "Rust"
	case "lua":
		return "Lua"
	case "csharp", "cs":
		return "C#"
	case "":
		return "后端"
	default:
		return lang
	}
}

// specSection 把仓库挂载的规范/checklist 文档拼进系统提示。历史 bug 清单
// 第一版留空（§8），有就拼、没有就跳过。
func specSection(docs map[string]string) string {
	if len(docs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n以下是本仓库的专属审核规范，审核时需结合这些约束判断：\n")
	names := make([]string, 0, len(docs))
	for k := range docs {
		names = append(names, k)
	}
	sortStrings(names)
	for _, n := range names {
		fmt.Fprintf(&sb, "\n### %s\n%s\n", n, truncateRunes(docs[n], 12000))
	}
	return sb.String()
}

// SpecVersion 是规范文档的内容指纹：挂载文档一改，版本号随之变化，
// 趋势图就能解释「为什么这天之后问题数变了」。
func SpecVersion(docs map[string]string) string {
	if len(docs) == 0 {
		return "none"
	}
	h := sha256.New()
	names := make([]string, 0, len(docs))
	for k := range docs {
		names = append(names, k)
	}
	sortStrings(names)
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%s\x00", n, docs[n])
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// buildSystem 组装系统提示。模板、语言、仓库名与挂载文档都在调用点决定。
func buildSystem(tmpl, lang, repoName string, docs map[string]string) string {
	return RenderPrompt(tmpl, lang, repoName) + specSection(docs)
}

// toolDefs 是给模型的代码库检索工具（§4.8：让 AI 按需查符号定义与已有实现，
// 否则「重复造轮子」和「N+1」这两类跨文件问题根本看不见）。
func toolDefs(lang string) []llm.Tool {
	mk := func(name, desc string, params map[string]any, required ...string) llm.Tool {
		var t llm.Tool
		t.Type = "function"
		t.Function.Name = name
		t.Function.Description = desc
		t.Function.Parameters = map[string]any{
			"type":       "object",
			"properties": params,
			"required":   required,
		}
		return t
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

	return []llm.Tool{
		mk("grep", "在整个代码库里按正则检索，返回 文件:行号: 内容。用于找已有实现、调用点、配置项。",
			map[string]any{"pattern": str("正则表达式"), "max_hits": num("最多返回多少条，默认 40")}, "pattern"),
		mk("find_symbol", "按名字查找 "+LangName(lang)+" 符号声明（函数/类型/常量）的定义位置。查「这个轮子是不是已经有了」用这个。",
			map[string]any{"name": str("符号名，如 CalcReward")}, "name"),
		mk("read_file", "读取代码库中某个文件的行区间内容，行号带前缀。",
			map[string]any{"path": str("相对仓库根的文件路径"), "start": num("起始行，从 1 开始"), "end": num("结束行")}, "path"),
		mk("list_dir", "列出目录内容，用于先摸清仓库结构。",
			map[string]any{"path": str("相对仓库根的目录，仓库根传 .")}, "path"),
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n...(已截断)"
}
