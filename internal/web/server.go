// Package web 是 HTTP 层：REST API + 内嵌单页前端。
// 前端零构建（无 npm/Vite），首屏就是一次 HTML + 两个静态资源，直接满足 §5 的 <1s 首屏要求。
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aiudge/internal/auth"
	"aiudge/internal/config"
	"aiudge/internal/notify"
	"aiudge/internal/queue"
	"aiudge/internal/store"
	"aiudge/webui"
)

const sessionCookie = "aiudge_session"

type Server struct {
	cfg    *config.Config
	db     *store.DB
	q      *queue.Queue
	sched  *queue.Scheduler
	feishu *notify.Feishu
	auth   *auth.Feishu
}

func New(cfg *config.Config, db *store.DB, q *queue.Queue, sched *queue.Scheduler, feishu *notify.Feishu) *Server {
	return &Server{
		cfg: cfg, db: db, q: q, sched: sched, feishu: feishu,
		auth: auth.New(cfg.FeishuAppID, cfg.FeishuAppSecret, cfg.BaseURL+"/api/auth/callback", cfg.FeishuOAuthBase),
	}
}

// access 是路由的访问级别。路由注册与鉴权都从这张表读，
// 好处是「某个接口忘了加鉴权」在测试里会立刻暴露——
// 靠人肉 grep 检查 60 多条路由的包裹层，注定会漏。
type access int

const (
	accessPublic access = iota // 免登录：健康检查、登录流程、webhook
	accessUser                 // 需登录
	accessAdmin                // 需登录 + 全局管理员（内部还会校验项目级权限）
)

type route struct {
	method  string
	pattern string
	access  access
	h       http.HandlerFunc
}

