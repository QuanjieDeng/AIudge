package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// newRepoWithDatedCommits 造一个带固定提交时间的仓库。
// 提交时间写死是为了让「按时间范围筛提交」可断言——用 now 的话测试会在跨天时随机红。
func newRepoWithDatedCommits(t *testing.T, dates ...string) *Git {
	t.Helper()
	dir := t.TempDir()
	git := func(date, msg string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{
			"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false",
		}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("", "", "init", "-q")
	for i, d := range dates {
		name := filepath.Join(dir, "f"+string(rune('a'+i))+".txt")
		if err := os.WriteFile(name, []byte(d), 0o644); err != nil {
			t.Fatal(err)
		}
		git(d, "", "add", "-A")
		git(d, "", "commit", "-q", "-m", "c"+string(rune('a'+i)))
	}
	return Open(dir)
}

// TestLogSinceWindow 手动触发的时间范围就靠这个函数，参数拼错会静默审错一批代码。
func TestLogSinceWindow(t *testing.T) {
	g := newRepoWithDatedCommits(t,
		"2026-03-01T10:00:00+08:00", // 窗外的老提交
		"2026-03-05T10:00:00+08:00", // 窗内
		"2026-03-06T10:00:00+08:00", // 窗内
		"2026-03-20T10:00:00+08:00", // 窗外的新提交
	)
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}

	// 只给下界：上界不设限，应该拿到窗内两条 + 更晚的那一条
	commits, err := g.LogSince(at("2026-03-05T00:00:00+08:00"), time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 3 {
		t.Fatalf("只给 since 应拿到 3 条（新到旧），实际 %d 条", len(commits))
	}
	if commits[0].Subject != "cd" { // git log 默认新在前
		t.Errorf("应按时间倒序，首条是 %q", commits[0].Subject)
	}

	// 两端都给：只剩窗内的两条
	commits, err = g.LogSince(at("2026-03-05T00:00:00+08:00"), at("2026-03-06T23:59:59+08:00"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || commits[1].Subject != "cb" {
		t.Fatalf("时间窗内应正好是 cb/c c 两条，实际 %+v", commits)
	}

	// limit：时间窗再大也截断，这是上下文预算的硬上限
	commits, err = g.LogSince(at("2026-03-01T00:00:00+08:00"), time.Time{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("limit=1 应只返回 1 条，实际 %d 条", len(commits))
	}
}
