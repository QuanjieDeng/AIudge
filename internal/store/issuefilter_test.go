package store

import "testing"

// TestIssueAssigneeFilter 问题库要能按「负责人」筛，而且是多选：可以同时勾几个人，
// 也可以勾上「未指派」。
//
// 「未指派」在这里就是 assignee_id 的一个普通取值 0，不是单独的标志位——
// 真要有标志位，勾「张三 + 未指派」这种组合就没法表达了（那是个 OR，不是 AND）。
func TestIssueAssigneeFilter(t *testing.T) {
	db := newTestDB(t)
	seed := []struct {
		fp       string
		assignee int64
	}{
		{"f1", 7},
		{"f2", 0},
		{"f3", 0},
		{"f4", 9},
	}
	for i, s := range seed {
		// 负责人不走 UpsertIssue（审核管线写入时没人认领），得单独指派一次
		res, err := db.UpsertIssue(&Issue{
			Fingerprint: s.fp, RepoID: 1, Source: SourceAI, Category: CategoryLogic,
			Severity: SeverityAdvice, FilePath: "a.go", LineStart: i + 1, LineEnd: i + 1,
			Title: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
		if s.assignee > 0 {
			if err := db.AssignIssue(res.IssueID, s.assignee, 1, "tester"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 零值过滤器是「什么都看不到」而不是「不过滤」，所以可见性要显式给
	all := IssueFilter{AllProjects: true, Limit: 50}

	count := func(f IssueFilter) int {
		t.Helper()
		f.AllProjects = true
		f.Limit = 50
		_, n, err := db.ListIssues(f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n := count(all); n != 4 {
		t.Fatalf("不带过滤应查到 4 条，实际 %d", n)
	}
	if n := count(IssueFilter{AssigneeIDs: []int64{7}}); n != 1 {
		t.Errorf("按人筛应查到 1 条，实际 %d", n)
	}
	if n := count(IssueFilter{AssigneeIDs: []int64{0}}); n != 2 {
		t.Errorf("筛「未指派」应查到 2 条，实际 %d", n)
	}
	// 多选是 OR：勾两个人就是两个人的都要
	if n := count(IssueFilter{AssigneeIDs: []int64{7, 9}}); n != 2 {
		t.Errorf("勾两个人应查到 2 条，实际 %d", n)
	}
	// 「某人 + 未指派」也要能表达，这类组合在标志位写法里表达不出来
	if n := count(IssueFilter{AssigneeIDs: []int64{9, 0}}); n != 3 {
		t.Errorf("「9 号 + 未指派」应查到 3 条，实际 %d", n)
	}
}

// TestIssueMultiSelectFilters 等级/状态/分类/来源都改成多选后，
// 勾多个是 OR（都要），一个不勾就是不筛这一项——不能变成「筛空值」查出 0 条。
func TestIssueMultiSelectFilters(t *testing.T) {
	db := newTestDB(t)
	for i, s := range []struct{ sev, status, cat string }{
		{"高危", StatusPending, CategorySecurity},
		{"建议", StatusPending, CategoryPerf},
		{"风格", StatusFixed, CategoryLogic},
	} {
		if _, err := db.UpsertIssue(&Issue{
			Fingerprint: s.sev, RepoID: 1, Source: SourceAI, Category: s.cat, Severity: s.sev,
			Status: s.status, FilePath: "a.go", LineStart: i + 1, LineEnd: i + 1, Title: "x",
		}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(f IssueFilter) int {
		t.Helper()
		f.AllProjects, f.Limit = true, 50
		_, n, err := db.ListIssues(f)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n := count(IssueFilter{Severities: []string{"高危", "建议"}}); n != 2 {
		t.Errorf("勾两个等级应查到 2 条，实际 %d", n)
	}
	if n := count(IssueFilter{Severities: []string{"高危"}}); n != 1 {
		t.Errorf("勾一个等级应查到 1 条，实际 %d", n)
	}
	if n := count(IssueFilter{Severities: []string{}, Statuses: []string{}}); n != 3 {
		t.Errorf("一项都不勾应是「不筛」而不是「筛空」；应查到 3 条，实际 %d", n)
	}
	if n := count(IssueFilter{Statuses: []string{StatusPending}, Categories: []string{CategoryPerf}}); n != 1 {
		t.Errorf("两个维度之间是 AND，应查到 1 条，实际 %d", n)
	}
	if n := count(IssueFilter{Severities: []string{"不存在"}}); n != 0 {
		t.Errorf("勾一个不存在的值应查到 0 条，实际 %d", n)
	}
}

// TestProjectMemberCarriesAliases 指派选择器靠这些字段做「中文名或英文名」的模糊匹配：
// 姓名、登录名，以及身份归并里绑到这个人的 git 身份（dengquanjie1 这类）。
// 少了别名，界面上表现为「搜不到人」，不像报错那样显眼。
func TestProjectMemberCarriesAliases(t *testing.T) {
	db := newTestDB(t)
	u, err := db.UpsertUser("ou_deng", "邓全杰", "")
	if err != nil {
		t.Fatal(err)
	}
	// username 只有内置管理员/密码登录那条路径会写，测试里直接落库
	if _, err := db.Exec(`UPDATE users SET username='dengquanjie' WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	// 一个真人身份 + 一个机器人身份：机器人不是人的名字，不该被搜出来
	if err := db.UpsertIdentity("dengquanjie1", u.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertIdentity("ci-bot", u.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	// 用一个不会和别的用例撞车的项目 id
	pid := int64(42)
	if err := db.UpsertProjectMember(pid, u.ID, RoleMember); err != nil {
		t.Fatal(err)
	}

	list, err := db.ListProjectMembers(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应只有 1 个成员，实际 %d", len(list))
	}
	m := list[0]
	if m.Name != "邓全杰" || m.Username != "dengquanjie" {
		t.Errorf("姓名/登录名没带出来: %+v", m)
	}
	if len(m.Aliases) != 1 || m.Aliases[0] != "dengquanjie1" {
		t.Errorf("别名应只有真人身份 dengquanjie1，实际 %v（机器人身份被带出来了？）", m.Aliases)
	}
}
