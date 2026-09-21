package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "aiudge.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestValuesAreFileOnly 确认配置只认文件：环境变量里放什么都不该被读到。
// 这条守着「配置源头唯一」——环境变量一旦参与进来，「这个值从哪来的」就说不清了。
func TestValuesAreFileOnly(t *testing.T) {
	t.Setenv("AIUDGE_ADDR", ":9999")
	t.Setenv("AIUDGE_CONCURRENCY", "77")

	p := writeConf(t, "AIUDGE_ADDR=:1234\n")
	cfg := Load(p, true)
	if cfg.Addr != ":1234" {
		t.Errorf("应读文件里的值，得到 %q", cfg.Addr)
	}
	if cfg.Concurrency != 2 {
		t.Errorf("文件里没配就取内置默认 2，不该读环境变量，得到 %d", cfg.Concurrency)
	}
}

// TestParse 容错：注释、空行、export 前缀、引号、值里带等号。
func TestParse(t *testing.T) {
	p := writeConf(t, `
# 注释
AIUDGE_ADDR=:8787

export AIUDGE_BASE_URL="http://10.0.0.5:8787"
AIUDGE_FEISHU_APP_ID='cli_abc'
AIUDGE_SPEC_DIRS=/a:/b:c
AIUDGE_GATEWAY_KEY=sk-a=b=c
`)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := parse(f)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"AIUDGE_ADDR":          ":8787",
		"AIUDGE_BASE_URL":      "http://10.0.0.5:8787",
		"AIUDGE_FEISHU_APP_ID": "cli_abc",
		"AIUDGE_SPEC_DIRS":     "/a:/b:c",
		"AIUDGE_GATEWAY_KEY":   "sk-a=b=c",
	}
	if len(got) != len(want) {
		t.Fatalf("解析出 %d 项，期望 %d 项: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q，期望 %q", k, got[k], v)
		}
	}
}

// TestParseRejectsGarbage 格式不对要报错并指出行号，而不是静默忽略——
// 静默忽略会让人对着「配了没生效」查半天。
func TestParseRejectsGarbage(t *testing.T) {
	p := writeConf(t, "AIUDGE_ADDR=:1\n这行没有等号\n")
	f, _ := os.Open(p)
	defer f.Close()
	_, err := parse(f)
	if err == nil {
		t.Fatal("格式错误应当报错")
	}
	if !contains(err.Error(), "第 2 行") {
		t.Errorf("错误信息应指出行号，得到 %v", err)
	}
}

// TestBadIntFallsBack 数字项写错了退回默认值并告警，不该让整个服务起不来。
func TestBadIntFallsBack(t *testing.T) {
	p := writeConf(t, "AIUDGE_CONCURRENCY=abc\nAIUDGE_ADDR=:1234\n")
	cfg := Load(p, true)
	if cfg.Concurrency != 2 {
		t.Errorf("写错应退回默认值 2，得到 %d", cfg.Concurrency)
	}
	if cfg.Addr != ":1234" {
		t.Error("一项写错不该影响其他项")
	}
}

// TestSecretTightensPerms 文件里有密钥就收紧到 0600，
// 别的用户读到就等于拿到了这些凭据。
func TestSecretTightensPerms(t *testing.T) {
	p := writeConf(t, "AIUDGE_FEISHU_APP_SECRET=verysecret\n")
	Load(p, true)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("含密钥时权限应收紧到 0600，得到 %o", st.Mode().Perm())
	}
}

// TestMissingFileGeneratesTemplate 默认路径没文件时生成一份模板，
// 省得人去翻源码找有哪些可配项；模板里所有项都该是注释状态，不能替人做决定。
func TestMissingFileGeneratesTemplate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "aiudge.conf")
	v := LoadFile(p, false)
	if !v.Created {
		t.Error("新建模板时应标记 Created")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("应生成模板: %v", err)
	}
	s := string(b)
	if !contains(s, "AIUDGE_ADDR") || !contains(s, "AIUDGE_FEISHU_APP_ID") {
		t.Error("模板里应列出可配项")
	}
	for _, line := range splitLines(s) {
		if line != "" && line[0] != '#' {
			t.Errorf("模板里不该有未注释的配置行: %q", line)
		}
	}
}

// TestMissingFileNotCreatedWhenPathGiven 指定的路径不存在时直接失败，
// 不默默用默认值跑起来——那会让人以为配置生效了。
func TestMissingFileNotCreatedWhenPathGiven(t *testing.T) {
	if os.Getenv("AIUDGE_TEST_CHILD") == "1" {
		LoadFile(filepath.Join(os.Getenv("AIUDGE_TEST_DIR"), "nope.conf"), true)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestMissingFileNotCreatedWhenPathGiven")
	cmd.Env = append(os.Environ(), "AIUDGE_TEST_CHILD=1", "AIUDGE_TEST_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("不存在的配置文件应当退出，实际正常返回: %s", out)
	}
	if !contains(string(out), "读取配置文件") {
		t.Errorf("应报出读取失败，得到 %s", out)
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

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		if r != '\r' {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
