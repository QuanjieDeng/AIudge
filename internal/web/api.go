package web

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"aiudge/internal/gitx"
	"aiudge/internal/gocx"
	"aiudge/internal/llm"
	"aiudge/internal/review"
	"aiudge/internal/store"
)

// ---------- 仓库管理（§4.1） ----------

func (s *Server) handleRepoList(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	pids, all := s.visibleProjects(u)
	repos, err := s.db.ListRepos(pids, all)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 带上当前用户在每个仓库上的权限，前端据此决定按钮灰不灰
	type repoView struct {
		store.Repository
		CanAdmin   bool `json:"can_admin"`
		CanTrigger bool `json:"can_trigger"`
	}
	out := make([]repoView, 0, len(repos))
	for _, rp := range repos {
		out = append(out, repoView{
			Repository: rp,
			CanAdmin:   s.canAdminRepo(u, &rp),
			CanTrigger: s.canTriggerRepo(u, &rp),
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleRepoGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	repo, err := s.db.GetRepo(id)
	if err != nil {
		writeErr(w, 404, "仓库不存在")
		return
	}
	if !s.canViewRepo(userFrom(r), repo) {
		writeErr(w, 403, "无权访问该仓库")
		return
	}
	writeJSON(w, 200, repo)
}

func (s *Server) handleRepoCreate(w http.ResponseWriter, r *http.Request) {
	var in store.RepoInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.GitURL) == "" {
		writeErr(w, 400, "仓库名称与 Git 地址必填")
		return
	}
	if len(in.ExcludeGlobs) == 0 {
		// 默认排除生成代码：占变更量约 45%，不排除会烧掉上下文并污染分类分布（§附录决策 8）
		in.ExcludeGlobs = []string{"autogen", "*.pb.go", "/tbl/"}
	}
	u := userFrom(r)
	if in.ProjectID == 0 {
		writeErr(w, 400, "请指定所属项目")
		return
	}
	if !s.db.CanAdminProject(u, in.ProjectID) {
		writeErr(w, 403, "需要该项目管理员权限")
		return
	}
	id, err := s.db.CreateRepo(in)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.db.AppendAudit(u.ID, u.Name, "repo.create", "repo", strconv.FormatInt(id, 10), in.Name)
	_ = s.sched.Reload()
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) handleRepoUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepoForWrite(w, r)
	if !ok {
		return
	}
	id := repo.ID
	var in store.RepoInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	// 不允许通过改配置把仓库挪到别人管的项目下
	if in.ProjectID != repo.ProjectID && !s.canAdminRepo(userFrom(r), &store.Repository{ProjectID: in.ProjectID}) {
		writeErr(w, 403, "无权把仓库移到该项目")
		return
	}
	if err := s.db.UpdateRepo(id, in); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "repo.update", "repo", strconv.FormatInt(id, 10), in.Name)
	_ = s.sched.Reload() // cron 表达式可能变了
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepoForWrite(w, r)
	if !ok {
		return
	}
	id := repo.ID
	if err := s.db.DeleteRepo(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "repo.delete", "repo", strconv.FormatInt(id, 10), "")
	_ = s.sched.Reload()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleRepoTrigger 支持手动触发指定 commit 范围或时间范围（§4.1）。
//
// 这里是「手动触发」权限的落点：会直接消耗模型额度，所以不能只要求「看得见这个仓库」。
func (s *Server) handleRepoTrigger(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepoForRead(w, r)
	if !ok {
		return
	}
	if !s.canTriggerRepo(userFrom(r), repo) {
		writeErr(w, 403, "没有手动触发权限，请让项目管理员在成员管理里授予")
		return
	}
	if !repo.Enabled {
		writeErr(w, 400, "该仓库已停用审核，先启用再触发")
		return
	}
	var in struct {
		From     string `json:"from"`
		To       string `json:"to"`
		TimeFrom string `json:"time_from"`
		TimeTo   string `json:"time_to"`
	}
	_ = readJSON(r, &in)
	from, to := strings.TrimSpace(in.From), strings.TrimSpace(in.To)
	// 两类范围互斥：同时给的话按哪边审是含糊的，宁可让人明确选一个
	if (from != "" || to != "") && (in.TimeFrom != "" || in.TimeTo != "") {
		writeErr(w, 400, "提交范围与时间范围只能选一个")
		return
	}
	task := &store.Task{
		RepoID: repo.ID, TriggerType: "manual", CommitFrom: from, CommitTo: to,
		Status: "queued", ModelName: repo.DefaultModel,
	}
	// 时间在本地时区输入，转成库里统一的 UTC 字符串存；*解析不了就报错*，
	// 不能把非法值存成空串——那会变成「不限时间」，静默审整个历史
	if in.TimeFrom != "" {
		task.TimeFrom = localToStoreTime(in.TimeFrom)
		if task.TimeFrom == "" {
			writeErr(w, 400, "起始时间格式不对")
			return
		}
	}
	if in.TimeTo != "" {
		task.TimeTo = localToStoreTime(in.TimeTo)
		if task.TimeTo == "" {
			writeErr(w, 400, "结束时间格式不对")
			return
		}
	}
	if err := s.db.CreateTask(task); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.q.Submit(task.ID)
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "task.trigger", "repo", strconv.FormatInt(repo.ID, 10),
		"手动触发 "+triggerRange(task))
	writeJSON(w, 200, map[string]any{"task_id": task.ID})
}

