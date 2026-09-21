package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"aiudge/internal/store"
)

// loginAs 建一个已改密的用户并返回可直接用的会话 cookie。
func loginAs(t *testing.T, srv *Server, feishuID, name, role string) (int64, *http.Cookie) {
	t.Helper()
	u, err := srv.db.UpsertUser(feishuID, name, "")
	if err != nil {
		t.Fatal(err)
	}
	if role != "" {
		if err := srv.db.SetUserRole(u.ID, role); err != nil {
			t.Fatal(err)
		}
	}
	tok := "tok-" + feishuID
	if err := srv.db.CreateSession(tok, u.ID, "2099-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	return u.ID, &http.Cookie{Name: sessionCookie, Value: tok}
}

func do(h http.Handler, method, path string, ck *http.Cookie, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestProjectCreatorBecomesAdmin 建项目的人默认就是该项目的项目管理员。
// 否则项目建完在成员列表里是空的，看不出谁管这个项目。
func TestProjectCreatorBecomesAdmin(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()
	uid, ck := loginAs(t, srv, "ou_boss", "老板", "admin")

	rec := do(h, "POST", "/api/projects", ck, `{"name":"我的项目"}`)
	if rec.Code != 200 {
		t.Fatalf("建项目失败: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if role := srv.db.ProjectRoleOf(uid, created.ID); role != store.RoleProjectAdmin {
		t.Errorf("创建者应自动成为项目管理员，得到 %q", role)
	}
	if !srv.db.CanAdminProject(mustUser(t, srv, uid), created.ID) {
		t.Error("创建者应能改该项目配置")
	}
}

// TestGlobalConfigNeedsGlobalAdmin 全局配置类接口只给全局管理员。
// 项目管理员也不行——模型网关、通知、Prompt 是全平台共用的。
func TestGlobalConfigNeedsGlobalAdmin(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	bossID, _ := loginAs(t, srv, "ou_boss", "老板", "admin")
	pid, err := srv.db.CreateProject("P", "")
	if err != nil {
		t.Fatal(err)
	}
	// 项目管理员：能管项目，但读不到全局配置
	paID, paCK := loginAs(t, srv, "ou_pa", "项目管理员", "user")
	if err := srv.db.UpsertProjectMember(pid, paID, store.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	_ = bossID

	for _, path := range []string{
		"/api/model-config", "/api/notify-config", "/api/prompt-config",
		"/api/users", "/api/identities", "/api/audit",
	} {
		if rec := do(h, "GET", path, paCK, ""); rec.Code != http.StatusForbidden {
			t.Errorf("项目管理员访问 %s 应被拒（403），实际 %d", path, rec.Code)
		}
	}
}

// TestAssignCandidatesAreProjectMembers 指派只能选本项目成员——
// 指派给一个看不到这个项目的人没有意义。
func TestAssignCandidatesAreProjectMembers(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	aID, err := srv.db.CreateProject("A", "")
	if err != nil {
		t.Fatal(err)
	}
	bID, err := srv.db.CreateProject("B", "")
	if err != nil {
		t.Fatal(err)
	}
	_, ckA := loginAs(t, srv, "ou_a", "A 的人", "user")
	inB, _ := loginAs(t, srv, "ou_b", "B 的人", "user")

	// A 的人在项目 A，B 的人在项目 B
	if err := srv.db.UpsertProjectMember(aID, mustUserID(t, srv, "ou_a"), store.RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.UpsertProjectMember(bID, inB, store.RoleMember); err != nil {
		t.Fatal(err)
	}

	rec := do(h, "GET", "/api/projects/"+strconv.FormatInt(aID, 10)+"/members", ckA, "")
	if rec.Code != 200 {
		t.Fatalf("应能看自己项目的成员: %d", rec.Code)
	}
	var members []store.ProjectMember
	if err := json.Unmarshal(rec.Body.Bytes(), &members); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == inB {
			t.Error("不该看到别的项目的成员")
		}
	}
	// 看不见别人项目的成员列表
	if rec := do(h, "GET", "/api/projects/"+strconv.FormatInt(bID, 10)+"/members", ckA, ""); rec.Code != http.StatusForbidden {
		t.Errorf("访问别人项目的成员应被拒，实际 %d", rec.Code)
	}
}

func mustUser(t *testing.T, srv *Server, id int64) *store.User {
	t.Helper()
	u, err := srv.db.UserByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustUserID(t *testing.T, srv *Server, feishuID string) int64 {
	t.Helper()
	u, err := srv.db.UserByFeishu(feishuID)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// TestOrdinaryUserCanCreateProject 普通用户也能建项目，建完就是该项目的项目管理员。
//
// 这条路由原来是 accessAdmin，能力被路由表挡住，但 handleProjectCreate 里
// 「创建者自动成为项目管理员」早就写好了。上面那条 TestProjectCreatorBecomesAdmin
// 用的是全局管理员，覆盖不到放开之后的这段。
func TestOrdinaryUserCanCreateProject(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()
	uid, ck := loginAs(t, srv, "ou_plain", "普通用户", "user")

	rec := do(h, "POST", "/api/projects", ck, `{"name":"我自己建的"}`)
	if rec.Code != 200 {
		t.Fatalf("普通用户建项目应成功，实际 %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if role := srv.db.ProjectRoleOf(uid, created.ID); role != store.RoleProjectAdmin {
		t.Errorf("创建者应自动成为项目管理员，得到 %q", role)
	}

	// 建完只该看见自己这一个项目——ListProjects 也按可见性过滤
	var list []store.Project
	rec = do(h, "GET", "/api/projects", ck, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Errorf("普通用户应只看到自己建的项目，得到 %+v", list)
	}
}

// TestProjectWriteNeedsProjectAdmin 改/删项目要项目级权限：
// 不是成员的人换个 id 就能改别人项目的话，项目这层隔离就白做了。
func TestProjectWriteNeedsProjectAdmin(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	pid, err := srv.db.CreateProject("别人的项目", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := srv.db.CreateProject("我的项目", "")
	if err != nil {
		t.Fatal(err)
	}
	// 一个普通用户，只在「我的项目」里
	uid, ck := loginAs(t, srv, "ou_plain", "普通用户", "user")
	if err := srv.db.UpsertProjectMember(other, uid, store.RoleProjectAdmin); err != nil {
		t.Fatal(err)
	}
	pidStr := strconv.FormatInt(pid, 10)

	if rec := do(h, "PUT", "/api/projects/"+pidStr, ck, `{"name":"改了","enabled":true}`); rec.Code != http.StatusForbidden {
		t.Errorf("非成员改别人的项目应被拒（403），实际 %d", rec.Code)
	}
	if rec := do(h, "DELETE", "/api/projects/"+pidStr, ck, ""); rec.Code != http.StatusForbidden {
		t.Errorf("非成员删别人的项目应被拒（403），实际 %d", rec.Code)
	}
	// 自己管的那个项目要能改——别把校验做成谁都动不了
	if rec := do(h, "PUT", "/api/projects/"+strconv.FormatInt(other, 10), ck, `{"name":"改名了","enabled":true}`); rec.Code != 200 {
		t.Errorf("项目管理员应能改自己的项目，实际 %d %s", rec.Code, rec.Body)
	}
}

// TestProjectScopeCannotBeWidened 是本次改动唯一的安全面。
//
// project_id 是用户可控的查询参数，若直接拿它当过滤谓词（而不是与可见集求交集），
// 任何人改一下 URL 就能读到别的项目的数据。这里用「在 A 的人请求 B」来钉死它：
// 必须返回空，而不是 B 的数据。
func TestProjectScopeCannotBeWidened(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	aID, err := srv.db.CreateProject("A", "")
	if err != nil {
		t.Fatal(err)
	}
	bID, err := srv.db.CreateProject("B", "")
	if err != nil {
		t.Fatal(err)
	}
	repoA, err := srv.db.CreateRepo(store.RepoInput{Name: "ra", GitURL: "u", ProjectID: aID})
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := srv.db.CreateRepo(store.RepoInput{Name: "rb", GitURL: "u", ProjectID: bID})
	if err != nil {
		t.Fatal(err)
	}
	seed := func(repoID int64, fp string) {
		t.Helper()
		if _, err := srv.db.UpsertIssue(&store.Issue{
			Fingerprint: fp, RepoID: repoID, Source: store.SourceAI,
			Category: store.CategoryLogic, Severity: store.SeverityHigh,
			FilePath: "a.go", LineStart: 1, LineEnd: 1, Title: "t",
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed(repoA, "fa")
	seed(repoB, "fb")

	uid, ck := loginAs(t, srv, "ou_in_a", "A 的人", "user")
	if err := srv.db.UpsertProjectMember(aID, uid, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	bStr := strconv.FormatInt(bID, 10)

	// 问题列表：带 B 的 id 必须一条都没有
	var issues struct {
		Items []store.Issue `json:"items"`
		Total int           `json:"total"`
	}
	rec := do(h, "GET", "/api/issues?project_id="+bStr, ck, "")
	if rec.Code != 200 {
		t.Fatalf("应正常返回 200，实际 %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &issues); err != nil {
		t.Fatal(err)
	}
	if issues.Total != 0 || len(issues.Items) != 0 {
		t.Errorf("越权带 project_id=%s 不该看到 B 的问题，得到 total=%d", bStr, issues.Total)
	}

	// 概览同理：B 有一条问题，越权读到的必须是 0 而不是 1
	var ov store.Overview
	rec = do(h, "GET", "/api/stats/overview?project_id="+bStr, ck, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.NewCount != 0 {
		t.Errorf("越权带 project_id=%s 不该看到 B 的统计，得到 new_count=%d", bStr, ov.NewCount)
	}

	// 反向确认：正常按自己的项目过滤要能看到自己的那条，
	// 否则上面测的只是「全都查不到」，为空是坏掉而不是隔离
	rec = do(h, "GET", "/api/stats/overview?project_id="+strconv.FormatInt(aID, 10), ck, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatal(err)
	}
	if ov.NewCount != 1 {
		t.Errorf("按自己的项目过滤应看到 1 条，得到 %d", ov.NewCount)
	}
	// 不带 project_id 时也只看得到自己项目的
	rec = do(h, "GET", "/api/issues", ck, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &issues); err != nil {
		t.Fatal(err)
	}
	if issues.Total != 1 {
		t.Errorf("不带 project_id 时应看到自己项目的 1 条，得到 %d", issues.Total)
	}
}
