package gitx

import "testing"

// TestParseUnifiedDiff 覆盖审核粒度的输入源：改动行号必须精确到新文件坐标，
// 否则「改动行所在函数」会圈错函数（§附录决策 16）。
// 样例按 `--unified=0` 的真实输出构造——hunk 里没有上下文行，区间即改动行本身。
func TestParseUnifiedDiff(t *testing.T) {
	diff := `diff --git a/server/x.go b/server/x.go
index 111..222 100644
--- a/server/x.go
+++ b/server/x.go
@@ -10,0 +11,2 @@ func Foo() {
+	b := 2
+	c := 3
@@ -20,1 +22,3 @@
-	old
+	new1
+	new2
+	new3
diff --git a/old.go b/old.go
deleted file mode 100644
--- a/old.go
+++ /dev/null
@@ -1,5 +0,0 @@
-package old
`
	files := parseUnifiedDiff(diff)
	if len(files) != 2 {
		t.Fatalf("改动文件 + 被删除的文件都该出现，期望 2 个，得到 %d", len(files))
	}
	f := files[0]
	if f.Path != "server/x.go" {
		t.Errorf("路径应去掉 b/ 前缀，得到 %q", f.Path)
	}
	// 整文件删除也要留下一条记录：删掉的东西还有没有人用，正是要审的问题
	if d := files[1]; d.Path != "old.go" || !d.Removed || len(d.Deleted) != 1 {
		t.Errorf("删除的文件应记为 old.go/removed 并带上删掉的内容，得到 %+v", d)
	}
	if len(f.Lines) != 2 {
		t.Fatalf("期望 2 个改动块，得到 %d", len(f.Lines))
	}
	if f.Lines[0] != (LineRange{11, 12}) {
		t.Errorf("纯新增块应为 11-12，得到 %+v", f.Lines[0])
	}
	if f.Lines[1] != (LineRange{22, 24}) {
		t.Errorf("替换块应为 22-24，得到 %+v", f.Lines[1])
	}
}

// TestParseUnifiedDiffPureDeletion 纯删除的 hunk 没有新行可审，不能产出空区间，
// 但删掉的原文要留下来——不然「删了什么」在上下文里彻底看不见。
func TestParseUnifiedDiffPureDeletion(t *testing.T) {
	diff := `--- a/x.go
+++ b/x.go
@@ -5,3 +5,0 @@
-	a
-	b
-	c
`
	files := parseUnifiedDiff(diff)
	if len(files) != 1 || len(files[0].Lines) != 0 {
		t.Fatalf("纯删除不应产生改动行，得到 %+v", files)
	}
	got := files[0].Deleted
	want := []string{"5: \ta", "6: \tb", "7: \tc"}
	if len(got) != len(want) {
		t.Fatalf("删掉的行要带原行号留下，期望 %v，得到 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 行：期望 %q，得到 %q", i, want[i], got[i])
		}
	}
}

// TestParseUnifiedDiffDeletedMarkdown hunk 里的内容是任意文本，删掉的 Markdown 分隔线
// `---` 不能被当成文件头，否则删除内容会被切断、甚至凭空多出文件记录。
func TestParseUnifiedDiffDeletedMarkdown(t *testing.T) {
	diff := `diff --git a/docs/board.md b/docs/board.md
deleted file mode 100644
--- a/docs/board.md
+++ /dev/null
@@ -1,4 +0,0 @@
-# 看板
-
----
-- T-001
`
	files := parseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("只应有一个（被删除的）文件，得到 %d 个：%+v", len(files), files)
	}
	if f := files[0]; f.Path != "docs/board.md" || !f.Removed || len(f.Deleted) != 4 {
		t.Errorf("期望 docs/board.md 的 4 行删除内容，得到 %+v", f)
	}
}

// TestParseUnifiedDiffQuotedPath 中文/带空格的路径 git 会加引号并转义，
// 剥不掉前缀就取不到文件，这些改动会静默地不进审核上下文。
func TestParseUnifiedDiffQuotedPath(t *testing.T) {
	diff := `--- "a/docs/开服第X天/方案 一.md"
+++ "b/docs/开服第X天/方案 一.md"
@@ -0,0 +1,2 @@
+	x
+	y
`
	files := parseUnifiedDiff(diff)
	if len(files) != 1 {
		t.Fatalf("期望 1 个文件，得到 %d", len(files))
	}
	if got := files[0].Path; got != "docs/开服第X天/方案 一.md" {
		t.Errorf("带引号的路径要去引号、解转义并剥掉 b/ 前缀，得到 %q", got)
	}
}

func TestRedact(t *testing.T) {
	got := Redact("https://oauth2:glpat-secret@gitlab.x/slg/server.git")
	if got == "" || contains(got, "glpat-secret") {
		t.Errorf("凭据必须被抹掉，得到 %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
