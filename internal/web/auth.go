package web

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiudge/internal/store"
)

// ---------- 账号密码登录 ----------
//
// 账号密码只给内置管理员兜底：飞书没配、或者飞书挂了的时候还能进后台。
// 普通用户一律走飞书——§4.7 的 @ 通知依赖飞书 open_id，走密码的人收不到。

// loginThrottle 按用户名记失败次数，失败越多等得越久。
// 密码是写死在代码里的默认值，不限速就等于把爆破成本降到零。
type loginThrottle struct {
	mu    sync.Mutex
	fails map[string]*failState
}

type failState struct {
	count int
	until time.Time
}

var throttle = &loginThrottle{fails: map[string]*failState{}}

const (
	throttleFreeTries = 3
	throttleMaxWait   = 30 * time.Second
)

// blocked 返回还需要等待多久；0 表示可以尝试。
func (t *loginThrottle) blocked(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.fails[key]
	if !ok {
		return 0
	}
	if d := time.Until(st.until); d > 0 {
		return d
	}
	return 0
}

func (t *loginThrottle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.fails[key]
	if !ok {
		st = &failState{}
		t.fails[key] = st
	}
	st.count++
	if st.count <= throttleFreeTries {
		return
	}
	// 超过免费次数后翻倍等待：1s, 2s, 4s… 上限 30 秒
	wait := time.Duration(1<<uint(min(st.count-throttleFreeTries, 5))) * time.Second
	if wait > throttleMaxWait {
		wait = throttleMaxWait
	}
	st.until = time.Now().Add(wait)
}

func (t *loginThrottle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, key)
}

// handlePasswordLogin 处理账号密码登录。
func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || in.Password == "" {
		writeErr(w, 400, "请输入账号和密码")
		return
	}

	if wait := throttle.blocked(in.Username); wait > 0 {
		writeErr(w, 429, "失败次数过多，请 "+wait.Round(time.Second).String()+" 后重试")
		return
	}

	u, err := s.db.UserByUsername(in.Username)
	// 账号不存在与密码错误返回同一句话，不给爆破者任何区分信号
	deny := func() {
		throttle.fail(in.Username)
		writeErr(w, 401, "账号或密码错误")
	}
	if err != nil {
		// 账号不存在时也走一遍哈希校验，让耗时与账号存在时接近，
		// 否则响应时间差本身就是「这个账号存不存在」的旁路
		store.VerifyPassword(dummyHash, in.Password)
		deny()
		return
	}
	hash, err := s.db.PasswordHash(u.ID)
	if err != nil || !store.VerifyPassword(hash, in.Password) {
		deny()
		return
	}
	if u.Role != "admin" {
		// 只有全局管理员能用密码登录。普通用户走飞书，否则 @ 通知发不出去
		deny()
		return
	}

	throttle.reset(in.Username)
	if err := s.issueSession(w, u.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.db.TouchLogin(u.ID)
	s.db.AppendAudit(u.ID, u.Name, "user.login", "user", strconv.FormatInt(u.ID, 10), "账号密码")
	writeJSON(w, 200, u)
}

// dummyHash 用于「账号不存在」时也消耗一次哈希时间。
// 内容无所谓，只要是个合法格式、能让 VerifyPassword 真跑一遍 KDF。
const dummyHash = "pbkdf2-sha256$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// handleChangePassword 改密码。就是一个普通功能，改不改都不拦着用别的。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var in struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if len(in.New) < 8 {
		writeErr(w, 400, "新密码至少 8 位")
		return
	}
	if in.New == store.DefaultAdminPassword {
		writeErr(w, 400, "不能继续用默认密码")
		return
	}
	oldHash, err := s.db.PasswordHash(u.ID)
	if err != nil || !store.VerifyPassword(oldHash, in.Old) {
		writeErr(w, 401, "原密码不对")
		return
	}
	hash, err := store.HashPassword(in.New)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.db.SetPassword(u.ID, hash); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.db.AppendAudit(u.ID, u.Name, "user.password", "user", strconv.FormatInt(u.ID, 10), "修改密码")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleResetUserPassword 管理员把别人的密码重置回默认值。
