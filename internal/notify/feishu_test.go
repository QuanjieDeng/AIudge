package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aiudge/internal/store"
)

// TestHookFallsBackToDefault 收件地址的解析顺序：仓库自己的优先，没配用全局兜底。
func TestHookFallsBackToDefault(t *testing.T) {
	f := &Feishu{Default: "https://global.example/hook/g1"}
	if got := f.hook("https://repo.example/hook/r1"); got != "https://repo.example/hook/r1" {
		t.Errorf("仓库配了就该用仓库的，得到 %s", got)
	}
	if got := f.hook(""); got != "https://global.example/hook/g1" {
		t.Errorf("仓库没配应回落到全局，得到 %s", got)
	}
	// 全局也没配时不该 panic，也不该发出去
	empty := &Feishu{}
	if got := empty.hook(""); got != "" {
		t.Errorf("都没有时应返回空串，得到 %s", got)
	}
}

// TestSendRoutesToRepoHook 真正发出去的是仓库那个地址，而不是全局那个。
func TestSendRoutesToRepoHook(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["msg_type"] != "text" {
			t.Errorf("消息体格式不对: %+v", body)
		}
	}))
	defer srv.Close()

	f := New("https://global.invalid/hook/never", "http://ui")
	if err := f.Send(context.Background(), srv.URL+"/hook/repo1", "hello"); err != nil {
		t.Fatal(err)
	}
	if hit != "/hook/repo1" {
		t.Errorf("应发到仓库自己的地址，实际打到 %s", hit)
	}
}

// TestSendSilentWhenNoHook 两个地址都没配时静默跳过，不报错——
// 调用方遍布各处，不该每处都先判断一遍有没有配。
func TestSendSilentWhenNoHook(t *testing.T) {
	f := New("", "http://ui")
	if err := f.Send(context.Background(), "", "x"); err != nil {
		t.Errorf("没配应当静默跳过，得到 %v", err)
	}
}

// TestHighIssueCarriesRepoHook 高危推送带上仓库地址与 @ 人。
func TestHighIssueCarriesRepoHook(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
	}))
	defer srv.Close()

	f := New("", "http://ui")
	iss := &store.Issue{ID: 7, FilePath: "a/b.go", LineStart: 3, LineEnd: 5,
		Category: store.CategorySecurity, Severity: store.SeverityHigh, Title: "除零"}
	if err := f.HighIssue(context.Background(), srv.URL+"/hook/x", "svc", iss, "ou_abc"); err != nil {
		t.Fatal(err)
	}
	if hit != "/hook/x" {
		t.Errorf("应发到仓库自己的地址，实际打到 %s", hit)
	}
}