// triggerRange 是审计与提示用的范围描述。
func triggerRange(t *store.Task) string {
	if t.TimeFrom != "" || t.TimeTo != "" {
		return "时间 " + defaultStr(t.TimeFrom, "不限") + ".." + defaultStr(t.TimeTo, "不限")
	}
	if t.CommitFrom == "" && t.CommitTo == "" {
		return "最新一条提交"
	}
	return "提交 " + defaultStr(t.CommitFrom, "不限") + ".." + defaultStr(t.CommitTo, "最新")
}

// localToStoreTime 把界面上填的本地时间（HTML datetime-local 的 `2006-01-02T15:04`）
// 转成库里统一的 UTC 字符串，格式不对返回空串。
func localToStoreTime(v string) string {
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", store.TimeFmt} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t.UTC().Format(store.TimeFmt)
		}
	}
	return ""
}

// ---------- 手动触发的提交下拉 ----------

// handleRepoCommits 给手动触发的提交下拉列候选（§4.1）。
//
// 打开下拉时先同步一次远端，所以列表总是新的；同步只 fetch、不 checkout——
// 审核任务可能正在同一个工作副本上跑（见 gitx.SyncBranch 的注释）。
// 同步失败不是致命错误：退回本地副本的列表并如实标出 synced=false，界面自己决定怎么提示。
func (s *Server) handleRepoCommits(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepoForRead(w, r)
	if !ok {
		return
	}
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}

	dir := filepath.Join(s.cfg.WorkDir, fmt.Sprintf("repo-%d", repo.ID))
	g := gitx.Open(dir)
	cred, credErr := s.db.RepoCredential(repo.ID)
	var syncErr error
	ref := "FETCH_HEAD" // fetch 出来的远端分支尖端
	switch {
	case credErr != nil:
		syncErr, ref = credErr, "HEAD"
	case !g.Cloned():
		// 还没同步过：整份克隆一次。这么长的一步只在首次点开下拉时发生，
		// 之后走下面那条只 fetch 的路。克隆完 HEAD 就是分支尖端——
		// FETCH_HEAD 是 fetch 才会写的，这时候还没有。
		syncErr, ref = g.Clone(repo.GitURL, repo.Branch, cred), "HEAD"
	default:
		syncErr = g.SyncBranch(repo.GitURL, repo.Branch, cred)
	}
	synced, syncedAt := syncErr == nil, ""
	if synced {
		syncedAt = store.Now()
	} else {
		ref = "HEAD" // 同步失败：退回本地副本的位置
	}
	msg := ""
	if syncErr != nil {
		// git 的报错里可能带着注入了 token 的地址，抹掉再回给界面（§5）
		msg = gitx.Redact(syncErr.Error())
	}

	commits, err := g.LogRef(limit, ref)
	if err != nil && ref != "HEAD" {
		// FETCH_HEAD 没读出来就退回本地副本：能列出东西比坚持报错有用
		synced = false
		commits, err = g.LogRef(limit, "HEAD")
	}
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"items": []gitx.Commit{}, "synced": false,
			"sync_error": "读取提交列表失败：这个仓库还没同步过工作副本",
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"items": commits, "synced": synced, "synced_at": syncedAt, "sync_error": msg,
	})
}

