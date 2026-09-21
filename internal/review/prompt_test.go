package review

import (
	"strings"
	"testing"
)

// TestRenderPromptPercentIsLiteral 是具名占位符存在的理由：prompt 是人手写的，
// 随手写个「错误率 % 上升」就会被 fmt.Sprintf 当成格式动词，输出成「%! 上升」，
// 而且不会报错，只是悄悄改掉了发给模型的内容。
func TestRenderPromptPercentIsLiteral(t *testing.T) {
	got := RenderPrompt("仓库 {{repo}}，错误率 % 上升，100%% 确定，{{language}} 代码", "go", "svc")
	want := "仓库 svc，错误率 % 上升，100%% 确定，Go 代码"
	if got != want {
		t.Errorf("\n got = %q\nwant = %q", got, want)
	}
	if strings.Contains(got, "%!") {
		t.Errorf("百分号被当成格式动词吃掉了: %q", got)
	}
}

// TestRenderPromptPlaceholders 占位符替换与缺省值。
func TestRenderPromptPlaceholders(t *testing.T) {
	cases := []struct{ tmpl, lang, repo, want string }{
		{"审 {{language}} / {{repo}}", "go", "svc", "审 Go / svc"},
		{"审 {{language}}", "cpp", "svc", "审 C++"},
		{"{{repo}}", "go", "", "本项目"},             // 仓库名空时兜底
		{"没有占位符", "go", "svc", "没有占位符"},           // 用户删光了也照跑
		{"{{不存在的变量}}", "go", "svc", "{{不存在的变量}}"}, // 未定义的原样保留，不静默吞掉
	}
	for _, c := range cases {
		if got := RenderPrompt(c.tmpl, c.lang, c.repo); got != c.want {
			t.Errorf("RenderPrompt(%q) = %q，期望 %q", c.tmpl, got, c.want)
		}
	}
}

// TestPromptVersionTracksContent 版本号跟着内容走。
// 手填的版本必然漂移——改了 prompt 忘改版本，历史曲线就解释不清了（§4.6 要防的正是这个）。
func TestPromptVersionTracksContent(t *testing.T) {
	a := PromptVersion("这是 prompt A")
	if a != PromptVersion("这是 prompt A") {
		t.Error("同样内容必须得到同样版本号")
	}
	if a == PromptVersion("这是 prompt B") {
		t.Error("内容变了版本号必须变")
	}
	if a == PromptVersion("这是 prompt A ") {
		t.Error("哪怕只多一个空格也算一次变更，宁多标不漏标")
	}
	if len(a) != 12 {
		t.Errorf("版本号应固定 12 位 hex，得到 %q", a)
	}
}

// TestEffectivePromptFallback 没自定义就用内置默认，
// 这样升级二进制时默认 prompt 的改进能自动生效。
func TestEffectivePromptFallback(t *testing.T) {
	if EffectivePrompt("") != DefaultSystemPrompt() {
		t.Error("空配置应回落到内置默认")
	}
	// 只存了空白，界面上等于没填，也按未自定义处理
	if EffectivePrompt("   \n\t ") != DefaultSystemPrompt() {
		t.Error("纯空白应回落到内置默认")
	}
	if EffectivePrompt("我自己写的") != "我自己写的" {
		t.Error("自定义内容应原样生效")
	}
}

// TestDefaultPromptHasPlaceholders 默认 prompt 必须带占位符，
// 否则换个语言或仓库名，提示词里还是写死的那一套。
func TestDefaultPromptHasPlaceholders(t *testing.T) {
	d := DefaultSystemPrompt()
	for _, ph := range []string{phLanguage, phRepo} {
		if !strings.Contains(d, ph) {
			t.Errorf("默认 prompt 缺少占位符 %s", ph)
		}
	}
	// 内置默认不该再写死某个具体项目
	for _, banned := range []string{"SLG", "游戏服务端", "战斗结算"} {
		if strings.Contains(d, banned) {
			t.Errorf("默认 prompt 不该写死项目特征 %q", banned)
		}
	}
}

// TestBuildSystemAppendsDocs 挂载的规范文档要拼在模板后面，且只在有文档时出现。
func TestBuildSystemAppendsDocs(t *testing.T) {
	bare := buildSystem(DefaultSystemPrompt(), "go", "svc", nil)
	if strings.Contains(bare, "专属审核规范") {
		t.Error("没有挂载文档时不该出现规范段落")
	}
	withDocs := buildSystem(DefaultSystemPrompt(), "go", "svc", map[string]string{"CLAUDE.md": "禁止裸奔"})
	if !strings.Contains(withDocs, "专属审核规范") || !strings.Contains(withDocs, "禁止裸奔") {
		t.Error("挂载的文档内容应拼进系统提示")
	}
}