// routes 是全部 API 路由。路径参数用 Go 1.22+ 的 {id} 语法。
func (s *Server) routes() []route {
	return []route{
		// 免登录
		{"GET", "/api/health", accessPublic, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"ok": true, "time": store.Now(), "ui": webui.Version()})
		}},
		{"GET", "/api/auth/config", accessPublic, s.handleAuthConfig},
		{"GET", "/api/auth/login", accessPublic, s.handleLogin},
		{"GET", "/api/auth/callback", accessPublic, s.handleCallback},
		{"POST", "/api/auth/dev-login", accessPublic, s.handleDevLogin},
		{"POST", "/api/auth/password", accessPublic, s.handlePasswordLogin},
		{"POST", "/api/auth/logout", accessPublic, s.handleLogout},
		{"POST", "/api/webhook/{id}", accessPublic, s.handleWebhook},

		{"GET", "/api/me", accessUser, s.handleMe},
		{"POST", "/api/auth/change-password", accessUser, s.handleChangePassword},

		{"GET", "/api/permissions", accessUser, s.handlePermissionList},
		{"GET", "/api/projects", accessUser, s.handleProjectList},
		{"GET", "/api/projects/{id}/members", accessUser, s.handleMemberList},
		{"GET", "/api/projects/{id}/candidates", accessUser, s.handleMemberCandidates},
		{"POST", "/api/projects/{id}/members", accessUser, s.handleMemberUpsert},
		{"DELETE", "/api/projects/{id}/members", accessUser, s.handleMemberRemove},

		{"GET", "/api/repos", accessUser, s.handleRepoList},
		{"POST", "/api/repos", accessUser, s.handleRepoCreate},
		{"GET", "/api/repos/{id}", accessUser, s.handleRepoGet},
		{"PUT", "/api/repos/{id}", accessUser, s.handleRepoUpdate},
		{"DELETE", "/api/repos/{id}", accessUser, s.handleRepoDelete},
		{"POST", "/api/repos/{id}/trigger", accessUser, s.handleRepoTrigger},
		{"GET", "/api/repos/{id}/commits", accessUser, s.handleRepoCommits},
		{"GET", "/api/repos/{id}/subsystems", accessUser, s.handleSubsystemList},
		{"PUT", "/api/repos/{id}/subsystems", accessUser, s.handleSubsystemPut},
		{"POST", "/api/repos/{id}/subsystems/scan", accessUser, s.handleSubsystemScan},
		{"GET", "/api/repos/{id}/spec-docs", accessUser, s.handleRepoSpecDocs},

		{"GET", "/api/tasks", accessUser, s.handleTaskList},
		{"GET", "/api/tasks/{id}", accessUser, s.handleTaskGet},
		{"GET", "/api/tasks/{id}/logs", accessUser, s.handleTaskLogs},
		{"POST", "/api/tasks/{id}/retry", accessUser, s.handleTaskRetry},

		{"GET", "/api/reports", accessUser, s.handleReportList},
		{"GET", "/api/reports/{id}", accessUser, s.handleReportGet},

		{"GET", "/api/issues", accessUser, s.handleIssueList},
		{"POST", "/api/issues/batch", accessUser, s.handleIssueBatch},
		{"GET", "/api/issues/{id}", accessUser, s.handleIssueGet},
		{"POST", "/api/issues/{id}/status", accessUser, s.handleIssueStatus},
		{"POST", "/api/issues/{id}/assign", accessUser, s.handleIssueAssign},
		{"POST", "/api/issues/{id}/comments", accessUser, s.handleCommentAdd},

		{"GET", "/api/stats/overview", accessUser, s.handleStatsOverview},
		{"GET", "/api/stats/trend", accessUser, s.handleStatsTrend},
		{"GET", "/api/stats/distribution", accessUser, s.handleStatsDistribution},
		{"GET", "/api/stats/assignees", accessUser, s.handleStatsAssignees},
		{"GET", "/api/stats/versions", accessUser, s.handleStatsVersions},
		{"GET", "/api/stats/export", accessUser, s.handleStatsExport},

		{"GET", "/api/users", accessAdmin, s.handleUserList},
		{"GET", "/api/identities", accessAdmin, s.handleIdentityList},
		{"GET", "/api/notify-config", accessAdmin, s.handleNotifyGet},
		{"GET", "/api/prompt-config", accessAdmin, s.handlePromptGet},

		// 项目增删改：任何登录用户都能建项目（建完自动成为该项目管理员），
		// 但改/删要项目级权限——那层校验在 handler 里，路由表这层只判登录。
		{"POST", "/api/projects", accessUser, s.handleProjectCreate},
		{"PUT", "/api/projects/{id}", accessUser, s.handleProjectUpdate},
		{"DELETE", "/api/projects/{id}", accessUser, s.handleProjectDelete},

		// 全局管理员专属：全局配置、用户与权限
		{"PUT", "/api/users/{id}/role", accessAdmin, s.handleUserRole},
		{"POST", "/api/users/{id}/reset-password", accessAdmin, s.handleResetUserPassword},
		{"POST", "/api/identities", accessAdmin, s.handleIdentityUpsert},
		{"DELETE", "/api/identities/{id}", accessAdmin, s.handleIdentityDelete},
		{"GET", "/api/audit", accessAdmin, s.handleAuditList},
		{"PUT", "/api/model-config", accessAdmin, s.handleModelConfigPut},
		{"POST", "/api/model-config/refresh", accessAdmin, s.handleModelRefresh},
		{"PUT", "/api/notify-config", accessAdmin, s.handleNotifyPut},
		{"POST", "/api/notify-config/clear", accessAdmin, s.handleNotifyClear},
		{"POST", "/api/notify-config/test", accessAdmin, s.handleNotifyTest},
		{"PUT", "/api/prompt-config", accessAdmin, s.handlePromptPut},
		{"GET", "/api/admin/reset-preview", accessAdmin, s.handleResetPreview},
		{"POST", "/api/admin/reset", accessAdmin, s.handleAdminReset},

		// 模型配置读取：任何登录用户都能看（要用它显示当前模型），但看不到密钥明文
		{"GET", "/api/model-config", accessAdmin, s.handleModelConfigGet},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		h := rt.h
		switch rt.access {
		case accessAdmin:
			h = s.admin(h) // admin 自带登录校验
		case accessUser:
			h = s.authed(h)
		case accessPublic:
			// 不加任何校验
		}
		mux.Handle(rt.method+" "+rt.pattern, h)
	}
	mux.Handle("/", webui.Handler())
	return s.recoverPanic(s.logRequests(mux))
}

// ---------- 中间件 ----------

type ctxKey string

const userKey ctxKey = "user"

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if e := recover(); e != nil {
				log.Printf("[panic] %s %s: %v", r.Method, r.URL.Path, e)
				writeErr(w, 500, "服务内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/health" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// authed/admin 返回 http.HandlerFunc（而不是 http.Handler），这样它们能互相套。
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			writeErr(w, 401, "未登录")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// admin 自带登录校验：它自己包了 authed，所以单独用也不会把接口裸奔出去。
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r); u.Role != "admin" {
			writeErr(w, 403, "需要全局管理员权限")
			return
		}
		next(w, r)
	})
}

func (s *Server) currentUser(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	u, err := s.db.SessionUser(c.Value)
	if err != nil {
		return nil
	}
	return u
}

func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

// ---------- 登录 ----------

