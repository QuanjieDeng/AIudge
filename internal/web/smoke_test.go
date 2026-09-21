package web

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestFrontendSmoke 跑一份前端冒烟检查（webui/smoke.mjs）。
//
// 前端逻辑没法用 Go 测，但有些 bug 又只在前端才暴露——比如登录页那个
// 「密码错了看起来没反应」：api() 里的 401 拦截器把整页重绘了，错误信息写进
// 已经被丢弃的节点，浏览器里看着就是「点了没动静」，后端日志一切正常。
// 单独一个进程跑真实的 app.js，能挡住这类回归。
func TestFrontendSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没装 node，跳过前端冒烟检查")
	}
	script := filepath.Join("..", "..", "webui", "smoke.mjs")
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("前端冒烟检查失败:\n%s", out)
	}
	t.Logf("%s", out)
}
