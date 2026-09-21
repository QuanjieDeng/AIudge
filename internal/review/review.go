package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aiudge/internal/config"
	"aiudge/internal/gitx"
	"aiudge/internal/gocx"
	"aiudge/internal/lint"
	"aiudge/internal/llm"
	"aiudge/internal/store"
)

const (
	maxCommits        = 30 // 单任务审的提交上限，超出截断并告警
	maxFilesPerCommit = 15
	maxCalleesPerFile = 5
	maxContextBytes   = 160 * 1024
)

// maxToolRounds 是单次审核里模型能进行的「检索 → 回填 → 再问」轮数上限。
// 是 var 不是 const：真实提交上撞过顶（日志里「工具调用轮次用尽，强制模型输出结论」），
// 要量化「放开轮次值不值」得能改它再跑对照，见 ab_test.go。
var maxToolRounds = 8

// fileJob 是「一个改动文件 + 它的审核上下文」，送进模型的最小单元。
type fileJob struct {
	commit gitx.Commit
	change gitx.FileChange
	ctx    string
}

// ErrConfig 标记「当前配置不可能跑通」的错误：模型没配、模型名不存在、凭据解不开。
// 这类错误重试没有意义——等再久配置也不会自己变好，只会把真正的错因埋进退避等待里。
var ErrConfig = errors.New("配置错误")

type Pipeline struct {
	Cfg  *config.Config
	DB   *store.DB
	Lint lint.Runner
}

// Logf 由调用方注入，日志同时落 task_logs 与进程日志（§4.3 要求留存执行日志）。
type Logf func(level, format string, args ...any)

type Result struct {
	ReportID int64
	Commits  []string
	Files    []string
	Issues   int
	High     int
	NewHigh  []store.Issue // 供通知模块即时推送高危（§4.7）
}

