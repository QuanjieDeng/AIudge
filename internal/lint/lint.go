// Package lint 跑 golangci-lint 并把 JSON 输出转成统一的问题项。
// 规范/风格类问题全部交给 lint，AI 不参与（§4.8：lint 是确定性判定且零成本，
// 让 LLM 重复发现会虚高误报率）。
package lint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Issue struct {
	FromLinter  string   `json:"FromLinter"`
	Text        string   `json:"Text"`
	Severity    string   `json:"Severity"`
	SourceLines []string `json:"SourceLines"`
	Pos         struct {
		Filename string `json:"Filename"`
		Line     int    `json:"Line"`
		Column   int    `json:"Column"`
	} `json:"Pos"`
}

type output struct {
	Issues []Issue `json:"Issues"`
}

// Runner 绑定二进制路径与超时，避免每次调用都读全局配置。
type Runner struct {
	Bin     string
	Timeout time.Duration
}

// Run 只对改动涉及的包目录执行，不在整个仓库上跑：全量 lint 单次就要几分钟，
// 会直接吃掉 §5 的 10 分钟软超时预算。
func (r Runner) Run(repoDir string, pkgDirs []string) ([]Issue, error) {
	if len(pkgDirs) == 0 {
		return nil, nil
	}
	args := append([]string{
		"run", "--out-format", "json",
		"--timeout", r.Timeout.String(),
		"--allow-parallel-runners", // 同机可能并发跑多个任务，不加会互相抢锁
	}, pkgDirs...)

	cmd := exec.Command(r.Bin, args...)
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(r.Timeout + 30*time.Second):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("golangci-lint 超时")
	}

	var out output
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		// 没吐 JSON 说明是配置/编译错误，报错让任务失败并暴露原因，不静默吞掉
		return nil, fmt.Errorf("解析 golangci-lint 输出失败: %v; stderr: %s", err, truncate(stderr.String(), 500))
	}
	return out.Issues, nil
}

// PkgDirs 把改动文件收成「包目录」列表，并补上 ./... 后缀供 golangci-lint 使用。
// lang 非 Go 时返回空——调用方据此跳过，不为其他语言硬跑 golangci-lint。
func PkgDirs(files []string, lang string) []string {
	if l := strings.ToLower(lang); l != "" && l != "go" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f))
		p := "./" + dir + "/..."
		if dir == "." {
			p = "./..."
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// styleLinters 是「风格」级 linter。其余按「建议」处理——lint 从不产出「高危」，
// 高危是 AI 对安全/逻辑问题的判定，两者口径不同（§4.4）。
var styleLinters = map[string]bool{
	"gofmt": true, "gofumpt": true, "goimports": true, "gci": true, "lll": true,
	"revive": true, "stylecheck": true, "wsl": true, "nlreturn": true, "whitespace": true,
	"godot": true, "misspell": true, "tagliatelle": true, "dupl": true, "gocritic": true,
}

func Severity(fromLinter string) string {
	if styleLinters[fromLinter] {
		return "风格"
	}
	return "建议"
}

// Snippet 取 lint 给出的源码片段，用于计算指纹与界面展示。
func (i Issue) Snippet() string {
	if len(i.SourceLines) > 0 {
		return strings.Join(i.SourceLines, "\n")
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
