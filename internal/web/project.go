package web

import (
	"net/http"
	"strconv"
	"strings"

	"aiudge/internal/store"
)

// ---------- 项目管理 ----------
//
// 项目是权限的锚点：仓库归属项目，用户在项目内有角色。
// 建项目对任何登录用户开放，建完自动成为该项目的项目管理员；
// 改/删则要项目级权限——它不是权限体系本身，改不动别人的项目就够了。

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	projects, err := s.db.ListProjects()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	pids, all := s.visibleProjects(u)
	visible := map[int64]bool{}
	for _, id := range pids {
		visible[id] = true
	}

	type projectView struct {
		store.Project
		Role     string `json:"role"` // 我在这个项目里的角色
		RoleName string `json:"role_name"`
		CanAdmin bool   `json:"can_admin"` // 能不能改配置
	}
	out := make([]projectView, 0, len(projects))
	for _, p := range projects {
		if !all && !visible[p.ID] {
			continue // 不是成员的项目根本不该出现在列表里
		}
		role, roleName := "admin", "全局管理员"
		if !all {
			role = s.db.ProjectRoleOf(u.ID, p.ID)
			roleName = store.ProjectRoleNames[role]
		}
		out = append(out, projectView{
			Project: p, Role: role, RoleName: roleName,
			CanAdmin: s.db.CanAdminProject(u, p.ID),
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeErr(w, 400, "项目名称必填")
		return
	}
	id, err := s.db.CreateProject(strings.TrimSpace(in.Name), in.Description)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	// 创建者默认就是本项目的项目管理员。全局管理员本来就有全量权限，
	// 但在成员表里留一条记录，项目详情页才看得出「这是谁建的、谁管」
	if err := s.db.UpsertProjectMember(id, u.ID, store.RoleProjectAdmin); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.db.AppendAudit(u.ID, u.Name, "project.create", "project", strconv.FormatInt(id, 10), in.Name)
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.db.CanAdminProject(userFrom(r), id) {
		writeErr(w, 403, "需要该项目管理员权限")
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeErr(w, 400, "项目名称必填")
		return
	}
	if err := s.db.UpdateProject(id, strings.TrimSpace(in.Name), in.Description, in.Enabled); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "project.update", "project", strconv.FormatInt(id, 10), in.Name)
	_ = s.sched.Reload() // 项目禁用会影响其下仓库的定时任务
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.db.CanAdminProject(userFrom(r), id) {
		writeErr(w, 403, "需要该项目管理员权限")
		return
	}
	if err := s.db.DeleteProject(id); err != nil {
		// 项目下还有仓库时拒绝，把「先挪走再删」这个顺序明确回给界面
		writeErr(w, 409, err.Error())
		return
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "project.delete", "project", strconv.FormatInt(id, 10), "")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 项目成员 ----------

func (s *Server) handleMemberList(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.db.CanViewProject(userFrom(r), id) {
		writeErr(w, 403, "无权访问该项目")
		return
	}
	list, err := s.db.ListProjectMembers(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, list)
}

// handleMemberCandidates 列出还能加进本项目的人，给项目管理员挑。
// 不是全局用户表——那个是管理员视角的信息。
func (s *Server) handleMemberCandidates(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.db.CanAdminProject(userFrom(r), id) {
		writeErr(w, 403, "需要该项目管理员权限")
		return
	}
	list, err := s.db.ListAddableUsers(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) handleMemberUpsert(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.db.CanAdminProject(userFrom(r), id) {
		writeErr(w, 403, "需要该项目管理员权限")
		return
	}
	var in struct {
		UserID int64  `json:"user_id"`
		Role   string `json:"role"`
		// 权限是**可选**字段：角色切换的请求不带它，就不该把人家已有的权限抹掉。
		// 指针为 nil 表示「不改」，空数组表示「全部清空」。
		Permissions *[]string `json:"permissions"`
	}
	if err := readJSON(r, &in); err != nil || in.UserID == 0 {
		writeErr(w, 400, "请选择用户")
		return
	}
	if in.Role == "" {
		in.Role = store.RoleMember
	}
	if err := s.db.UpsertProjectMember(id, in.UserID, in.Role); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	note := "设置 user#" + strconv.FormatInt(in.UserID, 10) + " 为 " + store.ProjectRoleNames[in.Role]
	if in.Permissions != nil {
		if err := s.db.SetMemberPerms(id, in.UserID, *in.Permissions); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		note += "，权限 " + permNames(*in.Permissions)
	}
	u := userFrom(r)
	s.db.AppendAudit(u.ID, u.Name, "project.member", "project", strconv.FormatInt(id, 10), note)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handlePermissionList 返回可分配的权限类型清单（§3）。
// 界面按它渲染成员管理里的勾选项，以后加权限类型不用改前端。
func (s *Server) handlePermissionList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, store.PermissionList)
}

// permNames 把权限 key 翻成名字，供审计用；没有权限时给一句明确的话而不是空串。
func permNames(keys []string) string {
	if len(keys) == 0 {
		return "清空"
	}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, p := range store.PermissionList {
			if p.Key == k {
				names = append(names, p.Name)
			}
		}
	}
	return strings.Join(names, "、")
}

func (s *Server) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	userID, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "缺少 user_id")
		return
	}
	me := userFrom(r)
	if !s.db.CanAdminProject(me, id) {
		writeErr(w, 403, "需要该项目管理员权限")
		return
	}
	if userID == me.ID && me.Role != "admin" {
		// 项目管理员把自己踢出去就再也进不来了，还要找全局管理员捞人
		writeErr(w, 400, "不能把自己移出项目，请让全局管理员操作")
		return
	}
	if err := s.db.RemoveProjectMember(id, userID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.db.AppendAudit(me.ID, me.Name, "project.member.remove", "project", strconv.FormatInt(id, 10),
		"移除 user#"+strconv.FormatInt(userID, 10))
	writeJSON(w, 200, map[string]any{"ok": true})
}
