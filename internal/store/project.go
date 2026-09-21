package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// 项目内角色。全局管理员不在这套里——它由 users.role='admin' 单独判定，
// 并且对所有项目都有最高权限。
const (
	RoleProjectAdmin = "project_admin"
	RoleMember       = "member"
)

var ProjectRoleNames = map[string]string{
	RoleProjectAdmin: "项目管理员",
	RoleMember:       "成员",
}

type Project struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`

	RepoCount   int `json:"repo_count"`
	MemberCount int `json:"member_count"`
}

// Permission 是一种可分配的项目级权限（§3 角色与权限）。
//
// 清单写死在代码里而不是建一张表：每加一种权限都得在代码里实现对应的校验，
// 往表里插一行不会让权限生效。表只存「谁有哪几个 key」。
type Permission struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// PermManualTrigger 允许手动触发本项目的审核任务（会消耗模型额度，所以按人授权）。
const PermManualTrigger = "manual_trigger"

var PermissionList = []Permission{{PermManualTrigger, "手动触发审核"}}

func ValidPermission(key string) bool {
	for _, p := range PermissionList {
		if p.Key == key {
			return true
		}
	}
	return false
}

type ProjectMember struct {
	UserID   int64    `json:"user_id"`
	Name     string   `json:"name"`
	Avatar   string   `json:"avatar"`
	Role     string   `json:"role"`
	RoleName string   `json:"role_name"`
	Perms    []string `json:"perms"` // 角色之外的额外授权，空数组表示没有
	JoinedAt string   `json:"joined_at"`

	// 指派时给人用的别名：Username 是登录名，Aliases 是身份归并里绑到这个人的
	// git 身份（dengquanjie1 这类）。指派人往往只记得住一个人「平时被怎么叫」，
	// 中文名未必想得起来——这几个字段就是给搜索匹配用的（§4.5）。
	Username string   `json:"username"`
	Aliases  []string `json:"aliases"`
}

const projectCols = `p.id,p.name,p.description,p.enabled,p.created_at,p.updated_at,
	(SELECT COUNT(*) FROM repositories r WHERE r.project_id=p.id),
	(SELECT COUNT(*) FROM project_members m WHERE m.project_id=p.id)`

func scanProject(s interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var en int
	err := s.Scan(&p.ID, &p.Name, &p.Description, &en, &p.CreatedAt, &p.UpdatedAt, &p.RepoCount, &p.MemberCount)
	if err != nil {
		return nil, err
	}
	p.Enabled = en == 1
	return &p, nil
}

func (d *DB) ListProjects() ([]Project, error) {
	rows, err := d.Query(`SELECT ` + projectCols + ` FROM projects p ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (d *DB) GetProject(id int64) (*Project, error) {
	return scanProject(d.QueryRow(`SELECT `+projectCols+` FROM projects p WHERE p.id=?`, id))
}

