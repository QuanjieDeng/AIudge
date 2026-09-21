package store

import "testing"

// TestResetKeepsConfig 清档的语义边界：业务数据清光，配置全部留下。
// 测试阶段反复清档是为了丢掉跑出来的噪音，不是把仓库和网关重配一遍。
func TestResetKeepsConfig(t *testing.T) {
	db := newTestDB(t)

	// 配置类
	repoID, err := db.CreateRepo(RepoInput{Name: "r", GitURL: "u", Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSubsystems(repoID, []Subsystem{{Pattern: "x", Name: "X 系统"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertIdentity("someone", 0, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertUser("ou_1", "张三", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateModelConfig("http://gw", nil, 600, []string{"m1"}, "m1"); err != nil {
		t.Fatal(err)
	}

	// 业务数据
	iss := &Issue{Fingerprint: "f1", RepoID: repoID, Source: SourceAI, Category: CategoryLogic,
		Severity: SeverityHigh, FilePath: "a.go", LineStart: 1, LineEnd: 1, Title: "t"}
	ur, err := db.UpsertIssue(iss)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddComment(ur.IssueID, 1, "张三", "看了看"); err != nil {
		t.Fatal(err)
	}
	task := &Task{RepoID: repoID, TriggerType: "manual", Status: "success"}
	if err := db.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	db.AppendTaskLog(task.ID, "info", "hi")
	rep := &Report{TaskID: task.ID, RepoID: repoID}
	if err := db.CreateReport(rep); err != nil {
		t.Fatal(err)
	}
	db.MarkReviewed(repoID, []string{"sha1"}, task.ID)
	db.AppendAudit(1, "张三", "x", "y", "z", "")

	counts, err := db.ResetData()
	if err != nil {
		t.Fatalf("清档失败: %v", err)
	}
	if counts["issues"] != 1 || counts["review_tasks"] != 1 || counts["review_reports"] != 1 {
		t.Errorf("统计不对: %+v", counts)
	}

	// 业务数据必须全空
	for _, check := range []struct {
		name string
		fn   func() (int, error)
	}{
		{"issues", func() (int, error) { _, n, e := db.ListIssues(IssueFilter{Limit: 10}); return n, e }},
		{"review_tasks", func() (int, error) { _, n, e := db.ListTasks(0, "", 10, 0, nil, true); return n, e }},
		{"review_reports", func() (int, error) { _, n, e := db.ListReports(0, "", "", 10, 0, nil, true); return n, e }},
		{"audit_logs", func() (int, error) { _, n, e := db.ListAudit(10, 0); return n, e }},
	} {
		n, err := check.fn()
		if err != nil {
			t.Fatalf("%s 查询失败: %v", check.name, err)
		}
		if n != 0 {
			t.Errorf("%s 应清空，还剩 %d 条", check.name, n)
		}
	}
	remarks, _ := db.ReviewedSet(repoID, "2000-01-01 00:00:00")
	if len(remarks) != 0 {
		t.Errorf("已审核 commit 标记应清空，还剩 %d 条", len(remarks))
	}

	// 配置必须原样都在
	if _, err := db.GetRepo(repoID); err != nil {
		t.Errorf("仓库配置被清掉了: %v", err)
	}
	rules, _ := db.ListSubsystems(repoID)
	if len(rules) != 1 {
		t.Errorf("子系统规则被清掉了: %+v", rules)
	}
	if ids, _ := db.ListIdentities(); len(ids) != 1 {
		t.Errorf("身份归并被清掉了: %+v", ids)
	}
	// 库里除了测试建的那个用户，还有初始化时种下的内置管理员
	us, _ := db.ListUsers()
	var found bool
	for _, u := range us {
		if u.FeishuID == "ou_1" {
			found = true
		}
	}
	if !found {
		t.Errorf("用户被清掉了: %+v", us)
	}
	if len(us) < 2 {
		t.Errorf("内置管理员应当保留，得到 %+v", us)
	}
	if mc, _ := db.GetModelConfig(); mc.GatewayURL != "http://gw" || mc.DefaultModel != "m1" {
		t.Errorf("模型配置被清掉了: %+v", mc)
	}
}

// TestResetRestartsIDs 清档后编号从 1 重新开始——测试时对着一堆 #47 的数看着累。
func TestResetRestartsIDs(t *testing.T) {
	db := newTestDB(t)
	repoID, _ := db.CreateRepo(RepoInput{Name: "r", GitURL: "u"})
	for i := 0; i < 3; i++ {
		task := &Task{RepoID: repoID, TriggerType: "manual", Status: "success"}
		db.CreateTask(task)
	}
	if _, err := db.ResetData(); err != nil {
		t.Fatal(err)
	}
	task := &Task{RepoID: repoID, TriggerType: "manual", Status: "queued"}
	if err := db.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if task.ID != 1 {
		t.Errorf("清档后任务编号应从 1 开始，得到 %d", task.ID)
	}
}

// TestResetOnEmptyDB 空库上清档不该报错——重复点按钮是常见操作。
func TestResetOnEmptyDB(t *testing.T) {
	db := newTestDB(t)
	counts, err := db.ResetData()
	if err != nil {
		t.Fatalf("空库清档失败: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("空库不该有统计，得到 %+v", counts)
	}
}