// ---------- 模型配置（§4.2） ----------

// handleModelConfigGet 返回**实际生效**的值：库里存了用库里的，没存回落配置文件。
//
// 早先直接返回库里的行，界面就显示空——而审核管线那边是回落配置文件的，
// 于是「界面上看着没配、实际在用」，两边说的不是一回事。
func (s *Server) handleModelConfigGet(w http.ResponseWriter, r *http.Request) {
	mc, err := s.db.GetModelConfig()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	source := ""
	if mc.GatewayURL == "" && s.cfg.GatewayURL != "" {
		mc.GatewayURL = s.cfg.GatewayURL
		source = "配置文件"
	} else if mc.GatewayURL != "" {
		source = "界面"
	}
	writeJSON(w, 200, map[string]any{
		"gateway_url": mc.GatewayURL, "gateway_source": source,
		"gateway_key_mask": mc.GatewayKeyMask, "has_gateway_key": mc.HasGatewayKey,
		"timeout_sec": mc.TimeoutSec, "models": mc.Models,
		"default_model": mc.DefaultModel, "updated_at": mc.UpdatedAt,
	})
}

func (s *Server) handleModelConfigPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GatewayURL   string   `json:"gateway_url"`
		GatewayKey   *string  `json:"gateway_key"`
		TimeoutSec   int      `json:"timeout_sec"`
		Models       []string `json:"models"`
		DefaultModel string   `json:"default_model"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if in.TimeoutSec <= 0 {
		in.TimeoutSec = 600
	}
	if err := s.db.UpdateModelConfig(in.GatewayURL, in.GatewayKey, in.TimeoutSec, in.Models, in.DefaultModel); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "model.update", "model_config", "1", "默认模型="+in.DefaultModel)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleModelRefresh 从网关 /models 拉列表（§4.2）。
// 请求体可以带上还没保存的 gateway_url / gateway_key，这样界面上「先改地址再拉一下试试」
// 不会把已有的默认模型和列表先写坏——拉取是只读操作，不该有副作用。
func (s *Server) handleModelRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GatewayURL string  `json:"gateway_url"`
		GatewayKey *string `json:"gateway_key"`
	}
	_ = readJSON(r, &in)

	mc, err := s.db.GetModelConfig()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	key, _ := s.db.GatewayKey()
	if key == "" {
		key = s.cfg.GatewayKey
	}
	if in.GatewayKey != nil && *in.GatewayKey != "" {
		key = *in.GatewayKey
	}
	url := mc.GatewayURL
	if in.GatewayURL != "" {
		url = in.GatewayURL
	}
	if url == "" {
		url = s.cfg.GatewayURL
	}
	ctx, cancel := timeoutCtx(r, 30*time.Second)
	defer cancel()
	models, err := llm.New(url, key, "", 30*time.Second).Models(ctx)
	if err != nil {
		writeErr(w, 502, fmt.Sprintf("拉取模型列表失败（已尝试 %s/models 与 %s/v1/models）: %v",
			strings.TrimSuffix(url, "/"), strings.TrimSuffix(url, "/"), err))
		return
	}
	if err := s.db.UpdateModelConfig(url, nil, mc.TimeoutSec, models, mc.DefaultModel); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 拉到了列表就顺手核对当前默认模型还在不在里面。模型名对不上是首次配置最常踩的坑，
	// 而且错得很隐蔽——要等到任务跑起来、白等一次重试才暴露。
	warn := ""
	if mc.DefaultModel != "" && !slices.Contains(models, mc.DefaultModel) {
		warn = fmt.Sprintf("当前默认模型 %q 不在网关返回的列表里，请重新选择", mc.DefaultModel)
	}
	writeJSON(w, 200, map[string]any{"models": models, "warn": warn})
}

// ---------- 任务（§4.3） ----------

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	u := userFrom(r)
	pids, all := s.visibleProjects(u)
	pids, all = scopeToProject(queryInt64(r, "project_id"), pids, all)
	tasks, total, err := s.db.ListTasks(queryInt64(r, "repo_id"), r.URL.Query().Get("status"), limit, offset, pids, all)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": tasks, "total": total})
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	task, err := s.db.GetTask(id)
	if err != nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	if repo, err := s.db.GetRepo(task.RepoID); err != nil || !s.canViewRepo(userFrom(r), repo) {
		writeErr(w, 403, "无权访问该任务")
		return
	}
	writeJSON(w, 200, task)
}

func (s *Server) handleTaskLogs(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	logs, err := s.db.ListTaskLogs(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, logs)
}

func (s *Server) handleTaskRetry(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	task, err := s.db.GetTask(id)
	if err != nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	if task.Status == "running" {
		writeErr(w, 409, "任务正在执行中")
		return
	}
	if err := s.db.RetryTask(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.q.Submit(id)
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "task.retry", "task", strconv.FormatInt(id, 10), "")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 报告（§4.4） ----------

func (s *Server) handleReportList(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	from, to := timeRange(r)
	u := userFrom(r)
	pids, all := s.visibleProjects(u)
	pids, all = scopeToProject(queryInt64(r, "project_id"), pids, all)
	reports, total, err := s.db.ListReports(queryInt64(r, "repo_id"), from, to, limit, offset, pids, all)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": reports, "total": total})
}

func (s *Server) handleReportGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	report, err := s.db.GetReport(id)
	if err != nil {
		writeErr(w, 404, "报告不存在")
		return
	}
	if pids, all := s.visibleProjects(userFrom(r)); !s.db.ReportVisible(id, pids, all) {
		writeErr(w, 403, "无权访问该报告")
		return
	}
	issues, err := s.db.ListReportIssues(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	matcher := s.db.LoadMatcher(report.RepoID)
	for i := range issues {
		issues[i].Subsystem = matcher.Match(issues[i].FilePath)
	}
	writeJSON(w, 200, map[string]any{"report": report, "issues": issues})
}

// ---------- 问题项（§4.5） ----------

func (s *Server) handleIssueList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := pageParams(r)
	from, to := timeRange(r)
	u := userFrom(r)
	pids, all := s.visibleProjects(u)
	pids, all = scopeToProject(queryInt64(r, "project_id"), pids, all)
	// 这五个维度都是多选，前端用逗号分隔传（值都是枚举名或数字，不含逗号）。
	// 空串拆出空切片，等于「不筛这一项」。
	//
	// 界面上「一个都不勾」会传 __none__（前端常量 MS_NONE）：它在这里不做特殊处理，
	// 直接进 IN (...) 就天然匹配不到任何行。之所以不在后端认这个值，是因为它表达的
	// 就是「筛一个不存在的值」，语义上不需要后端特殊照顾。
	f := store.IssueFilter{
		ProjectIDs:  pids,
		AllProjects: all,
		RepoID:      queryInt64(r, "repo_id"),
		Statuses:    splitList(q.Get("status")),
		Severities:  splitList(q.Get("severity")),
		Categories:  splitList(q.Get("category")),
		Sources:     splitList(q.Get("source")),
		AssigneeIDs: splitIDs(q.Get("assignee_id")),
		Keyword:     q.Get("keyword"),
		From:        from,
		To:          to,
		Regression:  q.Get("regression") == "1",
		Limit:       limit,
		Offset:      offset,
	}
	issues, total, err := s.db.ListIssues(f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 规则按仓库取一次，别在循环里逐条查库
	repoIDs := map[int64]bool{}
	for i := range issues {
		repoIDs[issues[i].RepoID] = true
	}
	// 最多也就是仓库数条查询
	matchers := map[int64]*store.SubsystemMatcher{}
	for id := range repoIDs {
		matchers[id] = s.db.LoadMatcher(id)
	}
	for i := range issues {
		issues[i].Subsystem = store.MatchAny(matchers, issues[i].RepoID, issues[i].FilePath)
	}
	writeJSON(w, 200, map[string]any{"items": issues, "total": total})
}

func (s *Server) handleIssueGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	iss, err := s.db.GetIssue(id)
	if err != nil {
		writeErr(w, 404, "问题不存在")
		return
	}
	if pids, all := s.visibleProjects(userFrom(r)); !s.db.IssueVisible(id, pids, all) {
		writeErr(w, 403, "无权访问该问题")
		return
	}
	iss.Subsystem = s.db.SubsystemOf(iss.RepoID, iss.FilePath)
	comments, _ := s.db.ListComments(id)
	events, _ := s.db.ListIssueEvents(id)
	writeJSON(w, 200, map[string]any{"issue": iss, "comments": comments, "events": events})
}

func (s *Server) handleIssueStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var in struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if _, ok := store.StatusNames[in.Status]; !ok {
		writeErr(w, 400, "状态非法")
		return
	}
	u := userFrom(r)
	if err := s.db.SetIssueStatus(id, in.Status, u.ID, u.Name, in.Note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleIssueAssign(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var in struct {
		AssigneeID int64 `json:"assignee_id"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	u := userFrom(r)
	if err := s.db.AssignIssue(id, in.AssigneeID, u.ID, u.Name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 指派后飞书通知被指派人（§4.5）。同步发：通知模块自带 8s 超时，
	// 丢进 goroutine 反而会因为请求结束时 ctx 被取消而发不出去
	if in.AssigneeID > 0 {
		if at := s.db.FeishuID(in.AssigneeID); at != "" {
			if iss, err := s.db.GetIssue(id); err == nil {
				if err := s.feishu.HighIssue(r.Context(), s.q.WebhookFor(iss.RepoID), "指派提醒", iss, at); err != nil {
					log.Printf("[notify] 指派通知发送失败 issue#%d: %v", id, err)
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleCommentAdd(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Content) == "" {
		writeErr(w, 400, "评论内容不能为空")
		return
	}
	u := userFrom(r)
	cid, err := s.db.AddComment(id, u.ID, u.Name, strings.TrimSpace(in.Content))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": cid})
}

// handleIssueBatch 支持批量标记误报与批量指派（§4.5）。
func (s *Server) handleIssueBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs   []int64 `json:"ids"`
		Op    string  `json:"op"`
		Value int64   `json:"value"`
	}
	if err := readJSON(r, &in); err != nil || len(in.IDs) == 0 {
		writeErr(w, 400, "请选择至少一个问题项")
		return
	}
	u := userFrom(r)
	if err := s.db.BatchIssueOp(in.IDs, in.Op, in.Value, u.ID, u.Name); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.db.AppendAudit(u.ID, u.Name, "issue.batch", "issue", strconv.Itoa(len(in.IDs)), in.Op)
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(in.IDs)})
}

