// Package gitx 用本地浅克隆做代码获取与 diff 解析。审核粒度为「改动行所在函数 +
// 直接调用链」（§附录决策 16），所以这里必须给出精确的新文件行号范围。
package gitx

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Git struct {
	Dir string
}

func Open(dir string) *Git { return &Git{Dir: dir} }

type Commit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Email   string `json:"email"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type FileChange struct {
	Path    string      `json:"path"`
	Lines   []LineRange `json:"lines"`             // 新文件中的改动行，全新增/全删文件时为空
	Removed bool        `json:"removed,omitempty"` // 整个文件被删除
	Deleted []string    `json:"deleted,omitempty"` // 纯删除的内容（带原文件行号），供「删了还有没有人用」判断
}

func (g *Git) run(timeout time.Duration, args ...string) (string, error) {
	return g.runIn(g.Dir, timeout, args...)
}

// runIn 允许指定工作目录。clone 时目标目录还不存在，必须落在父目录里跑。
func (g *Git) runIn(dir string, timeout time.Duration, args ...string) (string, error) {
	// core.quotepath=false：不加这个，含非 ASCII 的路径会被 git 转成带引号的八进制转义
	// （"b/xxx/\345\274\200.md"），后面剥 b/ 前缀、按路径读文件全部对不上，
	// 中文目录下的改动会静默地不进审核上下文。写在最前面，对所有子命令生效。
	sub := args[0] // 下面报错要用子命令名，args[0] 会被上面的 -c 顶掉
	args = append([]string{"-c", "core.quotepath=false"}, args...)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // 凭据错时不要卡在交互式输入
		"GIT_ASKPASS=/bin/true",
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return out.String(), fmt.Errorf("git %s: %v: %s", sub, err, truncate(errb.String(), 500))
		}
		return out.String(), nil
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("git %s 超时", sub)
	}
}

// Clone 或 Fetch。工作副本按仓库名分目录，长期复用，避免每次全量克隆。
func (g *Git) Clone(url, branch, credential string) error {
	if _, err := os.Stat(filepath.Join(g.Dir, ".git")); err == nil {
		return g.Fetch(url, branch, credential)
	}
	parent := filepath.Dir(g.Dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(g.Dir)
	if err != nil {
		return err
	}
	_, err = g.runIn(parent, 10*time.Minute, "clone", "--depth", "100", "--no-tags",
		"--branch", branch, authURL(url, credential), abs)
	return err
}

func (g *Git) Fetch(url, branch, credential string) error {
	_, err := g.run(10*time.Minute, "fetch", "--depth", "100", "--no-tags", authURL(url, credential), branch)
	if err != nil {
		return err
	}
	// fetch 只写 FETCH_HEAD，本地分支停在原地不动。不显式重置的话 HEAD 永远是上次
	// 审过的位置，`since..HEAD` 恒为空——任务会「成功但什么都没审」，静默漏检。
	_, err = g.run(2*time.Minute, "checkout", "-f", "-B", branch, "FETCH_HEAD")
	return err
}

// authURL 把只读 token 注入 URL。调用方禁止把返回值写日志（§5）。
func authURL(raw, credential string) string {
	if credential == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return raw
	}
	u.User = url.UserPassword("oauth2", credential)
	return u.String()
}

// Redact 给日志用，抹掉 URL 里的凭据。
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("***")
	return u.String()
}

// Cloned 判断工作副本是否已经拉过。
func (g *Git) Cloned() bool {
	_, err := os.Stat(filepath.Join(g.Dir, ".git"))
	return err == nil
}

// SyncBranch 只拉对象，**不动工作区**（对比 Clone/Fetch 末尾那次 checkout -B）。
//
// 界面上的提交下拉要在审核任务可能正在跑的时候同步：任何 checkout 都会和它抢工作区，
// 轻则审核读到半新半旧的源码，重则 index.lock 冲突把审核任务打成失败。
// 拉完 FETCH_HEAD 就是远端分支尖端，列表从它取；FETCH_HEAD 被并发改写是无害的，
// 管线自己的 Fetch 在 checkout -B 之后就不再读它。
func (g *Git) SyncBranch(url, branch, credential string) error {
	_, err := g.run(2*time.Minute, "fetch", "--depth", "100", "--no-tags", authURL(url, credential), branch)
	return err
}

// log 是取提交列表的统一入口。limit<=0 表示不限条数，extra 里放范围/时间条件与 ref。
func (g *Git) log(limit int, extra ...string) ([]Commit, error) {
	args := []string{"log", "--no-merges"}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	args = append(args, "--format=%H%x1f%an%x1f%ae%x1f%aI%x1f%s")
	out, err := g.run(time.Minute, append(args, extra...)...)
	if err != nil {
		return nil, err
	}
	commits := []Commit{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) < 5 {
			continue
		}
		commits = append(commits, Commit{SHA: f[0], Author: f[1], Email: f[2], Date: f[3], Subject: f[4]})
	}
	return commits, nil
}

// Log 列出自 since 之后（不含）的提交。since 为空则取最近 limit 条。
func (g *Git) Log(limit int, sinceSHA string) ([]Commit, error) {
	rng := "HEAD"
	if sinceSHA != "" {
		rng = sinceSHA + "..HEAD"
	}
	return g.log(limit, rng)
}

// LogSince 列出 [since, until] 时间窗内的提交（§4.1）。until 为零值表示不限上界，
// limit<=0 表示不限条数。定时兜底只用 since，手动触发的时间范围两个都用。
func (g *Git) LogSince(since, until time.Time, limit int) ([]Commit, error) {
	extra := []string{"--since=" + since.UTC().Format(time.RFC3339)}
	if !until.IsZero() {
		extra = append(extra, "--until="+until.UTC().Format(time.RFC3339))
	}
	return g.log(limit, append(extra, "HEAD")...)
}

// LogRef 列某个 ref 的最近 limit 条提交。手动触发的提交下拉传 FETCH_HEAD：
// 那个位置不取决于工作区当前 checkout 到哪儿（审核任务会把它切来切去）。
func (g *Git) LogRef(limit int, ref string) ([]Commit, error) { return g.log(limit, ref) }

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// CommitDiff 解析单个提交相对其首个父提交的改动文件与新增行范围。
// 合并提交按 --first-parent 口径，与常见 GitLab 展示一致。
func (g *Git) CommitDiff(sha string) ([]FileChange, error) {
	out, err := g.run(2*time.Minute, "show", "--no-color", "--unified=0", "--format=", "--first-parent", sha)
	if err != nil {
		return nil, err
	}
	return parseUnifiedDiff(out), nil
}

// parseUnifiedDiff 把 `git show --unified=0` 的输出解成「文件 → 新文件改动行区间」。
//
// 删除也要收：早先整文件删除（+++ /dev/null）被直接跳过，纯删除的 hunk 也只丢个空区间，
// 于是「删掉的东西还有没有人用」这一类问题结构上审不出来——删文件、删公共函数、
// 删唯一调用点全在这个盲区里。纯删除没有新行号可圈，就把删掉的原文带上交给模型。
func parseUnifiedDiff(diff string) []FileChange {
	var files []FileChange
	var cur *FileChange
	var oldPath string
	var collect bool // 当前 hunk 是新侧 0 行的纯删除，把 - 行收进 Deleted
	inHunk, oldLine := false, 0

	for _, line := range strings.Split(diff, "\n") {
		switch {
		// hunk 内的 - 行内容是任意文本（Markdown 的 `---` 分隔线尤其常见），
		// 所以头部识别必须限定在 hunk 之外，否则删掉的内容会被当成分隔进来的文件头
		case strings.HasPrefix(line, "diff --git"):
			cur, inHunk, collect, oldPath = nil, false, false, ""
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			if cur == nil {
				continue
			}
			m := hunkRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			oldStart, _ := strconv.Atoi(m[1])
			start, _ := strconv.Atoi(m[2])
			count := 1
			if m[3] != "" {
				count, _ = strconv.Atoi(m[3])
			}
			oldLine = oldStart
			collect = count == 0 // 纯删除：新侧一行都没有
			if collect {
				continue
			}
			cur.Lines = append(cur.Lines, LineRange{Start: start, End: start + count - 1})
		case !inHunk && strings.HasPrefix(line, "--- "):
			oldPath = diffPath(strings.TrimPrefix(line, "--- "))
		case !inHunk && strings.HasPrefix(line, "+++ "):
			p := diffPath(strings.TrimPrefix(line, "+++ "))
			removed := p == "/dev/null"
			if removed {
				p = oldPath // 整个文件被删掉，路径只能从 --- 那行取
			}
			cur = nil
			if p == "" {
				continue
			}
			files = append(files, FileChange{Path: p, Removed: removed})
			cur = &files[len(files)-1]
		case inHunk && collect && cur != nil && strings.HasPrefix(line, "-"):
			cur.Deleted = append(cur.Deleted, fmt.Sprintf("%d: %s", oldLine, strings.TrimPrefix(line, "-")))
			oldLine++
		}
	}
	return files
}

// diffPath 从 diff 头部取出仓库相对路径。git 对含空格、引号、反斜杠的路径会加引号并做
// C 风格转义（"b/my file.md"），非 ASCII 的路径也走同一条路（另见 runIn 的 core.quotepath）。
func diffPath(p string) string {
	if strings.HasPrefix(p, `"`) {
		if s, err := strconv.Unquote(p); err == nil {
			p = s
		}
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	return p
}

// Checkout 把工作副本切到指定提交，供 AI 检索工具读到与分析一致的快照。
func (g *Git) Checkout(sha string) error {
	_, err := g.run(2*time.Minute, "checkout", "-f", sha)
	return err
}

// EffectiveRemote 返回工作副本实际的 origin 地址。它未必等于界面里填的 Git 地址：
// 机器上的 `url.<base>.insteadOf` 会把 https 改写成 ssh，凭据也就跟着换成了
// 本机的 SSH key。审核日志里报这一条，是为了让「没填凭据却拉下来了」可解释。
func (g *Git) EffectiveRemote() string {
	out, err := g.run(time.Minute, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return Redact(strings.TrimSpace(out))
}

func (g *Git) HeadSHA() (string, error) {
	out, err := g.run(time.Minute, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
