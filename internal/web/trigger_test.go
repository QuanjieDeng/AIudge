package web

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"aiudge/internal/store"
)

// TestManualTriggerNeedsPermission 手动触发是**花钱**的接口（一次真实的大模型审核），
// 所以「看得见这个仓库」不够，必须项目管理员或被单独授予 manual_trigger 的成员。
//
// 这条权限以前不存在（任何项目成员都能调），加权限的时候最容易漏掉的就是
// 「没权限时到底拦住没有」——拦住的是 403，不是 200。
func TestManualTriggerNeedsPermission(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	_, ckAdmin := loginAs(t, srv, "ou_boss", "老板", "admin")
	rec := do(h, "POST", "/api/projects", ckAdmin, `{"name":"P1"}`)
	if rec.Code != 200 {
		t.Fatalf("建项目失败: %d %s", rec.Code, rec.Body)
	}
	var pj struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pj)

	rec = do(h, "POST", "/api/repos", ckAdmin,
		`{"project_id":`+itoa(pj.ID)+`,"name":"r1","git_url":"http://git.example/r1.git","enabled":true}`)
	if rec.Code != 200 {
		t.Fatalf("建仓库失败: %d %s", rec.Code, rec.Body)
	}
	var repo struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &repo)

	// 成员：在项目里，但没有任何额外权限
	memberID, ckMember := loginAs(t, srv, "ou_dev", "小王", "user")
	if rec := do(h, "POST", "/api/projects/"+itoa(pj.ID)+"/members", ckAdmin,
		`{"user_id":`+itoa(memberID)+`,"role":"member"}`); rec.Code != 200 {
		t.Fatalf("加成员失败: %d %s", rec.Code, rec.Body)
	}
	// 局外人：不在项目里
	_, ckOut := loginAs(t, srv, "ou_out", "路人", "user")

	body := `{"time_from":"2026-09-01T00:00","time_to":"2026-09-02T00:00"}`
	if rec := do(h, "POST", "/api/repos/"+itoa(repo.ID)+"/trigger", ckMember, body); rec.Code != 403 {
		t.Errorf("没授权的成员应被拦住（403），实际 %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/api/repos/"+itoa(repo.ID)+"/trigger", ckOut, body); rec.Code != 403 {
		t.Errorf("非项目成员应被拦住（403），实际 %d %s", rec.Code, rec.Body)
	}

	// 授权后再来一次，这次要过，并且时间范围要真的落进任务里
	rec = do(h, "POST", "/api/projects/"+itoa(pj.ID)+"/members", ckAdmin,
		`{"user_id":`+itoa(memberID)+`,"role":"member","permissions":["`+store.PermManualTrigger+`"]}`)
	if rec.Code != 200 {
		t.Fatalf("授权失败: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, "POST", "/api/repos/"+itoa(repo.ID)+"/trigger", ckMember, body)
	if rec.Code != 200 {
		t.Fatalf("授权后应能触发，实际 %d %s", rec.Code, rec.Body)
	}
	var out struct {
		TaskID int64 `json:"task_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	task, err := srv.db.GetTask(out.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.TriggerType != "manual" {
		t.Errorf("触发类型应为 manual，得到 %q", task.TriggerType)
	}
	// 界面填的是本地时间、库里存的是 UTC，所以按本地时区还原回来比对：
	// 直接比字符串会在非 UTC 的机器上假失败（这里是 UTC+8）
	for _, c := range []struct{ got, want string }{
		{task.TimeFrom, "2026-09-01T00:00"},
		{task.TimeTo, "2026-09-02T00:00"},
	} {
		tm, err := time.ParseInLocation(store.TimeFmt, c.got, time.UTC)
		if err != nil {
			t.Fatalf("时间没落库或格式不对: %q", c.got)
		}
		if local := tm.Local().Format("2006-01-02T15:04"); local != c.want {
			t.Errorf("时间范围落库后还原为 %s，期望 %s（本地时区换算错了？）", local, c.want)
		}
	}

	// 两类范围同时给要说清楚是错的，不能含糊地按其中一边审
	rec = do(h, "POST", "/api/repos/"+itoa(repo.ID)+"/trigger", ckMember,
		`{"from":"abc","time_from":"2026-09-01T00:00"}`)
	if rec.Code != 400 {
		t.Errorf("提交范围与时间范围同时给应报错（400），实际 %d %s", rec.Code, rec.Body)
	}
}

// TestMemberPermsToggleKeepsRole 勾权限不该顺手把角色改掉：接口是覆盖式的，
// 只发权限字段时角色也必须原样不动。
func TestMemberPermsToggleKeepsRole(t *testing.T) {
	srv := testServer(t)
	h := srv.Handler()

	_, ckAdmin := loginAs(t, srv, "ou_boss", "老板", "admin")
	rec := do(h, "POST", "/api/projects", ckAdmin, `{"name":"P1"}`)
	var pj struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pj)

	uid, _ := loginAs(t, srv, "ou_pa", "老李", "user")
	if rec := do(h, "POST", "/api/projects/"+itoa(pj.ID)+"/members", ckAdmin,
		`{"user_id":`+itoa(uid)+`,"role":"project_admin","permissions":[]}`); rec.Code != 200 {
		t.Fatalf("加成员失败: %d %s", rec.Code, rec.Body)
	}
	if role := srv.db.ProjectRoleOf(uid, pj.ID); role != store.RoleProjectAdmin {
		t.Errorf("角色应为 project_admin，得到 %q", role)
	}

	// 再发一次只带 permissions 的请求（界面勾选就是这么发的）
	if rec := do(h, "POST", "/api/projects/"+itoa(pj.ID)+"/members", ckAdmin,
		`{"user_id":`+itoa(uid)+`,"role":"project_admin","permissions":["`+store.PermManualTrigger+`"]}`); rec.Code != 200 {
		t.Fatalf("改权限失败: %d %s", rec.Code, rec.Body)
	}
	if !srv.db.MemberHasPerm(uid, pj.ID, store.PermManualTrigger) {
		t.Error("权限应已生效")
	}
	if role := srv.db.ProjectRoleOf(uid, pj.ID); role != store.RoleProjectAdmin {
		t.Errorf("改权限不该动角色，得到 %q", role)
	}

	// 未知权限不能悄悄存进去
	if rec := do(h, "POST", "/api/projects/"+itoa(pj.ID)+"/members", ckAdmin,
		`{"user_id":`+itoa(uid)+`,"role":"project_admin","permissions":["root"]}`); rec.Code == 200 {
		t.Error("未知权限应被拒绝")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
