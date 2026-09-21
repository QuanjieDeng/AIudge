// Package store 是 SQLite 持久层（§附录决策 7：关系库起步，规模仅数千条/月）。
// 时间统一存 UTC 的 "2006-01-02 15:04:05" 字符串，可直接做字典序比较与 strftime 分桶。
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const TimeFmt = "2006-01-02 15:04:05"

func Now() string { return time.Now().UTC().Format(TimeFmt) }

func ParseTime(s string) time.Time {
	t, err := time.Parse(TimeFmt, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

type DB struct {
	*sql.DB
	key []byte // 凭据加密主密钥，只在本层用，不出现在任何返回值里
}

func Open(path string, key []byte) (*DB, error) {
	sqldb, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// SQLite 单写者：连接数放开会让并发写直接撞 SQLITE_BUSY，串行化更省事
	sqldb.SetMaxOpenConns(1)
	d := &DB{DB: sqldb, key: key}
	if err := d.migrate(); err != nil {
		return nil, err
	}
	return d, nil
}

const schema = `
-- 项目层：仓库归属项目，用户按项目分权。团队同时服务多个项目时，
-- 分类、成员、可见范围都得跟着项目走。
CREATE TABLE IF NOT EXISTS projects (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

-- 项目成员及其在项目内的角色（project_admin | member）。
-- 全局管理员不在这张表里，靠 users.role='admin' 判定。
-- perms 是这个成员**额外**拿到的项目级权限（JSON 数组，键见 PermissionList），
-- 角色之外的授权走这里，不再往 role 枚举里塞值——权限是会长的，角色不该跟着长。
CREATE TABLE IF NOT EXISTS project_members (
  project_id INTEGER NOT NULL,
  user_id    INTEGER NOT NULL,
  role       TEXT NOT NULL DEFAULT 'member',
  perms      TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members(user_id);

CREATE TABLE IF NOT EXISTS repositories (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id     INTEGER NOT NULL DEFAULT 0,
  name           TEXT NOT NULL UNIQUE,
  git_url        TEXT NOT NULL,
  branch         TEXT NOT NULL DEFAULT 'master',
  credential_enc TEXT NOT NULL DEFAULT '',
  webhook_enc    TEXT NOT NULL DEFAULT '',
  team           TEXT NOT NULL DEFAULT '',
  language       TEXT NOT NULL DEFAULT 'go',
  include_globs  TEXT NOT NULL DEFAULT '',
  exclude_globs  TEXT NOT NULL DEFAULT '*.pb.go',
  spec_docs      TEXT NOT NULL DEFAULT '[]',
  trigger_webhook INTEGER NOT NULL DEFAULT 1,
  trigger_cron   TEXT NOT NULL DEFAULT '',
  default_model  TEXT NOT NULL DEFAULT '',
  enabled        INTEGER NOT NULL DEFAULT 1,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_repos_project ON repositories(project_id);

CREATE TABLE IF NOT EXISTS model_configs (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  gateway_url    TEXT NOT NULL DEFAULT '',
  gateway_key_enc TEXT NOT NULL DEFAULT '',
  timeout_sec    INTEGER NOT NULL DEFAULT 600,
  models         TEXT NOT NULL DEFAULT '[]',
  default_model  TEXT NOT NULL DEFAULT '',
  updated_at     TEXT NOT NULL DEFAULT ''
);

-- 通知的全局配置。webhook 放这里而不是只靠配置文件：
-- 仓库级的 webhook 能在界面上改，全局兜底却要改文件重启，用起来不对称。
-- 配置文件里那份仍然作为更深一层的兜底。
CREATE TABLE IF NOT EXISTS notify_configs (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  webhook_enc  TEXT NOT NULL DEFAULT '',
  summary_cron TEXT NOT NULL DEFAULT '',
  updated_at   TEXT NOT NULL DEFAULT ''
);

-- 审核 prompt 的单行配置表。system_prompt 为空表示「用内置默认」，
-- 这样升级二进制时默认 prompt 的改进能自动生效，只有真的自定义过才需要人维护。
CREATE TABLE IF NOT EXISTS review_configs (
  id            INTEGER PRIMARY KEY CHECK (id = 1),
  system_prompt TEXT NOT NULL DEFAULT '',
  updated_at    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS review_tasks (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_id        INTEGER NOT NULL,
  trigger_type   TEXT NOT NULL,
  commit_from    TEXT NOT NULL DEFAULT '',
  commit_to      TEXT NOT NULL DEFAULT '',
  -- 手动触发的另一种口径：按时间范围选提交。时间范围只能在管线里解析成提交
  -- （那里刚 fetch 过），所以在 handler 里就换算成 SHA 会拿上次审核留下的旧副本去数。
  time_from      TEXT NOT NULL DEFAULT '',
  time_to        TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT 'queued',
  retry_count    INTEGER NOT NULL DEFAULT 0,
  model_name     TEXT NOT NULL DEFAULT '',
  model_version  TEXT NOT NULL DEFAULT '',
  prompt_version TEXT NOT NULL DEFAULT '',
  spec_version   TEXT NOT NULL DEFAULT '',
  error          TEXT NOT NULL DEFAULT '',
  soft_warned    INTEGER NOT NULL DEFAULT 0,
  queued_at      TEXT NOT NULL,
  started_at     TEXT NOT NULL DEFAULT '',
  finished_at    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tasks_repo ON review_tasks(repo_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON review_tasks(status);

CREATE TABLE IF NOT EXISTS task_logs (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id INTEGER NOT NULL,
  ts      TEXT NOT NULL,
  level   TEXT NOT NULL DEFAULT 'info',
  message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_logs_task ON task_logs(task_id, id);

CREATE TABLE IF NOT EXISTS review_reports (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id        INTEGER NOT NULL,
  repo_id        INTEGER NOT NULL,
  commits        TEXT NOT NULL DEFAULT '[]',
  files          TEXT NOT NULL DEFAULT '[]',
  issue_count    INTEGER NOT NULL DEFAULT 0,
  high_count     INTEGER NOT NULL DEFAULT 0,
  model_name     TEXT NOT NULL DEFAULT '',
  model_version  TEXT NOT NULL DEFAULT '',
  prompt_version TEXT NOT NULL DEFAULT '',
  spec_version   TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reports_repo ON review_reports(repo_id, id DESC);

-- report_issues 记录「某份报告本次发现了哪些问题」。不靠 issues.report_id 反查：
-- 同一问题在后续报告里再次命中会把 report_id 覆盖成最新的那份，旧报告的详情页
-- 就会变空，报告作为历史快照就不成立了。
CREATE TABLE IF NOT EXISTS report_issues (
  report_id INTEGER NOT NULL,
  issue_id  INTEGER NOT NULL,
  PRIMARY KEY (report_id, issue_id)
);
CREATE INDEX IF NOT EXISTS idx_report_issues ON report_issues(issue_id);

CREATE TABLE IF NOT EXISTS issues (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  fingerprint  TEXT NOT NULL,
  repo_id      INTEGER NOT NULL,
  report_id    INTEGER NOT NULL DEFAULT 0,
  source       TEXT NOT NULL,
  category     TEXT NOT NULL,
  severity     TEXT NOT NULL,
  file_path    TEXT NOT NULL,
  line_start   INTEGER NOT NULL DEFAULT 0,
  line_end     INTEGER NOT NULL DEFAULT 0,
  title        TEXT NOT NULL DEFAULT '',
  description  TEXT NOT NULL DEFAULT '',
  suggestion   TEXT NOT NULL DEFAULT '',
  code_snippet TEXT NOT NULL DEFAULT '',
  linter       TEXT NOT NULL DEFAULT '',
  commit_sha   TEXT NOT NULL DEFAULT '',
  committer    TEXT NOT NULL DEFAULT '',
  committer_id INTEGER NOT NULL DEFAULT 0,
  assignee_id  INTEGER NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'pending',
  regression   INTEGER NOT NULL DEFAULT 0,
  regression_at TEXT NOT NULL DEFAULT '',
  first_seen_at TEXT NOT NULL,
  last_seen_at  TEXT NOT NULL,
  seen_count    INTEGER NOT NULL DEFAULT 1,
  closed_at     TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_issues_fp ON issues(fingerprint);
CREATE INDEX IF NOT EXISTS idx_issues_status ON issues(status, severity);
CREATE INDEX IF NOT EXISTS idx_issues_repo ON issues(repo_id, id DESC);

CREATE TABLE IF NOT EXISTS comments (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_id   INTEGER NOT NULL,
  user_id    INTEGER NOT NULL DEFAULT 0,
  user_name  TEXT NOT NULL DEFAULT '',
  content    TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_comments_issue ON comments(issue_id, id);

CREATE TABLE IF NOT EXISTS users (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  feishu_user_id TEXT NOT NULL UNIQUE,
  -- username 只给内置管理员用（账号密码登录），飞书用户这里为空
  username       TEXT NOT NULL DEFAULT '',
  password_hash  TEXT NOT NULL DEFAULT '',
  name           TEXT NOT NULL DEFAULT '',
  role           TEXT NOT NULL DEFAULT 'user',
  avatar         TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  last_login_at  TEXT NOT NULL DEFAULT ''
);
-- 只索引真正有用户名的行：飞书用户这一列为空串，UNIQUE 会把它们全撞在一起
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_username ON users(username) WHERE username <> '';

CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS committer_identities (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  git_name  TEXT NOT NULL UNIQUE,
  user_id   INTEGER NOT NULL DEFAULT 0,
  is_bot    INTEGER NOT NULL DEFAULT 0,
  note      TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS review_marks (
  repo_id     INTEGER NOT NULL,
  sha         TEXT NOT NULL,
  task_id     INTEGER NOT NULL DEFAULT 0,
  reviewed_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, sha)
);

-- 子系统规则按仓库隔离：不同的项目有自己的目录结构和业务名词，
-- 全局一张表等于把某一个项目的分类强加给所有仓库。
CREATE TABLE IF NOT EXISTS subsystems (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_id  INTEGER NOT NULL DEFAULT 0,
  pattern  TEXT NOT NULL,
  name     TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 100
);
CREATE INDEX IF NOT EXISTS idx_subsystems_repo ON subsystems(repo_id, priority);

CREATE TABLE IF NOT EXISTS audit_logs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL DEFAULT 0,
  user_name   TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL,
  target_type TEXT NOT NULL DEFAULT '',
  target_id   TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(id DESC);

CREATE TABLE IF NOT EXISTS issue_events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  issue_id   INTEGER NOT NULL,
  from_status TEXT NOT NULL DEFAULT '',
  to_status   TEXT NOT NULL DEFAULT '',
  operator_id INTEGER NOT NULL DEFAULT 0,
  operator   TEXT NOT NULL DEFAULT '',
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_issue ON issue_events(issue_id, id);
`

func (d *DB) migrate() error {
	if _, err := d.Exec(schema); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO model_configs(id, gateway_url, updated_at) VALUES(1, '', ?)`, Now()); err != nil {
		return err
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO review_configs(id, system_prompt, updated_at) VALUES(1, '', ?)`, Now()); err != nil {
		return err
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO notify_configs(id, webhook_enc, summary_cron, updated_at) VALUES(1, '', '', ?)`, Now()); err != nil {
		return err
	}
	return d.seedAdmin()
}

// seedAdmin 是首次启动的初始化：得有一个能进后台的管理员账号，否则新部署没人能登录。
// 只在完全没有可用管理员时创建，重复启动幂等，也不会覆盖已经改过的密码。
//
// 项目**不**自动创建：项目是权限的锚点，建哪个、叫什么、谁管，是部署的人该决定的事，
// 种一个「默认项目」出来只会让第一个进来的人以为那就是他要用的项目。
func (d *DB) seedAdmin() error {
	var admins int
	if err := d.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin' AND username<>''`).Scan(&admins); err != nil {
		return err
	}
	if admins > 0 {
		return nil
	}
	var existing int
	if err := d.QueryRow(`SELECT COUNT(*) FROM users WHERE username='admin'`).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}
	hash, err := HashPassword(DefaultAdminPassword)
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO users(feishu_user_id,username,password_hash,name,role,created_at,last_login_at)
		VALUES('',?,?,'管理员','admin',?,?)`, DefaultAdminUsername, hash, Now(), Now())
	return err
}

// LikeEscape 转义 LIKE 的通配符，避免用户输入的 % 变成模糊匹配。
func LikeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
