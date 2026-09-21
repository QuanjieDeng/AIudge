package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Fingerprint 是问题的稳定身份（§4.5 / §附录决策 1）：
// 仓库 + 文件路径 + 问题分类 + 归一化代码片段。
//
// 行号刻意不参与计算——插入或删除代码会让行号漂移，拿行号当身份会让趋势统计
// 退化成「文件被改频率图」。回归检测（已修复的代码改回来）也完全依赖这一点。
//
// 比 §4.5 的公式多带了 linter：lint 的多个 linter 会在同一行给出不同问题，
// 不带 linter 名会被折叠成一条，白白丢掉信息。
func Fingerprint(repoID int64, filePath, category, snippet, linter string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%s", repoID, filePath, category, Normalize(snippet), linter)
	return hex.EncodeToString(h.Sum(nil))
}

// Normalize 归一化代码片段：去掉缩进差异与纯注释行，让「同一段代码被重新格式化」
// 或「只改了注释」不被误判成新问题。
//
// ponytail: 只做行级处理，不解析 AST。行内 `//` 不剥离——字符串字面量里的 `//`
// 会被误伤，而整行剥离已经覆盖了缩进/空行/注释三种主要漂移。
func Normalize(snippet string) string {
	var out []string
	for _, line := range strings.Split(snippet, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		// 片段全空（比如 lint 没给 SourceLines），退化成空串——
		// 此时同文件同分类的问题会折叠成一条，属于已知取舍
		return ""
	}
	return strings.Join(out, "\n")
}