// ---------- 用户 / 身份归并 / 子系统 / 审计 ----------

func (s *Server) handleUserList(w http.ResponseWriter, r *http.Request) {
	users, err := s.db.ListUsers()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, users)
}

func (s *Server) handleUserRole(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := readJSON(r, &in); err != nil || (in.Role != "admin" && in.Role != "user") {
		writeErr(w, 400, "角色非法")
		return
	}
	if id == userFrom(r).ID {
		writeErr(w, 400, "不能改自己的角色")
		return
	}
	if err := s.db.SetUserRole(id, in.Role); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "user.role", "user", strconv.FormatInt(id, 10), in.Role)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleIdentityList(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.ListIdentities()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) handleIdentityUpsert(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GitName string `json:"git_name"`
		UserID  int64  `json:"user_id"`
		IsBot   bool   `json:"is_bot"`
		Note    string `json:"note"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.GitName) == "" {
		writeErr(w, 400, "git 身份不能为空")
		return
	}
	if err := s.db.UpsertIdentity(strings.TrimSpace(in.GitName), in.UserID, in.IsBot, in.Note); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "identity.upsert", "identity", in.GitName, in.Note)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleIdentityDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.db.DeleteIdentity(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 子系统归类规则（按仓库） ----------
//
// 规则按仓库隔离：不同项目的目录结构和业务名词完全不同，全局一张表
// 等于把某一个项目的分类强加给所有仓库。

func (s *Server) handleSubsystemList(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepoForRead(w, r)
	if !ok {
		return
	}
	list, err := s.db.ListSubsystems(repo.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) handleSubsystemPut(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepoForWrite(w, r)
	if !ok {
		return
	}
	repoID := repo.ID
	var in []store.Subsystem
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if err := s.db.ReplaceSubsystems(repoID, in); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "subsystem.update", "repo", strconv.FormatInt(repoID, 10),
		strconv.Itoa(len(in))+" 条规则")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleSubsystemScan 扫一遍仓库工作副本的目录结构，给出候选规则。
// 首次接一个新仓库时照着目录树手打一遍很烦，但目录名离「战斗系统」这种业务名词
// 还差一层人工判断，所以这里只给候选，名称留空让人自己填。
func (s *Server) handleSubsystemScan(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepoForRead(w, r)
	if !ok {
		return
	}
	dir := filepath.Join(s.cfg.WorkDir, fmt.Sprintf("repo-%d", repo.ID))
	if _, err := os.Stat(dir); err != nil {
		writeErr(w, 409, "仓库还没有本地副本，先触发一次审核任务把它拉下来")
		return
	}
	exclude := append([]string{".git"}, repo.ExcludeGlobs...)
	cands, err := gocx.ScanDirs(dir, gocx.LangExts(repo.Language), exclude)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"candidates": cands, "language": repo.Language})
}

// handleRepoSpecDocs 列出仓库工作副本里可以挂作审核规范文档的文件，
// 让人在仓库编辑页点选而不是照着目录树手打路径。
//
// 和子系统扫描一样要求本地副本已存在：没有副本就无从列路径，与其猜不如让人先拉一次。
func (s *Server) handleRepoSpecDocs(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepoForRead(w, r)
	if !ok {
		return
	}
	dir := filepath.Join(s.cfg.WorkDir, fmt.Sprintf("repo-%d", repo.ID))
	if _, err := os.Stat(dir); err != nil {
		writeErr(w, 409, "仓库还没有本地副本，先触发一次审核任务把它拉下来")
		return
	}
	items, err := gocx.ScanDocs(dir)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// ---------- 通知配置（§4.7） ----------

// notifyView 把三层来源摊开给界面看，省得「配了却没生效」靠猜。
func (s *Server) notifyView() (map[string]any, error) {
	c, err := s.db.NotifyConfig()
	if err != nil {
		return nil, err
	}
	source := "未配置"
	switch {
	case c.HasWebhook:
		source = "界面配置"
	case s.cfg.FeishuWebhook != "":
		source = "配置文件"
	}
	return map[string]any{
		"webhook_mask":   c.WebhookMask,
		"has_webhook":    c.HasWebhook,
		"summary_cron":   c.SummaryCron,
		"effective_cron": s.q.SummaryCron(),
		"source":         source,
		// 配置文件里有没有一份兜底值，用来解释「这里不填会用到什么」
		"file_configured": s.cfg.FeishuWebhook != "",
		"file_mask":       maskHook(s.cfg.FeishuWebhook),
		"repo_overrides":  s.repoWebhookCount(),
	}, nil
}

func (s *Server) repoWebhookCount() int {
	repos, err := s.db.ListAllRepos()
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range repos {
		if r.HasWebhook {
			n++
		}
	}
	return n
}

func (s *Server) handleNotifyGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.notifyView()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) handleNotifyPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Webhook     *string `json:"webhook"`
		SummaryCron *string `json:"summary_cron"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	cron := ""
	if cur, err := s.db.NotifyConfig(); err == nil {
		cron = cur.SummaryCron
	}
	if in.SummaryCron != nil {
		cron = strings.TrimSpace(*in.SummaryCron)
	}
	// webhook 传了非空值才改；单独保存 cron 时别把已存的 webhook 冲掉
	hook := in.Webhook
	if hook != nil && *hook == "" {
		hook = nil // 留空 = 不修改，清空请走单独的入口
	}
	if err := s.db.UpdateNotifyConfig(hook, cron); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "notify.update", "notify_config", "1", "汇总时间="+cron)
	_ = s.sched.Reload() // cron 表达式可能变了
	v, _ := s.notifyView()
	writeJSON(w, 200, v)
}