type aiIssue struct {
	File        string `json:"file"`
	LineStart   int    `json:"line_start"`
	LineEnd     int    `json:"line_end"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Suggestion  string `json:"suggestion"`
	Code        string `json:"code"`
}

// Run 执行一次完整审核。任何一步失败都直接返回 error，由队列决定重试。
func (p *Pipeline) Run(ctx context.Context, taskID int64, logf Logf) (*Result, error) {
	task, err := p.DB.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("任务不存在: %w", err)
	}
	repo, err := p.DB.GetRepo(task.RepoID)
	if err != nil {
		return nil, fmt.Errorf("仓库不存在: %w", err)
	}

	mc, err := p.DB.GetModelConfig()
	if err != nil {
		return nil, err
	}
	gatewayKey, err := p.DB.GatewayKey()
	if err != nil {
		return nil, err
	}
	if gatewayKey == "" && p.Cfg.GatewayKey != "" {
		gatewayKey = p.Cfg.GatewayKey
	}
	gatewayURL := mc.GatewayURL
	if gatewayURL == "" {
		gatewayURL = p.Cfg.GatewayURL
	}
	model := repo.DefaultModel
	if model == "" {
		model = mc.DefaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("%w: 未配置模型，请在模型配置里设默认模型，或给仓库单独指定", ErrConfig)
	}

	repoDir := filepath.Join(p.Cfg.WorkDir, fmt.Sprintf("repo-%d", repo.ID))
	credential, err := p.DB.RepoCredential(repo.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: 解密仓库凭据失败（AIUDGE_SECRET_KEY 与建库时不一致？）: %v", ErrConfig, err)
	}
	g := gitx.Open(repoDir)
	logf("info", "同步代码 %s (分支 %s)", gitx.Redact(repo.GitURL), repo.Branch)
	if err := g.Clone(repo.GitURL, repo.Branch, credential); err != nil {
		return nil, fmt.Errorf("同步代码失败: %w", err)
	}
	if eff := g.EffectiveRemote(); eff != "" && eff != gitx.Redact(repo.GitURL) {
		// 本机的 insteadOf / 凭据助手把地址换掉了。记一条是因为这种「能跑」是假的：
		// 它依赖的是这台机器的 ~/.gitconfig 和 SSH key，换台机器、换个用户跑就没了。
		// 要能带着走，就得显式配只读 token。
		logf("warn", "实际 remote 为 %s（被本机 git 配置改写过），"+
			"当前依赖本机的 git 凭据与 SSH key 才拉通；换台机器或换用户跑就会失败，"+
			"要能带着走请显式配置只读 token", eff)
	}

	// 规范文档在克隆之后读：相对路径要落在工作副本里（见 loadSpecDocs）
	docs := p.loadSpecDocs(repo, repoDir, logf)
	specVer := SpecVersion(docs)
	custom, err := p.DB.SystemPrompt()
	if err != nil {
		return nil, err
	}
	tmpl := EffectivePrompt(custom)
	promptVer := PromptVersion(tmpl)
	if err := p.DB.SetTaskVersions(taskID, model, "", promptVer, specVer); err != nil {
		return nil, err
	}
	logf("info", "开始审核 仓库=%s 语言=%s 模型=%s prompt=%s%s 规范=%s",
		repo.Name, repo.Language, model, promptVer, customNote(custom), specVer)

	commits, err := p.pickCommits(g, task, logf)
	if err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		logf("info", "没有需要审核的提交（可能已被 webhook 覆盖），任务视为成功")
		return &Result{}, nil
	}
	if err := g.Checkout(commits[0].SHA); err != nil {
		return nil, fmt.Errorf("切换到 %s 失败: %w", commits[0].SHA[:8], err)
	}

	// 逐提交收集改动文件与审核上下文
	tb := gocx.NewToolbox(repoDir, repo.Language, repo.IncludeGlobs, repo.ExcludeGlobs)
	var jobs []fileJob
	var files, lintFiles []string
	seenFile := map[string]bool{}
	total := 0
	for _, c := range commits {
		changes, err := g.CommitDiff(c.SHA)
		if err != nil {
			logf("warn", "提交 %s diff 解析失败: %v", c.SHA[:8], err)
			continue
		}
		n := 0
		for _, ch := range changes {
			if !gocx.MatchPath(ch.Path, repo.IncludeGlobs, repo.ExcludeGlobs) {
				continue
			}
			if n >= maxFilesPerCommit {
				logf("warn", "提交 %s 改动文件超过 %d 个，其余跳过", c.SHA[:8], maxFilesPerCommit)
				break
			}
			n++
			if !seenFile[ch.Path] {
				seenFile[ch.Path] = true
				files = append(files, ch.Path)
				// 删除的文件已经不在工作副本里了，交给 golangci-lint 只会让它报错，
				// 把整次 lint 拖成「执行失败」
				if !ch.Removed {
					lintFiles = append(lintFiles, ch.Path)
				}
			}
			ctxText := fileContext(repoDir, ch, tb)
			if ctxText == "" {
				continue
			}
			if total+len(ctxText) > maxContextBytes {
				logf("warn", "上下文已达 %dKB 上限，剩余改动不再送入模型", maxContextBytes/1024)
				break
			}
			total += len(ctxText)
			jobs = append(jobs, fileJob{commit: c, change: ch, ctx: ctxText})
		}
	}
	if len(jobs) == 0 {
		logf("info", "过滤后没有需要审核的 Go 改动（生成代码已在路径过滤中排除）")
	}

	// lint：确定性判定，零成本，全量并入同一问题库（§4.8）
	lintIssues, err := p.runLint(repo, repoDir, lintFiles, logf)
	if err != nil {
		logf("warn", "golangci-lint 执行失败，本次仅出 AI 结果: %v", err)
	}

	var aiIssues []aiIssue
	if len(jobs) > 0 {
		aiIssues, err = p.callAI(ctx, gatewayURL, gatewayKey, model, mc.TimeoutSec,
			tmpl, repo.Language, repo.Name, docs, commits, jobs, tb, logf)
		if err != nil {
			return nil, fmt.Errorf("AI 审核失败: %w", err)
		}
	}

	report := &store.Report{
		TaskID: taskID, RepoID: repo.ID, Commits: shaList(commits), Files: files,
		ModelName: model, ModelVersion: "", PromptVersion: promptVer, SpecVersion: specVer,
	}
	if err := p.DB.CreateReport(report); err != nil {
		return nil, err
	}

	res := &Result{ReportID: report.ID, Commits: report.Commits, Files: files}
	committerBySHA := map[string]gitx.Commit{}
	for _, c := range commits {
		committerBySHA[c.SHA] = c
	}

	// AI 问题入库
	for _, ai := range aiIssues {
		if !validAIIssue(ai) {
			continue
		}
		c := jobs[0].commit
		if len(jobs) > 0 && ai.File != "" {
			c = committerBySHA[commitOfFile(jobs, ai.File)]
			if c.SHA == "" {
				c = jobs[0].commit
			}
		}
		issue, notify := p.buildIssue(repo, report, store.SourceAI, ai.Category, ai.Severity,
			ai.File, ai.LineStart, ai.LineEnd, ai.Title, ai.Description, ai.Suggestion,
			snippetOf(repoDir, ai.File, ai.LineStart, ai.LineEnd, ai.Code), "", c)
		ur, err := p.DB.UpsertIssue(issue)
		if err != nil {
			logf("warn", "问题入库失败 %s:%d: %v", ai.File, ai.LineStart, err)
			continue
		}
		res.Issues++
		if issue.Severity == store.SeverityHigh {
			res.High++
		}
		if ur.IsRegression {
			logf("warn", "回归：%s:%d %s（原状态 %s）", ai.File, ai.LineStart, ai.Title, store.StatusNames[ur.PrevStatus])
		}
		if notify {
			issue.ID = ur.IssueID
			res.NewHigh = append(res.NewHigh, *issue)
		}
	}

	// lint 问题入库。lintKept 单独计数：lint 报的是改动包内的全部问题，
	// 其中大量落在本次没改的文件上，会被下面的 changed 过滤掉。
	// 直接拿 len(lintIssues) 去打印，就会出现「19 个问题（2+42）」这种自相矛盾的行
	changed := map[string]bool{}
	lintKept := 0
	for _, f := range files {
		changed[f] = true
	}
	for _, li := range lintIssues {
		path := strings.TrimPrefix(filepath.ToSlash(li.Pos.Filename), "./")
		if !changed[path] {
			continue
		}
		c := gitx.Commit{}
		for _, j := range jobs {
			if j.change.Path == path {
				c = j.commit
				break
			}
		}
		issue, _ := p.buildIssue(repo, report, store.SourceLint, store.CategorySpec, lint.Severity(li.FromLinter),
			path, li.Pos.Line, li.Pos.Line, fmt.Sprintf("%s: %s", li.FromLinter, li.Text), li.Text, "", li.Snippet(), li.FromLinter, c)
		ur, err := p.DB.UpsertIssue(issue)
		if err != nil {
			logf("warn", "lint 问题入库失败 %s:%d: %v", path, li.Pos.Line, err)
			continue
		}
		res.Issues++
		lintKept++
		if ur.IsRegression {
			logf("warn", "回归(lint)：%s:%d", path, li.Pos.Line)
		}
	}

	p.DB.MarkReviewed(repo.ID, report.Commits, taskID)
	// 回填统计字段
	_, _ = p.DB.Exec(`UPDATE review_reports SET issue_count=?,high_count=? WHERE id=?`, res.Issues, res.High, report.ID)
	logf("info", "审核完成：%d 个问题（高危 %d，AI %d 条，lint %d 条，lint 原始输出 %d 条已按改动文件过滤）",
		res.Issues, res.High, res.Issues-lintKept, lintKept, len(lintIssues))
	return res, nil
}

// pickCommits 决定本次要审哪些提交。webhook/手动走增量区间，
// 定时任务只捞 24 小时内未被覆盖的提交（§4.1：避免与 webhook 重复审同一批代码）。
func (p *Pipeline) pickCommits(g *gitx.Git, task *store.Task, logf Logf) ([]gitx.Commit, error) {
	if task.TriggerType == "cron" {
		commits, err := g.LogSince(time.Now().Add(-24*time.Hour), time.Time{}, 0)
		if err != nil {
			return nil, fmt.Errorf("读取提交列表失败: %w", err)
		}
		covered, err := p.DB.ReviewedSet(task.RepoID, store.Now()[:10]+" 00:00:00")
		if err != nil {
			return nil, err
		}
		var pending []gitx.Commit
		for _, c := range commits {
			if !covered[c.SHA] {
				pending = append(pending, c)
			}
		}
		logf("info", "定时兜底：24h 内 %d 个提交，其中 %d 个未被覆盖", len(commits), len(pending))
		return capCommits(pending, logf), nil
	}

	// 手动触发的时间范围口径。时间在 handler 里**不**换算成 SHA：那要读工作副本，
	// 而工作副本是上次审核留下的，可能落后远端几周，换算出来会静默漏掉新提交。
	// 到这儿才解析，是因为上面刚 fetch 过。
	if task.TimeFrom != "" || task.TimeTo != "" {
		since, until := parseTaskTime(task.TimeFrom), parseTaskTime(task.TimeTo)
		if since.IsZero() && until.IsZero() {
			return nil, fmt.Errorf("时间范围无法解析: %q..%q", task.TimeFrom, task.TimeTo)
		}
		commits, err := g.LogSince(since, until, maxCommits)
		if err != nil {
			return nil, fmt.Errorf("读取提交列表失败: %w", err)
		}
		logf("info", "时间范围 %s..%s 命中 %d 个提交",
			defaultStr(task.TimeFrom, "不限"), defaultStr(task.TimeTo, "不限"), len(commits))
		return capCommits(commits, logf), nil
	}

	if task.CommitTo != "" {
		if err := g.Checkout(task.CommitTo); err != nil {
			return nil, fmt.Errorf("切换到 %s 失败: %w", task.CommitTo, err)
		}
	}
	limit := 1 // 没给起点就只审最新一条，避免 webhook 一次带出整个历史
	since := task.CommitFrom
	if since != "" {
		limit = maxCommits
	}
	commits, err := g.Log(limit, since)
	if err != nil {
		return nil, fmt.Errorf("读取提交列表失败: %w", err)
	}
	return capCommits(commits, logf), nil
}

// parseTaskTime 把库里的时间字符串（store.TimeFmt，UTC）解析回 time.Time，空串/非法值
// 返回零值（零值在 gitx 里表示「不限」那一端）。
func parseTaskTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.ParseInLocation(store.TimeFmt, s, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

func capCommits(commits []gitx.Commit, logf Logf) []gitx.Commit {
	if len(commits) > maxCommits {
		logf("warn", "提交数 %d 超过上限 %d，只审最近 %d 条", len(commits), maxCommits, maxCommits)
		return commits[:maxCommits]
	}
	return commits
}

// buildFileContext 组装单文件送给模型的上下文：改动行所在函数的完整源码，
// 加上直接调用链中被调函数的实现（N+1、锁滥用这类问题必须看到被调方才能判断）。
// fileContext 按改动类型组装上下文。整文件删除没有新行可定位（文件在工作副本里已经
// 不存在了），buildFileContext 那种「读文件 + 圈函数」的路子走不通，改成把删掉的内容
// 原样交出去——「删了还有没有人用」只能靠这个判断。
func fileContext(repoDir string, ch gitx.FileChange, tb *gocx.Toolbox) string {
	if ch.Removed {
		return fmt.Sprintf("#### %s（**整个文件被本次提交删除**）\n删除的原始内容（行号是原文件行号）:\n```\n%s\n```\n"+
			"这个文件已经不在工作副本里了，行号请填上面的原文件行号，有问题的原始代码填进 code 字段。\n",
			ch.Path, strings.Join(ch.Deleted, "\n"))
	}
	return buildFileContext(repoDir, ch.Path, ch.Lines, tb) + deletedSnippet(ch.Deleted)
}

// deletedSnippet 附上本次纯删除的代码块。删掉的代码不在新增行的上下文里，
// 不显式带上，模型根本看不到「这次还删了什么」。
func deletedSnippet(deleted []string) string {
	if len(deleted) == 0 {
		return ""
	}
	return fmt.Sprintf("本次删除的代码（原文件行号）:\n```\n%s\n```\n", strings.Join(deleted, "\n"))
}

func buildFileContext(repoDir, rel string, ranges []gitx.LineRange, tb *gocx.Toolbox) string {
	src, err := os.ReadFile(filepath.Join(repoDir, rel))
	if err != nil {
		return ""
	}
	text := string(src)
	fset, file, perr := gocx.Parse(rel, src)
	if perr != nil {
		// 解析不了（构建标签外、语法不全）就退回改动块，不让单个文件拖垮整次审核
		return fmt.Sprintf("#### %s\n改动行: %s\n```go\n%s\n```\n", rel, fmtRanges(ranges), gocx.SliceLines(text, ranges, 3))
	}

	funcs := gocx.FuncsForLines(fset, file, ranges)
	if len(funcs) == 0 {
		return fmt.Sprintf("#### %s\n改动行: %s（不在任何函数内）\n```go\n%s\n```\n",
			rel, fmtRanges(ranges), gocx.SliceLines(text, ranges, 3))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "#### %s\n改动行: %s\n", rel, fmtRanges(ranges))
	callees := map[string]bool{}
	for _, fn := range funcs {
		fmt.Fprintf(&sb, "```go\n%s\n```\n", fn.Source(text))
		fd := findFuncDecl(file, fn.Name, fset)
		if fd == nil {
			continue
		}
		for _, c := range gocx.Callees(fd) {
			callees[c] = true
		}
	}
	// 直接调用链：只在同目录（同包）内解析，跨包调用靠模型自己用 grep/read_file 查
	included := 0
	for name := range callees {
		if included >= maxCalleesPerFile {
			break
		}
		srcText, label := resolveCallee(repoDir, rel, name, tb)
		if srcText == "" {
			continue
		}
		fmt.Fprintf(&sb, "被调函数 %s 的实现（同包内）:\n```go\n%s\n```\n", label, srcText)
		included++
	}
	return sb.String()
}

func resolveCallee(repoDir, rel, name string, tb *gocx.Toolbox) (string, string) {
	hits, err := tb.FindSymbol(name, 20)
	if err != nil || len(hits) == 0 {
		return "", ""
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	for _, h := range hits {
		if filepath.ToSlash(filepath.Dir(h.Path)) != dir {
			continue // 同名函数跨包不算「已有实现」，避免上下文里塞进无关代码
		}
		b, err := os.ReadFile(filepath.Join(repoDir, h.Path))
		if err != nil {
			continue
		}
		fset, f, err := gocx.Parse(h.Path, b)
		if err != nil {
			continue
		}
		fd := findFuncDecl(f, name, fset)
		if fd == nil {
			continue
		}
		start := fset.Position(fd.Pos()).Line
		end := fset.Position(fd.End()).Line
		if end-start > 60 {
			return "", "" // 太长的被调函数只给签名，避免上下文爆炸
		}
		fn := gocx.Func{Name: name, StartLine: start, EndLine: end}
		return fn.Source(string(b)), h.Path + ":" + fmt.Sprint(start)
	}
	return "", ""
}

// findFuncDecl 按名字在文件里找函数声明。只看顶层声明，嵌套字面量里的方法不算。
func findFuncDecl(f *ast.File, name string, fset *token.FileSet) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

func fmtRanges(ranges []gitx.LineRange) string {
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		if r.Start == r.End {
			parts = append(parts, fmt.Sprint(r.Start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", r.Start, r.End))
		}
	}
	return strings.Join(parts, ",")
}

// loadSpecDocs 读取仓库挂载的规范文档。两种写法：
//
//	相对路径 —— 在仓库工作副本内解析，天然受限于仓库本身，任何项目都能挂自己的
//	           CLAUDE.md / docs/review-checklist.md，零配置
//	绝对路径 —— 必须落在 AIUDGE_SPEC_DIRS 白名单下，用于挂仓库外的共享文档
//
// 读不到的文档只记一条日志跳过，不让一次路径写错把整个审核任务打失败。
func (p *Pipeline) loadSpecDocs(repo *store.Repository, repoDir string, logf Logf) map[string]string {
	docs := map[string]string{}
	for _, d := range repo.SpecDocs {
		full, ok := p.resolveSpecDoc(d, repoDir)
		if !ok {
			logf("warn", "规范文档 %s 不在允许范围内（绝对路径需在 AIUDGE_SPEC_DIRS 下，或在仓库内用相对路径），已跳过", d)
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil {
			logf("warn", "规范文档 %s 读取失败，已跳过: %v", d, err)
			continue
		}
		docs[d] = string(b)
	}
	return docs
}

// customNote 让任务日志一眼看出用的是内置 prompt 还是自定义的。
// 排查「为什么这次审出来的东西不一样」时，这一条能省掉一轮猜测。
func customNote(custom string) string {
	if custom == "" {
		return "（内置）"
	}
	return "（自定义）"
}

func (p *Pipeline) resolveSpecDoc(doc, repoDir string) (string, bool) {
	if doc == "" {
		return "", false
	}
	if !filepath.IsAbs(doc) {
		full := filepath.Join(repoDir, filepath.Clean(doc))
		// 相对路径也不许跳出工作副本（../../etc/shadow 这类）
		if !strings.HasPrefix(full, repoDir+string(filepath.Separator)) {
			return "", false
		}
		return full, true
	}
	clean := filepath.Clean(doc)
	for _, root := range p.Cfg.SpecDirs {
		root = filepath.Clean(root)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return clean, true
		}
	}
	return "", false
}

func (p *Pipeline) runLint(repo *store.Repository, repoDir string, files []string, logf Logf) ([]lint.Issue, error) {
	// 目前只集成了 golangci-lint。其他语言先跳过——硬跑 golangci-lint 只会报错，
	// 把「没有 lint 结果」伪装成「没有规范问题」更糟，所以明确说一句
	if lang := strings.ToLower(repo.Language); lang != "" && lang != "go" {
		logf("info", "%s 仓库暂未接入 lint 工具，本次仅出 AI 结果", repo.Language)
		return nil, nil
	}
	dirs := lint.PkgDirs(files, repo.Language)
	if len(dirs) == 0 {
		return nil, nil
	}
	logf("info", "golangci-lint 检查 %d 个包目录", len(dirs))
	return p.Lint.Run(repoDir, dirs)
}

// callAI 跑「对话 → 工具调用 → 回填 → 再对话」的循环，直到模型给出最终 JSON。
func (p *Pipeline) callAI(ctx context.Context, gatewayURL, apiKey, model string, timeoutSec int,
	tmpl, lang, repoName string, docs map[string]string, commits []gitx.Commit, jobs []fileJob,
	tb *gocx.Toolbox, logf Logf) ([]aiIssue, error) {

	cli := llm.New(gatewayURL, apiKey, model, time.Duration(timeoutSec)*time.Second)
	msgs := []llm.Message{
		{Role: "system", Content: buildSystem(tmpl, lang, repoName, docs)},
		{Role: "user", Content: buildUserPrompt(commits, jobs)},
	}
	tools := toolDefs(lang)

	for round := 0; round < maxToolRounds; round++ {
		reply, err := cli.Chat(ctx, msgs, tools, false)
		if err != nil {
			return nil, p.explainModelError(ctx, cli, model, err)
		}
		logf("info", "第 %d 轮：tokens=%d 耗时=%.1fs", round+1, reply.Usage.TotalTokens, reply.Elapsed.Seconds())

		if len(reply.Message.ToolCalls) == 0 {
			issues, err := parseAIOutput(reply.Message.Content)
			if err != nil {
				return nil, err
			}
			return issues, nil
		}
		msgs = append(msgs, reply.Message)
		for _, tc := range reply.Message.ToolCalls {
			out := execTool(tb, tc.Function.Name, tc.Function.Arguments)
			logf("info", "工具 %s%s → %d 字节", tc.Function.Name, truncateRunes(tc.Function.Arguments, 120), len(out))
			msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}

	// 轮次用尽：再要一次纯 JSON，不再给工具，强制收敛
	logf("warn", "工具调用轮次用尽，强制模型输出结论")
	msgs = append(msgs, llm.Message{Role: "user", Content: "检索轮次已用尽。现在只输出最终 JSON 结论，不要再请求调用工具。"})
	reply, err := cli.Chat(ctx, msgs, nil, true)
	if err != nil {
		return nil, p.explainModelError(ctx, cli, model, err)
	}
	return parseAIOutput(reply.Message.Content)
}

// explainModelError 在模型名不被网关接受时，顺手把网关的可用模型列进错误里。
// 这类错最常见的原因是填了别处的模型别名，光看「Invalid model name」猜不出来该填什么。
func (p *Pipeline) explainModelError(ctx context.Context, cli *llm.Client, model string, err error) error {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || !apiErr.IsModelError() {
		return err
	}
	models, merr := cli.Models(ctx)
	if merr != nil || len(models) == 0 {
		return fmt.Errorf("%w；当前配置的模型是 %q，请到「设置 → 模型配置」点「从网关拉取」核对可用的模型名", err, model)
	}
	return fmt.Errorf("%w；当前配置的模型是 %q，网关可用模型：%s", err, model, strings.Join(models, ", "))
}

func execTool(tb *gocx.Toolbox, name, argsJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("参数解析失败: %v", err)
	}
	str := func(k string) string { s, _ := args[k].(string); return s }
	num := func(k string, def int) int {
		if f, ok := args[k].(float64); ok {
			return int(f)
		}
		return def
	}

	switch name {
	case "grep":
		hits, err := tb.Grep(str("pattern"), num("max_hits", 40))
		if err != nil {
			return "检索失败: " + err.Error()
		}
		return joinHits(hits)
	case "find_symbol":
		hits, err := tb.FindSymbol(str("name"), num("max_hits", 20))
		if err != nil {
			return "检索失败: " + err.Error()
		}
		return joinHits(hits)
	case "read_file":
		out, err := tb.ReadFile(str("path"), num("start", 1), num("end", 0))
		if err != nil {
			return "读取失败: " + err.Error()
		}
		return capBytes(out, 20000)
	case "list_dir":
		ents, err := tb.ListDir(defaultStr(str("path"), "."))
		if err != nil {
			return "列目录失败: " + err.Error()
		}
		return strings.Join(ents, "\n")
	default:
		return "未知工具: " + name
	}
}

func joinHits(hits []gocx.Hit) string {
	if len(hits) == 0 {
		return "（无匹配）"
	}
	var sb strings.Builder
	for _, h := range hits {
		sb.WriteString(h.String())
		sb.WriteString("\n")
	}
	return capBytes(sb.String(), 20000)
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// parseAIOutput 从模型回复里抠出 JSON。模型经常裹一层 ```json 代码块或加解释文字，
// 这里按「第一个 { 到最后一个 }」取，失败时把原文带进错误便于排查 prompt。
func parseAIOutput(content string) ([]aiIssue, error) {
	s := strings.TrimSpace(content)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var out struct {
		Issues []aiIssue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("模型输出不是合法 JSON: %v；原文: %s", err, truncateRunes(content, 500))
	}
	return out.Issues, nil
}

// validAIIssue 挡住模型越界产出：lint 独占「规范/风格」，AI 只允许四类两档（§4.8）。
func validAIIssue(a aiIssue) bool {
	if a.File == "" || a.Title == "" {
		return false
	}
	switch a.Severity {
	case store.SeverityHigh, store.SeverityAdvice:
	default:
		return false
	}
	switch a.Category {
	case store.CategorySecurity, store.CategoryPerf, store.CategoryLogic, store.CategoryDup:
		return true
	}
	return false
}

func (p *Pipeline) buildIssue(repo *store.Repository, report *store.Report, source, category, severity,
	file string, lineStart, lineEnd int, title, desc, suggestion, snippet, linter string, c gitx.Commit) (*store.Issue, bool) {

	committerID, isBot := p.DB.ResolveCommitter(c.Author)
	if isBot {
		committerID = 0 // 机器人不计入任何统计（§4.8）
	}
	issue := &store.Issue{
		Fingerprint: Fingerprint(repo.ID, file, category, snippet, linter),
		RepoID:      repo.ID,
		ReportID:    report.ID,
		Source:      source,
		Category:    category,
		Severity:    severity,
		FilePath:    file,
		LineStart:   lineStart,
		LineEnd:     lineEnd,
		Title:       title,
		Description: desc,
		Suggestion:  suggestion,
		CodeSnippet: snippet,
		Linter:      linter,
		CommitSHA:   c.SHA,
		Committer:   c.Author,
		CommitterID: committerID,
	}
	notify := severity == store.SeverityHigh
	return issue, notify
}

// snippetOf 优先取仓库里的真实代码行做片段——比模型复述的代码稳定，
// 指纹才不会因为模型换了个说法就漂移。取不到时退回模型给的 code。
func snippetOf(repoDir, rel string, start, end int, fallback string) string {
	s := readLines(repoDir, rel, start, end)
	if s == "" {
		s = fallback
	}
	return truncateRunes(s, 2000)
}

func readLines(repoDir, rel string, start, end int) string {
	b, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return ""
	}
	return strings.Join(lines[start-1:end], "\n")
}