func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	feishuOn := s.auth.Enabled()
	devOn := !feishuOn
	writeJSON(w, 200, map[string]any{
		"feishu_enabled": feishuOn,
		// 飞书未配置时放行开发登录，否则本地跑起来连不进去。生产必须配飞书
		"dev_login": devOn,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Enabled() {
		writeErr(w, 400, "飞书登录未配置，请使用开发登录")
		return
	}
	state, err := randomToken()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "aiudge_state", Value: state, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	http.Redirect(w, r, s.auth.AuthorizeURL(state), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/#/login?error="+r.URL.Query().Get("error"), http.StatusFound)
		return
	}
	c, err := r.Cookie("aiudge_state")
	if err != nil || c.Value == "" || c.Value != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/#/login?error=state_mismatch", http.StatusFound)
		return
	}
	profile, err := s.auth.Exchange(r.Context(), code)
	if err != nil {
		log.Printf("[auth] 飞书登录失败: %v", err)
		http.Redirect(w, r, "/#/login?error=exchange_failed", http.StatusFound)
		return
	}
	u, err := s.db.UpsertUser(profile.OpenID, profile.Name, profile.Avatar)
	if err != nil {
		http.Redirect(w, r, "/#/login?error=db", http.StatusFound)
		return
	}
	if err := s.issueSession(w, u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.db.AppendAudit(u.ID, u.Name, "user.login", "user", strconv.FormatInt(u.ID, 10), "飞书扫码")
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleDevLogin 只在飞书未配置时开放，避免成了绕过登录的后门。
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if s.auth.Enabled() {
		writeErr(w, 403, "已启用飞书登录，开发登录不可用")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeErr(w, 400, "请填写姓名")
		return
	}
	u, err := s.db.UpsertUser("dev:"+strings.TrimSpace(in.Name), strings.TrimSpace(in.Name), "")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 首位登录者自动成为管理员，否则新部署没有人能进配置页
	if n, _ := s.db.CountAdmins(); n == 0 {
		_ = s.db.SetUserRole(u.ID, "admin")
		u.Role = "admin"
	}
	if err := s.issueSession(w, u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, u)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.db.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, userFrom(r))
}

func (s *Server) issueSession(w http.ResponseWriter, userID int64) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(30 * 24 * time.Hour)
	if err := s.db.CreateSession(token, userID, expires.UTC().Format(store.TimeFmt)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Expires: expires,
	})
	return nil
}

// ---------- webhook ----------

// handleWebhook 接收 GitLab push 事件（§4.1 webhook 为主触发方式）。
// URL 形如 /api/webhook/{repoID}，GitLab 侧的 Secret token 需填成该仓库的访问凭据。
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "仓库 ID 非法")
		return
	}
	repo, err := s.db.GetRepo(id)
	if err != nil || !repo.Enabled {
		writeErr(w, 404, "仓库不存在或已禁用")
		return
	}
	if !repo.TriggerWebhook {
		writeErr(w, 403, "该仓库未启用 webhook 触发")
		return
	}
	if !s.webhookAuthorized(r, repo) {
		writeErr(w, 401, "webhook token 校验失败")
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var hook struct {
		Before string `json:"before"`
		After  string `json:"after"`
		Ref    string `json:"ref"`
	}
	_ = json.Unmarshal(body, &hook)

	task := &store.Task{
		RepoID:      repo.ID,
		TriggerType: "webhook",
		CommitFrom:  hook.Before,
		CommitTo:    hook.After,
		Status:      "queued",
		ModelName:   repo.DefaultModel,
	}
	if err := s.db.CreateTask(task); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.q.Submit(task.ID)
	log.Printf("[webhook] 仓库=%s ref=%s %s..%s 任务=#%d", repo.Name, hook.Ref, short(hook.Before), short(hook.After), task.ID)
	writeJSON(w, 200, map[string]any{"task_id": task.ID})
}

func (s *Server) webhookAuthorized(r *http.Request, repo *store.Repository) bool {
	credential, err := s.db.RepoCredential(repo.ID)
	if err != nil || credential == "" {
		return true // 仓库没配凭据就没得校验，只依赖 URL 里的仓库 ID
	}
	got := r.Header.Get("X-Gitlab-Token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(credential)) == 1
}

// ---------- 小工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, errors.New("ID 非法")
	}
	return id, nil
}

func queryInt(r *http.Request, key string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil {
		return v
	}
	return def
}

func queryInt64(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}

// splitList / splitIDs 解析多选筛选参数（界面上的筛选项可以勾好几个）。
// 空串给空切片，语义是「这一项不筛」而不是「筛空值」。
func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	out := []string{}
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func splitIDs(v string) []int64 {
	out := []int64{}
	for _, s := range splitList(v) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}
