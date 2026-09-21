package review

import (
	"strings"
	"testing"

	"aiudge/internal/gitx"
)

// TestFileContextRemovedFile 整文件删除没有新行可定位，上下文必须改成把删掉的内容交出去，
// 否则模型看到的只有一句「这个文件被删了」，判不出有没有人还在用。
func TestFileContextRemovedFile(t *testing.T) {
	ch := gitx.FileChange{
		Path:    "docs/board.md",
		Removed: true,
		Deleted: []string{"1: # 任务看板", "2: - T-001"},
	}
	got := fileContext("不存在的目录", ch, nil) // 删除的文件不读盘，tb 用不上
	if !strings.Contains(got, "整个文件被本次提交删除") || !strings.Contains(got, "docs/board.md") {
		t.Fatalf("要说清是哪个文件被整个删了，得到：%s", got)
	}
	if !strings.Contains(got, "2: - T-001") {
		t.Errorf("删掉的内容要原样带上，得到：%s", got)
	}
}

// TestDeletedSnippet 普通文件的纯删除块附在上下文后面；没有删除时不加噪音。
func TestDeletedSnippet(t *testing.T) {
	if got := deletedSnippet(nil); got != "" {
		t.Errorf("没有删除就不该多出一段，得到 %q", got)
	}
	got := deletedSnippet([]string{"7: \toldCall()"})
	if !strings.Contains(got, "本次删除的代码") || !strings.Contains(got, "7: \toldCall()") {
		t.Errorf("删除块要带原文，得到 %q", got)
	}
}