// handleNotifyClear 单独清空全局 webhook。跟保存分开，是因为「留空=不修改」和
// 「留空=清空」在同一个字段上没法同时表达。
func (s *Server) handleNotifyClear(w http.ResponseWriter, r *http.Request) {
	empty := ""
	cron := ""
	if c, err := s.db.NotifyConfig(); err == nil {
		cron = c.SummaryCron
	}
	if err := s.db.UpdateNotifyConfig(&empty, cron); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "notify.clear", "notify_config", "1", "清空全局 webhook")
	v, _ := s.notifyView()
	writeJSON(w, 200, v)
}

// handleNotifyTest 往生效的地址发一条测试消息。
// 没有这个按钮的话，「配好了但群里没动静」只能靠翻服务端日志——而通知失败恰恰是静默的。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Webhook string `json:"webhook"` // 可以不保存先试
		RepoID  int64  `json:"repo_id"` // 输入框为空时按这个仓库解析已存的地址
	}
	_ = readJSON(r, &in)

	// 优先测输入框里的内容；留空才往回落。
	// 仓库场景下必须先看仓库自己存的那层——直接掉到全局会让人以为仓库的配好了，
	// 实际测的是另一个群，等真有高危问题发错地方才发现。
	hook := in.Webhook
	if hook == "" {
		hook = s.q.WebhookFor(in.RepoID)
	}
	if hook == "" {
		writeErr(w, 400, "还没有可用的 webhook：请填写，或在「设置 → 通知」配全局兜底")
		return
	}
	// 输入框为空且仓库自己没存 webhook 时，测的其实是全局兜底那一层。
	// 要在响应里说清楚，别让人以为仓库自己配好了——等真有高危问题发错群才发现就晚了。
	viaGlobal := false
	if in.Webhook == "" && in.RepoID > 0 {
		if own, err := s.db.RepoWebhook(in.RepoID); err == nil && own == "" {
			viaGlobal = true
		}
	}

	ctx, cancel := timeoutCtx(r, 20*time.Second)
	defer cancel()
	msg := fmt.Sprintf("✅ [AIudge 连通性测试]\n如果你看到这条，说明通知链路是通的。\n后台: %s", s.cfg.BaseURL)
	if err := s.feishu.Send(ctx, hook, msg); err != nil {
		writeErr(w, 502, "发送失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "sent_to": maskHook(hook), "via_global": viaGlobal})
}

// maskHook 与 store 里的打码规则保持一致；这里只需处理一个字符串，不必来回捣腾 DB。
func maskHook(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "****"
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if n := len(segs); n > 0 && len(segs[n-1]) > 4 {
		segs[n-1] = "****" + segs[n-1][len(segs[n-1])-4:]
	} else if n > 0 {
		segs[n-1] = "****"
	}
	return u.Scheme + "://" + u.Host + "/" + strings.Join(segs, "/")
}

// ---------- 审核 prompt（§5：审核规则与 prompt 需可配置且带版本号） ----------

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request) {
	custom, err := s.db.SystemPrompt()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	effective := review.EffectivePrompt(custom)
	writeJSON(w, 200, map[string]any{
		"custom":       custom,
		"default":      review.DefaultSystemPrompt(),
		"effective":    effective,
		"version":      review.PromptVersion(effective),
		"customized":   custom != "",
		"placeholders": review.PromptPlaceholders,
	})
}