func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	hash, err := store.HashPassword(store.DefaultAdminPassword)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.db.SetPassword(id, hash); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "user.password.reset", "user", strconv.FormatInt(id, 10), "重置为默认密码")
	writeJSON(w, 200, map[string]any{"ok": true, "password": store.DefaultAdminPassword})
}

var errNotProjectMember = errors.New("无权访问该项目")

// ---------- 权限 ----------

// canSeeAll 返回用户是否不受项目限制（全局管理员）。
func (s *Server) canSeeAll(u *store.User) bool { return u != nil && u.Role == "admin" }

// visibleProjects 拿一次可见项目范围。几乎所有列表接口开头都要调。
// visibleProjects 出错时返回「什么都看不到」，宁可空列表也不要越权。
func (s *Server) visibleProjects(u *store.User) ([]int64, bool) {
	ids, all, err := s.db.VisibleProjects(u)
	if err != nil {
		return []int64{}, false
	}
	return ids, all
}

// scopeToProject 把「可见项目范围」收窄成「只看这一个项目」，供页面上选中某个项目时用。
//
// 必须与可见集求交集，不能直接拿 project_id 当谓词：它是用户可控的查询参数，
// 直接塞进 repoVisibleCond 就等于「换个 id 看别人的项目」。不可见时返回空集，
// projectPredicate 会拼出 1=0，查不出任何行——空而不是全量，这是这里唯一安全的失败方向。
func scopeToProject(projectID int64, pids []int64, all bool) ([]int64, bool) {
	if projectID <= 0 {
		return pids, all
	}
	if all || slices.Contains(pids, projectID) {
		return []int64{projectID}, false
	}
	return nil, false
}

// canAdminRepo 判断用户能否改某个仓库的配置：全局管理员或该仓库所属项目的项目管理员。
func (s *Server) canAdminRepo(u *store.User, repo *store.Repository) bool {
	return s.db.CanAdminProject(u, repo.ProjectID)
}

// canViewRepo 判断用户能否看到某个仓库。
func (s *Server) canViewRepo(u *store.User, repo *store.Repository) bool {
	return s.db.CanViewProject(u, repo.ProjectID)
}

// canTriggerRepo 判断能否手动触发该仓库的审核（§3 项目级权限）。
//
// 「看得见仓库」不够：手动触发会直接消耗模型额度，所以要项目管理员、
// 或项目管理员在成员管理里单独授予了 manual_trigger 的成员。
func (s *Server) canTriggerRepo(u *store.User, repo *store.Repository) bool {
	if u == nil {
		return false
	}
	if s.canAdminRepo(u, repo) {
		return true
	}
	return s.db.MemberHasPerm(u.ID, repo.ProjectID, store.PermManualTrigger)
}

// loadRepoForWrite 取仓库并校验写权限，是仓库相关写接口的统一入口。
// 少了这一步，任何登录用户改个 id 就能动别人项目的配置。
func (s *Server) loadRepoForWrite(w http.ResponseWriter, r *http.Request) (*store.Repository, bool) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return nil, false
	}
	repo, err := s.db.GetRepo(id)
	if err != nil {
		writeErr(w, 404, "仓库不存在")
		return nil, false
	}
	if !s.canAdminRepo(userFrom(r), repo) {
		writeErr(w, 403, "需要该项目管理员权限")
		return nil, false
	}
	return repo, true
}

// loadRepoForRead 同上，但只要求项目成员。
func (s *Server) loadRepoForRead(w http.ResponseWriter, r *http.Request) (*store.Repository, bool) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return nil, false
	}
	repo, err := s.db.GetRepo(id)
	if err != nil {
		writeErr(w, 404, "仓库不存在")
		return nil, false
	}
	if !s.canViewRepo(userFrom(r), repo) {
		writeErr(w, 403, "无权访问该仓库")
		return nil, false
	}
	return repo, true
}
