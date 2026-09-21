package store

import "testing"

// TestVisibilityZeroValueSeesNothing 是这套过滤的安全底线：
// 零值的过滤条件必须表示「什么都看不到」，绝不能表示「不过滤」。
// 后者是数据泄漏，而且是最容易写出来、最不容易被发现的一种。
func TestVisibilityZeroValueSeesNothing(t *testing.T) {
	db := newTestDB(t)
	repoID, _ := db.CreateRepo(RepoInput{Name: "r", GitURL: "u"})
	_, _ = db.UpsertIssue(&Issue{Fingerprint: "f", RepoID: repoID, Source: SourceAI,
		Category: CategoryLogic, Severity: SeverityHigh, FilePath: "a.go", LineStart: 1, LineEnd: 1, Title: "t"})

	// 零值 = 无可见项目 = 一条都不该出来
	issues, total, err := db.ListIssues(IssueFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(issues) != 0 {
		t.Errorf("零值过滤应查不到任何问题，得到 total=%d", total)
	}
	repos, err := db.ListRepos(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Errorf("零值过滤应查不到任何仓库，得到 %d 个", len(repos))
	}

	// 显式放开才看得到
	_, total2, _ := db.ListIssues(IssueFilter{Limit: 10, AllProjects: true})
	if total2 != 1 {
		t.Errorf("AllProjects 应看到全部，得到 %d", total2)
	}
}

// TestVisibleProjectsPerMembership 可见项目按成员关系算，全局管理员不受限。
func TestVisibleProjectsPerMembership(t *testing.T) {
	db := newTestDB(t)
	a, _ := db.CreateProject("A", "")
	b, _ := db.CreateProject("B", "")
	member, _ := db.UpsertUser("ou_m", "成员", "")
	boss, _ := db.UpsertUser("ou_b", "全局管理员", "")
	if err := db.SetUserRole(boss.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertProjectMember(a, member.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	boss, _ = db.UserByID(boss.ID)

	ids, all, err := db.VisibleProjects(member)
	if err != nil {
		t.Fatal(err)
	}
	if all || len(ids) != 1 || ids[0] != a {
		t.Errorf("成员应只看到项目 A，得到 ids=%v all=%v", ids, all)
	}
	if _, all, _ := db.VisibleProjects(boss); !all {
		t.Error("全局管理员应不受项目限制")
	}

	// 一个项目都不参与的人：空列表 + 非 all，调用方据此走「查不到」而非「不过滤」
	loner, _ := db.UpsertUser("ou_l", "路人", "")
	ids2, all2, _ := db.VisibleProjects(loner)
	if all2 || len(ids2) != 0 {
		t.Errorf("非成员应看不到任何项目，得到 ids=%v all=%v", ids2, all2)
	}
	if b == 0 {
		t.Fatal("项目 B 应创建成功")
	}
}

// TestProjectRoleChecks 项目内角色判定：项目管理员能改配置，成员只能看。
func TestProjectRoleChecks(t *testing.T) {
	db := newTestDB(t)
	pid, _ := db.CreateProject("P", "")
	pa, _ := db.UpsertUser("ou_pa", "项目管理员", "")
	mb, _ := db.UpsertUser("ou_mb", "普通成员", "")
	out, _ := db.UpsertUser("ou_out", "局外人", "")
	db.SetUserRole(out.ID, "user")
	db.UpsertProjectMember(pid, pa.ID, RoleProjectAdmin)
	db.UpsertProjectMember(pid, mb.ID, RoleMember)

	pa, _ = db.UserByID(pa.ID)
	mb, _ = db.UserByID(mb.ID)
	out, _ = db.UserByID(out.ID)

	if !db.CanAdminProject(pa, pid) {
		t.Error("项目管理员应能改配置")
	}
	if db.CanAdminProject(mb, pid) {
		t.Error("普通成员不该能改配置")
	}
	if db.CanAdminProject(out, pid) {
		t.Error("局外人不该能改配置")
	}
	if !db.CanViewProject(mb, pid) || db.CanViewProject(out, pid) {
		t.Error("可见性判定不对")
	}
}