func (s *Server) handlePromptPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SystemPrompt string `json:"system_prompt"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if err := s.db.UpdateSystemPrompt(in.SystemPrompt); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	effective := review.EffectivePrompt(in.SystemPrompt)
	ver := review.PromptVersion(effective)
	mode := "自定义"
	if in.SystemPrompt == "" {
		mode = "恢复内置默认"
	}
	s.db.AppendAudit(u.ID, u.Name, "prompt.update", "review_config", "1", mode+" prompt="+ver)
	writeJSON(w, 200, map[string]any{"ok": true, "version": ver, "effective": effective})
}

// ---------- 维护 ----------

// handleResetPreview 清档前先给出现状，让确认对话框能说清「会删掉什么」。
func (s *Server) handleResetPreview(w http.ResponseWriter, r *http.Request) {
	counts, err := s.db.CountBusinessRows()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	writeJSON(w, 200, map[string]any{"counts": counts, "total": total})
}

// handleAdminReset 清空业务数据，保留仓库/模型/用户等配置。仅管理员，且要求显式确认。
func (s *Server) handleAdminReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm   string `json:"confirm"`
		Worktrees bool   `json:"worktrees"` // 是否连本地仓库副本一起删
	}
	_ = readJSON(r, &in)
	if in.Confirm != "清空" {
		writeErr(w, 400, `确认字段不对：需要 {"confirm":"清空"}`)
		return
	}

	// 先停队列再删：正在跑的审核任务会在删完之后把报告写回来，
	// 于是「清空」之后还剩下最后那一条，看起来像没清干净
	ctx, cancel := timeoutCtx(r, 60*time.Second)
	defer cancel()
	s.q.Drain(ctx)

	counts, err := s.db.ResetData()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	removed := 0
	if in.Worktrees {
		matches, _ := filepath.Glob(filepath.Join(s.cfg.WorkDir, "repo-*"))
		for _, m := range matches {
			if err := os.RemoveAll(m); err == nil {
				removed++
			}
		}
	}

	// 审计写在这里：审计表刚被自己清掉了，写在此之前等于白写
	u := userFrom(r)
	total := 0
	for _, n := range counts {
		total += n
	}
	detail := fmt.Sprintf("清空业务数据 %d 行", total)
	if in.Worktrees {
		detail += fmt.Sprintf("，删除本地仓库副本 %d 个", removed)
	}
	s.db.AppendAudit(u.ID, u.Name, "admin.reset", "system", "", detail)

	writeJSON(w, 200, map[string]any{"counts": counts, "worktrees_removed": removed})
}

func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	logs, total, err := s.db.ListAudit(limit, offset)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": logs, "total": total})
}
