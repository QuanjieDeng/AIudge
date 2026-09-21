package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aiudge/internal/config"
	"aiudge/internal/notify"
	"aiudge/internal/queue"
	"aiudge/internal/review"
	"aiudge/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir: dir, WorkDir: filepath.Join(dir, "repos"),
		Addr: ":0", BaseURL: "http://test", GatewayURL: "http://gw",
		Concurrency: 1, MaxRetry: 0,
	}
	db, err := store.Open(filepath.Join(dir, "t.db"), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	feishu := notify.New("", cfg.BaseURL)
	pipe := &review.Pipeline{Cfg: cfg, DB: db}
	q := queue.New(cfg, db, pipe, feishu)
	return New(cfg, db, q, q.NewScheduler(), feishu)
}

// fillPath 把 {id} 之类的路径参数换成具体值，好在测试里发请求。
func fillPath(p string) string {
	r := strings.NewReplacer("{id}", "1")
	return r.Replace(p)
}

// TestEveryRouteRequiresAuth 是这张路由表存在的理由。
//
// 真实事故：一次批量替换把某条路由外层的登录校验整个抹掉了，接口对未登录请求
// 直接空指针崩溃（而且崩溃前已经进了业务逻辑）。靠人肉 grep 检查几十条路由的
// 包裹层，注定会漏；遍历同一张表发一次请求就全查出来了。
func TestEveryRouteRequiresAuth(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	for _, rt := range srv.routes() {
		if rt.access == accessPublic {
			continue
		}
		req := httptest.NewRequest(rt.method, fillPath(rt.pattern), strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未登录时应返回 401，实际 %d %s",
				rt.method, rt.pattern, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestAdminRoutesRejectNonAdmin 全局管理员专属接口不能被普通用户碰到。
func TestAdminRoutesRejectNonAdmin(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	// 造一个普通用户（密码为空即可，这里只关心角色）
	u, err := srv.db.UpsertUser("ou_user", "普通用户", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.db.SetPassword(u.ID, ""); err != nil {
		t.Fatal(err)
	}
	tok := "test-session-token"
	if err := srv.db.CreateSession(tok, u.ID, "2099-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	for _, rt := range srv.routes() {
		if rt.access != accessAdmin {
			continue
		}
		req := httptest.NewRequest(rt.method, fillPath(rt.pattern), strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s 普通用户应被拒（403），实际 %d", rt.method, rt.pattern, rec.Code)
		}
	}
}

// TestDefaultPasswordDoesNotBlock 用默认密码登录后，业务接口照常可用。
//
// 这里曾经有一道强制改密的关卡（428），结果是人一进来先撞一堵墙：能登录、能看见界面，
// 点哪儿都提示「请先修改默认密码」。改密现在只是一个普通功能，不再拦人。
func TestDefaultPasswordDoesNotBlock(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	admin, err := srv.db.UserByUsername(store.DefaultAdminUsername)
	if err != nil {
		t.Fatalf("内置管理员应已种下: %v", err)
	}
	tok := "no-gate-token"
	if err := srv.db.CreateSession(tok, admin.ID, "2099-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, p := range []string{"/api/me", "/api/repos", "/api/projects", "/api/model-config"} {
		if code := do("GET", p); code != http.StatusOK {
			t.Errorf("默认密码不该拦住 %s，实际 %d", p, code)
		}
	}
}

// TestPasswordLoginFlow 账号密码登录：默认密码能进、错密码被拒、账号不存在不泄漏差异。
func TestPasswordLoginFlow(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	login := func(user, pass string) (int, string) {
		body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		req := httptest.NewRequest("POST", "/api/auth/password", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}

	if code, _ := login("admin", store.DefaultAdminPassword); code != 200 {
		t.Errorf("默认密码应能登录，实际 %d", code)
	}
	c1, msg1 := login("admin", "wrong")
	c2, msg2 := login("nonexistent", "wrong")
	if c1 != 401 || c2 != 401 {
		t.Errorf("错误凭据应返回 401，实际 %d / %d", c1, c2)
	}
	if msg1 != msg2 {
		t.Errorf("账号不存在与密码错误必须返回同一句话，避免暴露账号是否存在：\n %s\n %s", msg1, msg2)
	}
}
