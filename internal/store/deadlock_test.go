package store

import (
	"testing"
	"time"
)

// timeout 把「卡死」变成「失败」。SQLite 连接池上限是 1，一旦有查询在游标未关时
// 再发查询，就会去等一个永远不释放的连接——测试会永久挂起，CI 上表现为超时而不是报错。
// 这里主动加超时，让这类回归以明确的失败信息出现。
func timeout(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: 5 秒未返回，疑似在游标遍历中又发了查询，连接池被自己占住（死锁）", name)
	}
}

// TestDistributionDoesNotDeadlock 曾经的真实故障：按子系统分布时，
// 循环里对每条问题调 MatchSubsystem 去查规则表，而外层游标还没关。
// 连接池只有 1 条连接，整个服务连同所有 HTTP 请求一起卡死。
func TestDistributionDoesNotDeadlock(t *testing.T) {
	db := newTestDB(t)
	seed := []string{
		"server/node/battlesvr/battle/calc.go",
		"server/node/corpssvr/corps/apply.go",
		"server/node/spawnsvr/x.go",
		"server/base/nothing.go",
	}
	for i, p := range seed {
		_, err := db.UpsertIssue(&Issue{
			Fingerprint: p, RepoID: 1, Source: SourceAI, Category: CategoryLogic,
			Severity: SeverityAdvice, FilePath: p, LineStart: i + 1, LineEnd: i + 1, Title: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// 规则按仓库走，这里给仓库 1 配一套
	if err := db.ReplaceSubsystems(1, []Subsystem{
		{Pattern: "battlesvr", Name: "战斗系统"},
		{Pattern: "corpssvr", Name: "公会系统"},
	}); err != nil {
		t.Fatal(err)
	}
	// AllProjects 必须显式给：零值的 StatsFilter 表示「什么都看不到」而不是「不过滤」，
	// 这是刻意的（见 visibility.go），不显式声明就查不到任何东西
	f := StatsFilter{AllProjects: true}
	timeout(t, "Distribution(by=subsystem)", func() {
		items, err := db.Distribution("subsystem", f)
		if err != nil {
			t.Errorf("查询失败: %v", err)
			return
		}
		got := map[string]int{}
		for _, it := range items {
			got[it.Key] = it.Count
		}
		if got["战斗系统"] != 1 || got["公会系统"] != 1 {
			t.Errorf("归类不对: %v", items)
		}
		if got["未分类"] != 2 {
			t.Errorf("未命中的路径应归「未分类」，得到 %v", items)
		}
	})

	// 其余几个分布维度也走一遍，确保没有别的地方在游标里查库
	for _, by := range []string{"category", "source", "repo"} {
		by := by
		timeout(t, "Distribution(by="+by+")", func() {
			if _, err := db.Distribution(by, f); err != nil {
				t.Errorf("查询失败: %v", err)
			}
		})
	}
	timeout(t, "Overview", func() {
		if _, err := db.Overview(f); err != nil {
			t.Errorf("查询失败: %v", err)
		}
	})
	timeout(t, "Trend", func() {
		if _, err := db.Trend(f); err != nil {
			t.Errorf("查询失败: %v", err)
		}
	})
	timeout(t, "ListIssues", func() {
		if _, _, err := db.ListIssues(IssueFilter{Limit: 10}); err != nil {
			t.Errorf("查询失败: %v", err)
		}
	})
}

// TestAllReadsReturn 把所有读方法都跑一遍。死锁的表现是「挂起」而不是「报错」，
// 只测出问题的那一个方法不够——下一个在循环里顺手查库的人不知道这个坑。
func TestAllReadsReturn(t *testing.T) {
	db := newTestDB(t)
	repoID, err := db.CreateRepo(RepoInput{Name: "r", GitURL: "u", ExcludeGlobs: []string{"autogen"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertIssue(&Issue{Fingerprint: "f1", RepoID: repoID, Source: SourceAI,
		Category: CategoryLogic, Severity: SeverityHigh, FilePath: "a/b.go", LineStart: 1, LineEnd: 2, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	task := &Task{RepoID: repoID, TriggerType: "manual", Status: "success"}
	if err := db.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	db.AppendTaskLog(task.ID, "info", "hello")
	rep := &Report{TaskID: task.ID, RepoID: repoID}
	if err := db.CreateReport(rep); err != nil {
		t.Fatal(err)
	}

	reads := map[string]func() error{
		"ListRepos":        func() error { _, e := db.ListAllRepos(); return e },
		"GetRepo":          func() error { _, e := db.GetRepo(repoID); return e },
		"RepoCredential":   func() error { _, e := db.RepoCredential(repoID); return e },
		"GetModelConfig":   func() error { _, e := db.GetModelConfig(); return e },
		"GatewayKey":       func() error { _, e := db.GatewayKey(); return e },
		"ListTasks":        func() error { _, _, e := db.ListTasks(0, "", 10, 0, nil, true); return e },
		"GetTask":          func() error { _, e := db.GetTask(task.ID); return e },
		"ListTaskLogs":     func() error { _, e := db.ListTaskLogs(task.ID); return e },
		"ReviewedSet":      func() error { _, e := db.ReviewedSet(repoID, "2000-01-01 00:00:00"); return e },
		"ListReports":      func() error { _, _, e := db.ListReports(0, "", "", 10, 0, nil, true); return e },
		"GetReport":        func() error { _, e := db.GetReport(rep.ID); return e },
		"ListReportIssues": func() error { _, e := db.ListReportIssues(rep.ID); return e },
		"ListIssues":       func() error { _, _, e := db.ListIssues(IssueFilter{Limit: 10, AllProjects: true}); return e },
		"GetIssue":         func() error { _, e := db.GetIssue(1); return e },
		"ListComments":     func() error { _, e := db.ListComments(1); return e },
		"ListIssueEvents":  func() error { _, e := db.ListIssueEvents(1); return e },
		"ListUsers":        func() error { _, e := db.ListUsers(); return e },
		"ListIdentities":   func() error { _, e := db.ListIdentities(); return e },
		"ListSubsystems":   func() error { _, e := db.ListSubsystems(repoID); return e },
		"LoadMatcher":      func() error { db.LoadMatcher(repoID); return nil },
		"ListAudit":        func() error { _, _, e := db.ListAudit(10, 0); return e },
		"Overview":         func() error { _, e := db.Overview(StatsFilter{AllProjects: true}); return e },
		"Trend":            func() error { _, e := db.Trend(StatsFilter{AllProjects: true}); return e },
		"AssigneeStats":    func() error { _, e := db.AssigneeStats(StatsFilter{AllProjects: true}); return e },
		"VersionTags":      func() error { _, e := db.VersionTags(0, "", "", "week", nil, true); return e },
	}
	for name, fn := range reads {
		name, fn := name, fn
		timeout(t, name, func() {
			if err := fn(); err != nil {
				t.Errorf("返回错误: %v", err)
			}
		})
	}
}

// TestMatcherIsPure Match 不该碰数据库——它是循环里逐条调用的热路径。
func TestMatcherIsPure(t *testing.T) {
	db := newTestDB(t)
	if err := db.ReplaceSubsystems(1, []Subsystem{{Pattern: "battlesvr", Name: "战斗系统"}}); err != nil {
		t.Fatal(err)
	}
	m := db.LoadMatcher(1)
	timeout(t, "Matcher.Match", func() {
		for i := 0; i < 200; i++ {
			if got := m.Match("server/node/battlesvr/battle/calc.go"); got != "战斗系统" {
				t.Errorf("得到 %q", got)
				return
			}
		}
	})
	if got := m.Match("totally/unknown.go"); got != "未分类" {
		t.Errorf("未命中应归未分类，得到 %q", got)
	}
}
