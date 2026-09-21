package gocx

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestScanDocs 只收文档、跳过 .git/vendor 这类目录、不把源码当规范文档列出来。
// 点目录（.claude/）要收——规范文档常常正好放在那里。
func TestScanDocs(t *testing.T) {
	root := t.TempDir()
	files := []string{"CLAUDE.md", "docs/spec.markdown", ".claude/doc/wiki/x.md", ".git/HEAD.md", "vendor/lib.md", "main.go"}
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ScanDocs(root)
	if err != nil {
		t.Fatal(err)
	}
	// 浅的先列：CLAUDE.md 在根、docs/spec.markdown 一层、.claude/... 在最深
	want := []string{"CLAUDE.md", "docs/spec.markdown", ".claude/doc/wiki/x.md"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
