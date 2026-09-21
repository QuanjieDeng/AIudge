package store

import (
	"path/filepath"
	"testing"
)

// TestFreshDBSeedsAdminOnly 空库首次启动只种内置管理员：
// 没有管理员就没人能登录。项目不自动建——建哪个、叫什么、谁管由部署的人决定。
func TestFreshDBSeedsAdminOnly(t *testing.T) {
	db := newTestDB(t)

	projects, err := db.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("不该自动建项目，得到 %+v", projects)
	}

	admin, err := db.UserByUsername(DefaultAdminUsername)
	if err != nil {
		t.Fatalf("内置管理员应自动创建: %v", err)
	}
	if admin.Role != "admin" || admin.Username != DefaultAdminUsername {
		t.Errorf("内置管理员应是全局管理员且带 username，得到 %+v", admin)
	}
	if !VerifyPassword(mustHash(t, db, admin.ID), DefaultAdminPassword) {
		t.Error("默认密码校验不过")
	}
}

// TestReopenIsIdempotent 重复启动不能报错，也不能把种子数据再种一遍。
// schema 全是幂等 DDL，每次 Open 都会整段跑一遍。
func TestReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, err := Open(path, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repoID, err := db.CreateRepo(RepoInput{Name: "a", GitURL: "u"})
	if err != nil {
		t.Fatal(err)
	}
	db.ReplaceSubsystems(repoID, []Subsystem{{Pattern: "x", Name: "A 的"}})
	db.Close()

	db2, err := Open(path, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("重复打开不应报错: %v", err)
	}
	defer db2.Close()

	if ps, _ := db2.ListProjects(); len(ps) != 0 {
		t.Errorf("重复启动也不该冒出项目来，得到 %d 个", len(ps))
	}
	if us, _ := db2.ListUsers(); len(us) != 1 {
		t.Errorf("重复启动不该再种一个管理员，得到 %d 个", len(us))
	}
	got, _ := db2.ListSubsystems(repoID)
	if len(got) != 1 || got[0].Name != "A 的" {
		t.Errorf("子系统规则不该被动，得到 %+v", got)
	}
}

func mustHash(t *testing.T, db *DB, userID int64) string {
	t.Helper()
	h, err := db.PasswordHash(userID)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