func (d *DB) CreateProject(name, description string) (int64, error) {
	now := Now()
	res, err := d.Exec(`INSERT INTO projects(name,description,enabled,created_at,updated_at) VALUES(?,?,1,?,?)`,
		name, description, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateProject(id int64, name, description string, enabled bool) error {
	_, err := d.Exec(`UPDATE projects SET name=?,description=?,enabled=?,updated_at=? WHERE id=?`,
		name, description, b2i(enabled), Now(), id)
	return err
}

func (d *DB) DeleteProject(id int64) error {
	var repos int
	if err := d.QueryRow(`SELECT COUNT(*) FROM repositories WHERE project_id=?`, id).Scan(&repos); err != nil {
		return err
	}
	if repos > 0 {
		// 仓库的问题、报告都挂在仓库上，级联删掉等于静默销毁历史数据，
		// 让人先把仓库挪走或删掉更安全
		return fmt.Errorf("项目下还有 %d 个仓库，请先转移或删除它们", repos)
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM project_members WHERE project_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM projects WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- 成员 ----------

func (d *DB) ListProjectMembers(projectID int64) ([]ProjectMember, error) {
	// 别名用子查询一次带出来：成员列表是个位数的人，不值得为它再发一条查询
	// （何况游标还开着的时候不能发第二条，见 deadlock_test.go）
	rows, err := d.Query(`SELECT m.user_id,COALESCE(u.name,''),COALESCE(u.avatar,''),m.role,m.perms,m.created_at,
			COALESCE(u.username,''),
			COALESCE((SELECT GROUP_CONCAT(ci.git_name,' ') FROM committer_identities ci
				WHERE ci.user_id=m.user_id AND ci.is_bot=0),'')
		FROM project_members m LEFT JOIN users u ON u.id=m.user_id
		WHERE m.project_id=? ORDER BY m.role,m.user_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectMember{}
	for rows.Next() {
		var m ProjectMember
		var perms, aliases string
		if err := rows.Scan(&m.UserID, &m.Name, &m.Avatar, &m.Role, &perms, &m.JoinedAt,
			&m.Username, &aliases); err != nil {
			return nil, err
		}
		m.RoleName = ProjectRoleNames[m.Role]
		m.Perms = []string{}
		_ = json.Unmarshal([]byte(perms), &m.Perms)
		m.Aliases = strings.Fields(aliases)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMemberPerms 覆盖式设置某个成员的项目级权限（传空切片即清空）。
// 与角色分开设：界面上的权限勾选不该顺带把角色也改写一遍。
// 调用者要保证该成员已在项目里，否则影响 0 行、静默什么都不做——所以这里明确报错。
func (d *DB) SetMemberPerms(projectID, userID int64, keys []string) error {
	for _, k := range keys {
		if !ValidPermission(k) {
			return fmt.Errorf("未知权限 %q", k)
		}
	}
	j, _ := json.Marshal(orEmpty(keys))
	res, err := d.Exec(`UPDATE project_members SET perms=? WHERE project_id=? AND user_id=?`,
		string(j), projectID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("该用户不是项目成员")
	}
	return nil
}

// MemberHasPerm 判断成员是否被授予了某个项目级权限。不是成员一律 false——
// 这是权限判定，失败方向只能是「没有」。
func (d *DB) MemberHasPerm(userID, projectID int64, key string) bool {
	var perms string
	if err := d.QueryRow(`SELECT perms FROM project_members WHERE project_id=? AND user_id=?`,
		projectID, userID).Scan(&perms); err != nil {
		return false
	}
	var keys []string
	if err := json.Unmarshal([]byte(perms), &keys); err != nil {
		return false
	}
	return slices.Contains(keys, key)
}

func (d *DB) UpsertProjectMember(projectID, userID int64, role string) error {
	if role != RoleProjectAdmin && role != RoleMember {
		return fmt.Errorf("未知项目角色 %q", role)
	}
	_, err := d.Exec(`INSERT INTO project_members(project_id,user_id,role,created_at) VALUES(?,?,?,?)
		ON CONFLICT(project_id,user_id) DO UPDATE SET role=excluded.role`,
		projectID, userID, role, Now())
	return err
}

func (d *DB) RemoveProjectMember(projectID, userID int64) error {
	_, err := d.Exec(`DELETE FROM project_members WHERE project_id=? AND user_id=?`, projectID, userID)
	return err
}

// ProjectRoleOf 返回用户在项目内的角色；不是成员则返回空串。
// 全局管理员不走这里，调用方要先判 Role。
func (d *DB) ProjectRoleOf(userID, projectID int64) string {
	var role string
	if err := d.QueryRow(`SELECT role FROM project_members WHERE project_id=? AND user_id=?`,
		projectID, userID).Scan(&role); err != nil {
		return ""
	}
	return role
}

// ---------- 可见性 ----------

// VisibleProjects 返回用户能看到的项目 id。all=true 表示不受限（全局管理员）。
//
// 返回 nil + true 而不是项目 id 列表，是为了让调用方能区分「全都能看」和
// 「一个都看不了」——后者按空列表过滤会查出全量数据，是最危险的那种错。
func (d *DB) VisibleProjects(u *User) (ids []int64, all bool, err error) {
	if u == nil {
		return nil, false, nil
	}
	if u.Role == "admin" {
		return nil, true, nil
	}
	rows, err := d.Query(`SELECT project_id FROM project_members WHERE user_id=?`, u.ID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		ids = append(ids, id)
	}
	if ids == nil {
		ids = []int64{} // 没有任何项目时返回空切片，不是 nil
	}
	return ids, false, rows.Err()
}

// CanAdminProject 判断用户能否改某个项目的配置：全局管理员或该项目的项目管理员。
func (d *DB) CanAdminProject(u *User, projectID int64) bool {
	if u == nil {
		return false
	}
	if u.Role == "admin" {
		return true
	}
	return d.ProjectRoleOf(u.ID, projectID) == RoleProjectAdmin
}

// CanViewProject 判断用户能否看到某个项目：全局管理员、或项目成员（任意角色）。
func (d *DB) CanViewProject(u *User, projectID int64) bool {
	if u == nil {
		return false
	}
	if u.Role == "admin" {
		return true
	}
	return d.ProjectRoleOf(u.ID, projectID) != ""
}

// UserBrief 是「可以被加进项目的人」的最小信息。刻意不带 feishu_user_id 和 role——
// 项目管理员需要知道有哪些人能加，不需要知道他们的飞书身份和全局角色。
type UserBrief struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

// ListAddableUsers 返回还没加入该项目的人，供项目管理员添加成员时挑选。
//
// 这是项目管理员需要的「候选名单」。不能让他们去读全局用户表——那张表带飞书 ID
// 和全局角色，属于管理员信息，而且普通成员连碰都不该碰。
func (d *DB) ListAddableUsers(projectID int64) ([]UserBrief, error) {
	rows, err := d.Query(`SELECT u.id, COALESCE(u.name,''), COALESCE(u.avatar,'')
		FROM users u
		WHERE u.id NOT IN (SELECT user_id FROM project_members WHERE project_id=?)
		ORDER BY u.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserBrief{}
	for rows.Next() {
		var u UserBrief
		if err := rows.Scan(&u.ID, &u.Name, &u.Avatar); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