func commitOfFile(jobs []fileJob, path string) string {
	for _, j := range jobs {
		if j.change.Path == path {
			return j.commit.SHA
		}
	}
	return ""
}

// buildUserPrompt 把本次提交与逐文件的改动上下文拼成用户消息。
// 明确标出改动行号，让模型知道「哪里是新代码」——否则它会去报存量问题。
func buildUserPrompt(commits []gitx.Commit, jobs []fileJob) string {
	var sb strings.Builder
	sb.WriteString("## 本次审核的提交\n\n")
	for _, c := range commits {
		fmt.Fprintf(&sb, "- %s %s <%s> %s\n", c.SHA[:8], c.Author, c.Email, c.Subject)
	}
	sb.WriteString("\n## 改动文件与上下文\n\n")
	for _, j := range jobs {
		fmt.Fprintf(&sb, "<!-- commit %s -->\n", j.commit.SHA[:8])
		sb.WriteString(j.ctx)
		sb.WriteString("\n")
	}
	sb.WriteString("\n请只针对上面标注的改动行报问题。需要确认跨文件影响时调用检索工具。" +
		"本次删除的代码（尤其整文件删除）也要判断「删掉的东西还有没有人用」——被删的符号、文件还被别处引用着就是问题。")
	return sb.String()
}

func shaList(commits []gitx.Commit) []string {
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		out = append(out, c.SHA)
	}
	return out
}

func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...(已截断)"
}
