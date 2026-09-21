package store

import (
	"path/filepath"
	"testing"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestRegressionFlow 是 §7 的验收项「回归检测可验证」：
// 把一处已标记「已修复」的改动改回来，系统要认出是同一个问题、翻回待处理并标回归。
func TestRegressionFlow(t *testing.T) {
	db := newTestDB(t)
	const fp = "fp-abc123"
	snippet := "if v == nil {\n\treturn 0\n}"

	first := &Issue{Fingerprint: fp, RepoID: 1, Source: SourceAI, Category: CategoryLogic,
		Severity: SeverityHigh, FilePath: "server/node/x/a.go", LineStart: 100, LineEnd: 102,
		Title: "空指针未处理", CodeSnippet: snippet, CommitSHA: "aaa"}
	res, err := db.UpsertIssue(first)
	if err != nil {
		t.Fatalf("首次入库失败: %v", err)
	}
	if !res.IsNew || res.IsRegression {
		t.Fatalf("首次入库应标记为新增且非回归，得到 %+v", res)
	}

	if err := db.SetIssueStatus(res.IssueID, StatusFixed, 1, "邓全杰", "已修"); err != nil {
		t.Fatalf("标记已修复失败: %v", err)
	}

	// 同一段代码在后续提交里再次出现：行号漂移到了 240
	again := *first
	again.LineStart, again.LineEnd = 240, 242
	again.CommitSHA = "bbb"
	res2, err := db.UpsertIssue(&again)
	if err != nil {
		t.Fatalf("回归入库失败: %v", err)
	}
	if res2.IssueID != res.IssueID {
		t.Errorf("回归应复用原记录：id %d → %d", res.IssueID, res2.IssueID)
	}
	if !res2.IsRegression {
		t.Error("应被识别为回归")
	}
	if res2.PrevStatus != StatusFixed {
		t.Errorf("回归前状态应为已修复，得到 %s", res2.PrevStatus)
	}

	got, err := db.GetIssue(res.IssueID)
	if err != nil {
		t.Fatalf("读取问题失败: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("回归后状态应翻回待处理，得到 %s", got.Status)
	}
	if !got.Regression {
		t.Error("regression 标记应为真")
	}
	if got.RegressionAt == "" {
		t.Error("应记录回归时间")
	}
	// 行号是展示字段，必须跟随最新一次
	if got.LineStart != 240 || got.LineEnd != 242 {
		t.Errorf("行号应更新为 240-242，得到 %d-%d", got.LineStart, got.LineEnd)
	}
	if got.SeenCount != 2 {
		t.Errorf("累计命中次数应为 2，得到 %d", got.SeenCount)
	}
	if got.ClosedAt != "" {
		t.Error("翻回待处理后应清空关闭时间")
	}
}

// TestUpsertKeepsOpenStatus 确保正常路径不会误判回归：未被关闭的问题重复命中
// 只更新展示字段，状态原地不动。
func TestUpsertKeepsOpenStatus(t *testing.T) {
	db := newTestDB(t)
	iss := &Issue{Fingerprint: "fp-keep", RepoID: 1, Source: SourceLint, Category: CategorySpec,
		Severity: SeverityAdvice, FilePath: "a.go", LineStart: 5, LineEnd: 5, Title: "errcheck"}
	first, _ := db.UpsertIssue(iss)
	if err := db.SetIssueStatus(first.IssueID, StatusConfirmed, 1, "u", ""); err != nil {
		t.Fatal(err)
	}
	iss.LineStart, iss.LineEnd = 88, 88
	res, err := db.UpsertIssue(iss)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsRegression {
		t.Error("已确认的问题重复出现不应算回归")
	}
	got, _ := db.GetIssue(res.IssueID)
	if got.Status != StatusConfirmed {
		t.Errorf("状态应保持已确认，得到 %s", got.Status)
	}
	if got.LineStart != 88 {
		t.Errorf("行号应更新，得到 %d", got.LineStart)
	}
}

// TestFalsePositiveAlsoRegresses 误报被改回来同样要翻回待处理（§4.5）。
func TestFalsePositiveAlsoRegresses(t *testing.T) {
	db := newTestDB(t)
	iss := &Issue{Fingerprint: "fp-fp", RepoID: 1, Source: SourceAI, Category: CategoryPerf,
		Severity: SeverityAdvice, FilePath: "a.go", LineStart: 1, LineEnd: 1, Title: "N+1"}
	first, _ := db.UpsertIssue(iss)
	if err := db.SetIssueStatus(first.IssueID, StatusFalsePos, 1, "u", ""); err != nil {
		t.Fatal(err)
	}
	res, _ := db.UpsertIssue(iss)
	if !res.IsRegression {
		t.Error("误报重现应被识别为回归")
	}
	got, _ := db.GetIssue(first.IssueID)
	if got.Status != StatusPending || !got.Regression {
		t.Errorf("误报重现后应为待处理+回归，得到 %s regression=%v", got.Status, got.Regression)
	}
}

// TestBotCommitterExcluded 机器人身份不参与人员统计（§4.8）。
func TestBotCommitterExcluded(t *testing.T) {
	db := newTestDB(t)
	if err := db.UpsertIdentity("ci-bot", 0, true, "CI"); err != nil {
		t.Fatal(err)
	}
	if uid, isBot := db.ResolveCommitter("ci-bot"); !isBot || uid != 0 {
		t.Errorf("机器人应返回 (0,true)，得到 (%d,%v)", uid, isBot)
	}
	if uid, isBot := db.ResolveCommitter("张三"); isBot || uid != 0 {
		t.Errorf("未登记身份应为 (0,false)，得到 (%d,%v)", uid, isBot)
	}
}

// TestSubsystemMatch 子系统归类走路径子串匹配，未命中归「未分类」。
// 规则按仓库走：同一个路径在不同仓库里可以归到不同分类。
func TestSubsystemMatch(t *testing.T) {
	db := newTestDB(t)
	a, _ := db.CreateRepo(RepoInput{Name: "a", GitURL: "u"})
	b, _ := db.CreateRepo(RepoInput{Name: "b", GitURL: "u"})

	if err := db.ReplaceSubsystems(a, []Subsystem{{Pattern: "battlesvr", Name: "战斗系统"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSubsystems(b, []Subsystem{{Pattern: "battlesvr", Name: "别的叫法"}}); err != nil {
		t.Fatal(err)
	}

	const path = "server/node/battlesvr/battle/calc.go"
	if got := db.MatchSubsystem(a, path); got != "战斗系统" {
		t.Errorf("仓库 a 应归战斗系统，得到 %s", got)
	}
	// 关键断言：同名路径在仓库 b 里按 b 自己的规则归类
	if got := db.MatchSubsystem(b, path); got != "别的叫法" {
		t.Errorf("仓库 b 应按自己的规则归类，得到 %s", got)
	}
	if got := db.MatchSubsystem(a, "some/unknown/place.go"); got != "未分类" {
		t.Errorf("未知路径应归未分类，得到 %s", got)
	}
	// 没配规则的仓库不该串到别人的规则
	c, _ := db.CreateRepo(RepoInput{Name: "c", GitURL: "u"})
	if got := db.MatchSubsystem(c, path); got != "未分类" {
		t.Errorf("未配规则的仓库应全部归未分类，得到 %s", got)
	}
}

// TestSubsystemRulesAreScoped 替换一个仓库的规则不能动到别的仓库。
func TestSubsystemRulesAreScoped(t *testing.T) {
	db := newTestDB(t)
	a, _ := db.CreateRepo(RepoInput{Name: "a", GitURL: "u"})
	b, _ := db.CreateRepo(RepoInput{Name: "b", GitURL: "u"})
	db.ReplaceSubsystems(a, []Subsystem{{Pattern: "x", Name: "A 的分类"}})
	db.ReplaceSubsystems(b, []Subsystem{{Pattern: "x", Name: "B 的分类"}})
	// 重写 a 的规则
	db.ReplaceSubsystems(a, []Subsystem{{Pattern: "y", Name: "A 改过"}})

	gotA, _ := db.ListSubsystems(a)
	gotB, _ := db.ListSubsystems(b)
	if len(gotA) != 1 || gotA[0].Name != "A 改过" {
		t.Errorf("a 的规则应为改后的 1 条，得到 %+v", gotA)
	}
	if len(gotB) != 1 || gotB[0].Name != "B 的分类" {
		t.Errorf("b 的规则不该被 a 影响，得到 %+v", gotB)
	}
}
