package review

import "testing"

// TestFingerprintStableAcrossNoise 覆盖 §4.5 的核心前提：行号漂移、缩进变化、
// 注释改动都不能改变问题身份，否则趋势统计会退化成「文件被改频率图」。
func TestFingerprintStableAcrossNoise(t *testing.T) {
	base := `func Add(a, b int) int {
	return a + b
}`
	cases := []struct {
		name    string
		snippet string
	}{
		{"原样", base},
		{"缩进变了", "  func Add(a, b int) int {\n\t\treturn a + b\n  }"},
		{"加了注释", "// 求和\nfunc Add(a, b int) int {\n\treturn a + b\n}"},
		{"多了空行", "func Add(a, b int) int {\n\n\treturn a + b\n\n}"},
	}
	want := Fingerprint(1, "a/b.go", "逻辑", base, "")
	for _, c := range cases {
		if got := Fingerprint(1, "a/b.go", "逻辑", c.snippet, ""); got != want {
			t.Errorf("%s: 指纹应当相同\n got=%s\nwant=%s", c.name, got, want)
		}
	}

	// 真正不同的维度必须改变指纹，否则不同问题会被折叠成一条
	diffs := []struct {
		name     string
		repo     int64
		path     string
		category string
		snippet  string
		linter   string
	}{
		{"仓库不同", 2, "a/b.go", "逻辑", base, ""},
		{"路径不同", 1, "a/c.go", "逻辑", base, ""},
		{"分类不同", 1, "a/b.go", "安全", base, ""},
		{"代码不同", 1, "a/b.go", "逻辑", base + "\nvar x = 1", ""},
		{"linter 不同", 1, "a/b.go", "规范", base, "errcheck"},
	}
	for _, d := range diffs {
		if got := Fingerprint(d.repo, d.path, d.category, d.snippet, d.linter); got == want {
			t.Errorf("%s: 指纹不应相同", d.name)
		}
	}
}

// TestFingerprintIgnoresLineNumbers 是回归检测能成立的关键：同一段代码换了位置
// 必须得到同一个指纹，否则「改回来」永远认不出来。
func TestFingerprintIgnoresLineNumbers(t *testing.T) {
	snippet := "if err != nil {\n\treturn nil\n}"
	// 指纹入参里根本没有行号，这里断言的是接口契约：调用方不需要传行号也能去重
	if Fingerprint(1, "x.go", "逻辑", snippet, "") != Fingerprint(1, "x.go", "逻辑", snippet, "") {
		t.Fatal("同一输入必须得到同一指纹")
	}
}
