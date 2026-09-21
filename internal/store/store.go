package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"aiudge/internal/crypto"
)

// ---------- repositories ----------

const repoCols = `r.id,r.project_id,COALESCE(p.name,''),r.name,r.git_url,r.branch,r.credential_enc,r.webhook_enc,
	r.team,r.language,r.include_globs,r.exclude_globs,
	r.spec_docs,r.trigger_webhook,r.trigger_cron,r.default_model,r.enabled,r.created_at,r.updated_at`

// repoFrom 是 repoCols 依赖的 JOIN，查询仓库时必须带上。
const repoFrom = ` FROM repositories r LEFT JOIN projects p ON p.id=r.project_id`

func (d *DB) scanRepo(s interface{ Scan(...any) error }) (*Repository, error) {
	var r Repository
	var inc, exc, spec, cred, hook string
	var wh, en int
	err := s.Scan(&r.ID, &r.ProjectID, &r.ProjectName, &r.Name, &r.GitURL, &r.Branch, &cred, &hook,
		&r.Team, &r.Language, &inc, &exc,
		&spec, &wh, &r.TriggerCron, &r.DefaultModel, &en, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.IncludeGlobs = splitGlobs(inc)
	r.ExcludeGlobs = splitGlobs(exc)
	r.TriggerWebhook = wh == 1
	r.Enabled = en == 1
	r.HasCredential = cred != ""
	if cred != "" {
		// 解密只为取末 4 位做回显；失败也照样显示 ****，不因为凭据不可读就整个列表报错
		if plain, err := crypto.Decrypt(d.key, cred); err == nil {
			r.CredentialMask = crypto.Mask(plain)
		} else {
			r.CredentialMask = "****"
		}
	}
	r.HasWebhook = hook != ""
	if hook != "" {
		if plain, err := crypto.Decrypt(d.key, hook); err == nil {
			r.WebhookMask = maskWebhook(plain)
		} else {
			r.WebhookMask = "****"
		}
	}
	_ = json.Unmarshal([]byte(spec), &r.SpecDocs)
	if r.SpecDocs == nil {
		r.SpecDocs = []string{}
	}
	return &r, nil
}

// maskWebhook 保留 scheme+host，把路径上的 token 抹掉。
// 只留末四位不够用——群里配了多个机器人时，得能分辨是发往哪个飞书域名的那一个。
func maskWebhook(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "****"
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	tail := ""
	if n := len(segs); n > 0 && segs[n-1] != "" {
		if len(segs[n-1]) > 4 {
			tail = "****" + segs[n-1][len(segs[n-1])-4:]
		} else {
			tail = "****"
		}
		segs[n-1] = tail
	}
	return u.Scheme + "://" + u.Host + "/" + strings.Join(segs, "/")
}

func splitGlobs(s string) []string {
	out := []string{}
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func joinGlobs(v []string) string { return strings.Join(v, ",") }

// ListRepos 列出用户可见项目下的仓库。pids/all 来自 VisibleProjects。
func (d *DB) ListRepos(pids []int64, all bool) ([]Repository, error) {
	cond, args := projectPredicate("r.project_id", pids, all)
	q := `SELECT ` + repoCols + repoFrom
	if cond != "" {
		q += ` WHERE ` + cond
	}
	q += ` ORDER BY r.project_id, r.id`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Repository{}
	for rows.Next() {
		r, err := d.scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListAllRepos 不分项目地列出全部，只给后台任务用（定时兜底扫描、日报分流）。
func (d *DB) ListAllRepos() ([]Repository, error) {
	return d.ListRepos(nil, true)
}

func (d *DB) GetRepo(id int64) (*Repository, error) {
	return d.scanRepo(d.QueryRow(`SELECT `+repoCols+repoFrom+` WHERE r.id=?`, id))
}

// RepoWebhook 取仓库的飞书 webhook 明文，只在发通知时调用。
// webhook URL 里带着 token，拿到就能往群里发消息，所以和代码凭据同等待遇：
// 加密存储、界面只回显形态、日志不打印。
func (d *DB) RepoWebhook(id int64) (string, error) {
	var enc string
	if err := d.QueryRow(`SELECT webhook_enc FROM repositories WHERE id=?`, id).Scan(&enc); err != nil {
		return "", err
	}
	if enc == "" {
		return "", nil
	}
	return crypto.Decrypt(d.key, enc)
}

// RepoCredential 只在拉代码时调用，明文绝不经由 API 返回（§5）。
func (d *DB) RepoCredential(id int64) (string, error) {
	var enc string
	if err := d.QueryRow(`SELECT credential_enc FROM repositories WHERE id=?`, id).Scan(&enc); err != nil {
		return "", err
	}
	if enc == "" {
		return "", nil
	}
	return crypto.Decrypt(d.key, enc)
}

type RepoInput struct {
	Name           string   `json:"name"`
	GitURL         string   `json:"git_url"`
	Branch         string   `json:"branch"`
	Credential     *string  `json:"credential"`     // nil=不改，""=清空
	Webhook        *string  `json:"feishu_webhook"` // 同上
	ProjectID      int64    `json:"project_id"`
	Team           string   `json:"team"`
	Language       string   `json:"language"`
	IncludeGlobs   []string `json:"include_globs"`
	ExcludeGlobs   []string `json:"exclude_globs"`
	SpecDocs       []string `json:"spec_docs"`
	TriggerWebhook bool     `json:"trigger_webhook"`
	TriggerCron    string   `json:"trigger_cron"`
	DefaultModel   string   `json:"default_model"`
	Enabled        bool     `json:"enabled"`
}

func (d *DB) CreateRepo(in RepoInput) (int64, error) {
	enc, err := d.encryptCredential(in.Credential)
	if err != nil {
		return 0, err
	}
	hook, err := d.encryptCredential(in.Webhook)
	if err != nil {
		return 0, err
	}
	if in.Branch == "" {
		in.Branch = "master"
	}
	spec, _ := json.Marshal(orEmpty(in.SpecDocs))
	now := Now()
	res, err := d.Exec(`INSERT INTO repositories
		(project_id,name,git_url,branch,credential_enc,webhook_enc,team,language,include_globs,exclude_globs,spec_docs,
		 trigger_webhook,trigger_cron,default_model,enabled,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.ProjectID, in.Name, in.GitURL, in.Branch, enc, hook, in.Team, orDefault(in.Language, "go"),
		joinGlobs(in.IncludeGlobs), joinGlobs(in.ExcludeGlobs),
		string(spec), b2i(in.TriggerWebhook), in.TriggerCron, in.DefaultModel, b2i(in.Enabled), now, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	// 老库迁移时如果还没有任何仓库，规则会以 repo_id=0 存着。第一个仓库建出来时认领，
	// 否则那批规则就成了永远看不见的孤儿。
	if _, err := d.Exec(`UPDATE subsystems SET repo_id=? WHERE repo_id=0`, id); err != nil {
		return id, err
	}
	return id, nil
}

func (d *DB) UpdateRepo(id int64, in RepoInput) error {
	sets := []string{`project_id=?`, `name=?`, `git_url=?`, `branch=?`, `team=?`, `language=?`, `include_globs=?`, `exclude_globs=?`,
		`spec_docs=?`, `trigger_webhook=?`, `trigger_cron=?`, `default_model=?`, `enabled=?`, `updated_at=?`}
	spec, _ := json.Marshal(orEmpty(in.SpecDocs))
	args := []any{in.ProjectID, in.Name, in.GitURL, in.Branch, in.Team, orDefault(in.Language, "go"),
		joinGlobs(in.IncludeGlobs), joinGlobs(in.ExcludeGlobs),
		string(spec), b2i(in.TriggerWebhook), in.TriggerCron, in.DefaultModel, b2i(in.Enabled), Now()}
	for _, f := range []struct {
		name string
		val  *string
		col  string
	}{{"凭据", in.Credential, "credential_enc"}, {"webhook", in.Webhook, "webhook_enc"}} {
		if f.val == nil {
			continue
		}
		enc, err := d.encryptCredential(f.val)
		if err != nil {
			return fmt.Errorf("加密%s失败: %w", f.name, err)
		}
		sets = append(sets, f.col+"=?")
		args = append(args, enc)
	}
	args = append(args, id)
	_, err := d.Exec(`UPDATE repositories SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	return err
}

func (d *DB) encryptCredential(p *string) (string, error) {
	if p == nil || *p == "" {
		return "", nil
	}
	return crypto.Encrypt(d.key, *p)
}

func (d *DB) DeleteRepo(id int64) error {
	_, err := d.Exec(`DELETE FROM repositories WHERE id=?`, id)
	return err
}

// ---------- model_configs ----------

func (d *DB) GetModelConfig() (*ModelConfig, error) {
	var mc ModelConfig
	var enc, modelsJSON string
	err := d.QueryRow(`SELECT gateway_url,gateway_key_enc,timeout_sec,models,default_model,updated_at
		FROM model_configs WHERE id=1`).
		Scan(&mc.GatewayURL, &enc, &mc.TimeoutSec, &modelsJSON, &mc.DefaultModel, &mc.UpdatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(modelsJSON), &mc.Models)
	if mc.Models == nil {
		mc.Models = []string{}
	}
	mc.HasGatewayKey = enc != ""
	if enc != "" {
		if plain, err := crypto.Decrypt(d.key, enc); err == nil {
			mc.GatewayKeyMask = crypto.Mask(plain)
		} else {
			mc.GatewayKeyMask = "****"
		}
	}
	return &mc, nil
}

func (d *DB) GatewayKey() (string, error) {
	var enc string
	if err := d.QueryRow(`SELECT gateway_key_enc FROM model_configs WHERE id=1`).Scan(&enc); err != nil {
		return "", err
	}
	if enc == "" {
		return "", nil
	}
	return crypto.Decrypt(d.key, enc)
}

func (d *DB) UpdateModelConfig(gatewayURL string, key *string, timeoutSec int, models []string, def string) error {
	sets := []string{`gateway_url=?`, `timeout_sec=?`, `models=?`, `default_model=?`, `updated_at=?`}
	mj, _ := json.Marshal(orEmpty(models))
	args := []any{gatewayURL, timeoutSec, string(mj), def, Now()}
	if key != nil {
		enc, err := d.encryptCredential(key)
		if err != nil {
			return err
		}
		sets = append(sets, `gateway_key_enc=?`)
		args = append(args, enc)
	}
	_, err := d.Exec(`UPDATE model_configs SET `+strings.Join(sets, ",")+` WHERE id=1`, args...)
	return err
}

// ---------- tasks ----------

func (d *DB) CreateTask(t *Task) error {
	res, err := d.Exec(`INSERT INTO review_tasks
		(repo_id,trigger_type,commit_from,commit_to,time_from,time_to,status,model_name,prompt_version,queued_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		t.RepoID, t.TriggerType, t.CommitFrom, t.CommitTo, t.TimeFrom, t.TimeTo,
		t.Status, t.ModelName, t.PromptVersion, Now())
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

func (d *DB) SetTaskStatus(id int64, status, errMsg string) error {
	sets := []string{`status=?`}
	args := []any{status}
	if errMsg != "" {
		sets = append(sets, `error=?`)
		args = append(args, errMsg)
	}
	switch status {
	case "running":
		sets = append(sets, `started_at=?`)
		args = append(args, Now())
	case "success", "failed":
		sets = append(sets, `finished_at=?`)
		args = append(args, Now())
	}
	args = append(args, id)
	_, err := d.Exec(`UPDATE review_tasks SET `+strings.Join(sets, ",")+` WHERE id=?`, args...)
	return err
}

func (d *DB) SetTaskVersions(id int64, model, modelVer, promptVer, specVer string) error {
	_, err := d.Exec(`UPDATE review_tasks SET model_name=?,model_version=?,prompt_version=?,spec_version=? WHERE id=?`,
		model, modelVer, promptVer, specVer, id)
	return err
}

func (d *DB) RetryTask(id int64) error {
	_, err := d.Exec(`UPDATE review_tasks SET retry_count=retry_count+1,status='queued',error='' WHERE id=?`, id)
	return err
}

func (d *DB) MarkSoftWarned(id int64) error {
	_, err := d.Exec(`UPDATE review_tasks SET soft_warned=1 WHERE id=?`, id)
	return err
}

func (d *DB) SoftWarned(id int64) bool {
	var n int
	_ = d.QueryRow(`SELECT soft_warned FROM review_tasks WHERE id=?`, id).Scan(&n)
	return n == 1
}

const taskCols = `t.id,t.repo_id,COALESCE(r.name,''),t.trigger_type,t.commit_from,t.commit_to,t.time_from,t.time_to,t.status,
	t.retry_count,t.model_name,t.model_version,t.prompt_version,t.spec_version,t.error,
	t.queued_at,t.started_at,t.finished_at,COALESCE((SELECT id FROM review_reports WHERE task_id=t.id LIMIT 1),0)`

func scanTask(s interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := s.Scan(&t.ID, &t.RepoID, &t.RepoName, &t.TriggerType, &t.CommitFrom, &t.CommitTo,
		&t.TimeFrom, &t.TimeTo, &t.Status,
		&t.RetryCount, &t.ModelName, &t.ModelVersion, &t.PromptVersion, &t.SpecVersion, &t.Error,
		&t.QueuedAt, &t.StartedAt, &t.FinishedAt, &t.ReportID)
	return &t, err
}

func (d *DB) GetTask(id int64) (*Task, error) {
	return scanTask(d.QueryRow(`SELECT `+taskCols+` FROM review_tasks t LEFT JOIN repositories r ON r.id=t.repo_id WHERE t.id=?`, id))
}

func (d *DB) ListTasks(repoID int64, status string, limit, offset int, pids []int64, all bool) ([]Task, int, error) {
	where, args := []string{"1=1"}, []any{}
	if repoID > 0 {
		where, args = append(where, "t.repo_id=?"), append(args, repoID)
	}
	if status != "" {
		where, args = append(where, "t.status=?"), append(args, status)
	}
	if cond, a := repoVisibleCond("t.repo_id", pids, all); cond != "" {
		where, args = append(where, cond), append(args, a...)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := d.QueryRow(`SELECT COUNT(*) FROM review_tasks t WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.Query(`SELECT `+taskCols+` FROM review_tasks t LEFT JOIN repositories r ON r.id=t.repo_id
		WHERE `+cond+` ORDER BY t.id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *t)
	}
	return out, total, rows.Err()
}

func (d *DB) AppendTaskLog(taskID int64, level, msg string) {
	_, _ = d.Exec(`INSERT INTO task_logs(task_id,ts,level,message) VALUES(?,?,?,?)`, taskID, Now(), level, msg)
}

func (d *DB) ListTaskLogs(taskID int64) ([]TaskLog, error) {
	rows, err := d.Query(`SELECT ts,level,message FROM task_logs WHERE task_id=? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskLog{}
	for rows.Next() {
		var l TaskLog
		if err := rows.Scan(&l.TS, &l.Level, &l.Message); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---------- reviewed commits（定时兜底判定覆盖，§4.1） ----------

func (d *DB) MarkReviewed(repoID int64, shas []string, taskID int64) {
	for _, sha := range shas {
		if sha == "" {
			continue
		}
		_, _ = d.Exec(`INSERT OR REPLACE INTO review_marks(repo_id,sha,task_id,reviewed_at) VALUES(?,?,?,?)`,
			repoID, sha, taskID, Now())
	}
}

func (d *DB) ReviewedSet(repoID int64, since string) (map[string]bool, error) {
	rows, err := d.Query(`SELECT sha FROM review_marks WHERE repo_id=? AND reviewed_at>=?`, repoID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out[sha] = true
	}
	return out, rows.Err()
}

// ---------- reports ----------

func (d *DB) CreateReport(r *Report) error {
	commits, _ := json.Marshal(orEmpty(r.Commits))
	files, _ := json.Marshal(orEmpty(r.Files))
	res, err := d.Exec(`INSERT INTO review_reports
		(task_id,repo_id,commits,files,issue_count,high_count,model_name,model_version,prompt_version,spec_version,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.TaskID, r.RepoID, string(commits), string(files), r.IssueCount, r.HighCount,
		r.ModelName, r.ModelVersion, r.PromptVersion, r.SpecVersion, Now())
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

const reportCols = `rp.id,rp.task_id,rp.repo_id,COALESCE(rc.name,''),rp.commits,rp.files,rp.issue_count,rp.high_count,
	rp.model_name,rp.model_version,rp.prompt_version,rp.spec_version,rp.created_at`

func scanReport(s interface{ Scan(...any) error }) (*Report, error) {
	var r Report
	var commits, files string
	err := s.Scan(&r.ID, &r.TaskID, &r.RepoID, &r.RepoName, &commits, &files, &r.IssueCount, &r.HighCount,
		&r.ModelName, &r.ModelVersion, &r.PromptVersion, &r.SpecVersion, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(commits), &r.Commits)
	_ = json.Unmarshal([]byte(files), &r.Files)
	if r.Commits == nil {
		r.Commits = []string{}
	}
	if r.Files == nil {
		r.Files = []string{}
	}
	return &r, nil
}

func (d *DB) GetReport(id int64) (*Report, error) {
	return scanReport(d.QueryRow(`SELECT `+reportCols+` FROM review_reports rp
		LEFT JOIN repositories rc ON rc.id=rp.repo_id WHERE rp.id=?`, id))
}

func (d *DB) ListReports(repoID int64, from, to string, limit, offset int, pids []int64, all bool) ([]Report, int, error) {
	where, args := []string{"1=1"}, []any{}
	if repoID > 0 {
		where, args = append(where, "rp.repo_id=?"), append(args, repoID)
	}
	if cond, a := repoVisibleCond("rp.repo_id", pids, all); cond != "" {
		where, args = append(where, cond), append(args, a...)
	}
	if from != "" {
		where, args = append(where, "rp.created_at>=?"), append(args, from)
	}
	if to != "" {
		where, args = append(where, "rp.created_at<=?"), append(args, to)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := d.QueryRow(`SELECT COUNT(*) FROM review_reports rp WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.Query(`SELECT `+reportCols+` FROM review_reports rp LEFT JOIN repositories rc ON rc.id=rp.repo_id
		WHERE `+cond+` ORDER BY rp.id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *r)
	}
	return out, total, rows.Err()
}

// ---------- issues ----------

const issueCols = `i.id,i.fingerprint,i.repo_id,COALESCE(rc.project_id,0),COALESCE(rc.name,''),i.report_id,i.source,i.category,i.severity,
	i.file_path,i.line_start,i.line_end,i.title,i.description,i.suggestion,i.code_snippet,i.linter,
	i.commit_sha,i.committer,i.committer_id,i.assignee_id,COALESCE(u.name,''),i.status,i.regression,
	i.regression_at,i.first_seen_at,i.last_seen_at,i.seen_count,i.closed_at`

func scanIssue(s interface{ Scan(...any) error }) (*Issue, error) {
	var i Issue
	var reg int
	err := s.Scan(&i.ID, &i.Fingerprint, &i.RepoID, &i.ProjectID, &i.RepoName, &i.ReportID, &i.Source, &i.Category, &i.Severity,
		&i.FilePath, &i.LineStart, &i.LineEnd, &i.Title, &i.Description, &i.Suggestion, &i.CodeSnippet, &i.Linter,
		&i.CommitSHA, &i.Committer, &i.CommitterID, &i.AssigneeID, &i.AssigneeName, &i.Status, &reg,
		&i.RegressionAt, &i.FirstSeenAt, &i.LastSeenAt, &i.SeenCount, &i.ClosedAt)
	if err != nil {
		return nil, err
	}
	i.Regression = reg == 1
	i.StatusName = StatusNames[i.Status]
	return &i, nil
}

// UpsertResult 描述一次指纹命中后的处置结果，供调用方决定是否发通知。
type UpsertResult struct {
	IssueID      int64
	IsNew        bool
	IsRegression bool
	PrevStatus   string
}

// UpsertIssue 按稳定指纹去重（§4.5）。已关闭（已修复/误报）的问题再次出现时翻回
// 「待处理」并标「回归」——这是 §附录决策 2 显式要求的信号。
func (d *DB) UpsertIssue(i *Issue) (UpsertResult, error) {
	var prevID, prevSeen int64
	var prevStatus string
	err := d.QueryRow(`SELECT id,status,seen_count FROM issues WHERE fingerprint=?`, i.Fingerprint).
		Scan(&prevID, &prevStatus, &prevSeen)
	now := Now()
	if err == sql.ErrNoRows {
		res, err := d.Exec(`INSERT INTO issues
			(fingerprint,repo_id,report_id,source,category,severity,file_path,line_start,line_end,title,
			 description,suggestion,code_snippet,linter,commit_sha,committer,committer_id,status,
			 regression,first_seen_at,last_seen_at,seen_count)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'pending',0,?,?,1)`,
			i.Fingerprint, i.RepoID, i.ReportID, i.Source, i.Category, i.Severity, i.FilePath,
			i.LineStart, i.LineEnd, i.Title, i.Description, i.Suggestion, i.CodeSnippet, i.Linter,
			i.CommitSHA, i.Committer, i.CommitterID, now, now)
		if err != nil {
			return UpsertResult{}, err
		}
		id, _ := res.LastInsertId()
		d.linkReport(i.ReportID, id)
		return UpsertResult{IssueID: id, IsNew: true}, nil
	}
	if err != nil {
		return UpsertResult{}, err
	}

	// 指纹命中：行号等展示字段跟随最新一次，status 按 §4.5 规则决定
	regression := 0
	newStatus := prevStatus
	if prevStatus == StatusFixed || prevStatus == StatusFalsePos {
		newStatus, regression = StatusPending, 1
	}
	_, err = d.Exec(`UPDATE issues SET report_id=?,line_start=?,line_end=?,title=?,description=?,suggestion=?,
		code_snippet=?,commit_sha=?,committer=?,committer_id=?,status=?,regression=regression|?,
		regression_at=CASE WHEN ?>0 THEN ? ELSE regression_at END,
		last_seen_at=?,seen_count=?,closed_at=CASE WHEN ?>0 THEN '' ELSE closed_at END
		WHERE id=?`,
		i.ReportID, i.LineStart, i.LineEnd, i.Title, i.Description, i.Suggestion, i.CodeSnippet,
		i.CommitSHA, i.Committer, i.CommitterID, newStatus, regression, regression, now,
		now, prevSeen+1, regression, prevID)
	if err != nil {
		return UpsertResult{}, err
	}
	d.linkReport(i.ReportID, prevID)
	return UpsertResult{IssueID: prevID, IsRegression: regression == 1, PrevStatus: prevStatus}, nil
}

// linkReport 记录问题与报告的关联。两条 Upsert 分支都要走，否则旧报告会缺条目。
func (d *DB) linkReport(reportID, issueID int64) {
	if reportID == 0 || issueID == 0 {
		return
	}
	_, _ = d.Exec(`INSERT OR IGNORE INTO report_issues(report_id,issue_id) VALUES(?,?)`, reportID, issueID)
}

// ReportVisible 判断报告是否落在可见项目内。详情页按 id 直取，必须单独把关，
// 否则换个 id 就能看到别的项目的内容。
func (d *DB) ReportVisible(reportID int64, pids []int64, all bool) bool {
	cond, args := repoVisibleCond("rp.repo_id", pids, all)
	q := `SELECT COUNT(*) FROM review_reports rp WHERE rp.id=?`
	qa := []any{reportID}
	if cond != "" {
		q += ` AND ` + cond
		qa = append(qa, args...)
	}
	var n int
	if err := d.QueryRow(q, qa...).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// IssueVisible 同上，问题详情按 id 直取时的把关。
func (d *DB) IssueVisible(issueID int64, pids []int64, all bool) bool {
	cond, args := repoVisibleCond("i.repo_id", pids, all)
	q := `SELECT COUNT(*) FROM issues i WHERE i.id=?`
	qa := []any{issueID}
	if cond != "" {
		q += ` AND ` + cond
		qa = append(qa, args...)
	}
	var n int
	if err := d.QueryRow(q, qa...).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// ListReportIssues 取某份报告本次发现的问题，供报告详情页展示。
func (d *DB) ListReportIssues(reportID int64) ([]Issue, error) {
	rows, err := d.Query(`SELECT `+issueCols+` FROM report_issues ri
		JOIN issues i ON i.id=ri.issue_id
		LEFT JOIN repositories rc ON rc.id=i.repo_id
		LEFT JOIN users u ON u.id=i.assignee_id
		WHERE ri.report_id=?
		ORDER BY CASE i.severity WHEN '高危' THEN 0 WHEN '建议' THEN 1 ELSE 2 END, i.id DESC`, reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Issue{}
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

// strs / ints 只是把具体类型的切片转成 []any，好让下面那张「列 + 取值」的表写得整齐。
func strs(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func ints(v []int64) []any {
	out := make([]any, len(v))
	for i, n := range v {
		out[i] = n
	}
	return out
}

// inClause 拼「某列 IN 多个值」的谓词与参数。列名是代码里写死的，不是用户输入。
// 取值是空切片时返回空谓词，调用方跳过——多选里「一个都没勾」就是「不筛这一项」。
func inClause(col string, vals []any) (string, []any) {
	if len(vals) == 0 {
		return "", nil
	}
	ph := make([]string, len(vals))
	for i := range vals {
		ph[i] = "?"
	}
	return col + " IN (" + strings.Join(ph, ",") + ")", vals
}

func (d *DB) buildIssueWhere(f IssueFilter) (string, []any) {
	where, args := []string{"1=1"}, []any{}
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if f.RepoID > 0 {
		add("i.repo_id=?", f.RepoID)
	}
	for _, c := range []struct {
		col  string
		vals []any
	}{
		{"i.status", strs(f.Statuses)},
		{"i.severity", strs(f.Severities)},
		{"i.category", strs(f.Categories)},
		{"i.source", strs(f.Sources)},
		{"i.assignee_id", ints(f.AssigneeIDs)},
	} {
		if cond, a := inClause(c.col, c.vals); cond != "" {
			where, args = append(where, cond), append(args, a...)
		}
	}
	if f.Regression {
		where = append(where, "i.regression=1")
	}
	if f.From != "" {
		add("i.last_seen_at>=?", f.From)
	}
	if f.To != "" {
		add("i.last_seen_at<=?", f.To)
	}
	if cond, a := repoVisibleCond("i.repo_id", f.ProjectIDs, f.AllProjects); cond != "" {
		where, args = append(where, cond), append(args, a...)
	}
	if f.Keyword != "" {
		kw := "%" + LikeEscape(f.Keyword) + "%"
		where = append(where, `(i.title LIKE ? ESCAPE '\' OR i.file_path LIKE ? ESCAPE '\' OR i.code_snippet LIKE ? ESCAPE '\')`)
		args = append(args, kw, kw, kw)
	}
	return strings.Join(where, " AND "), args
}

func (d *DB) ListIssues(f IssueFilter) ([]Issue, int, error) {
	cond, args := d.buildIssueWhere(f)
	var total int
	if err := d.QueryRow(`SELECT COUNT(*) FROM issues i WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT ` + issueCols + ` FROM issues i
		LEFT JOIN repositories rc ON rc.id=i.repo_id
		LEFT JOIN users u ON u.id=i.assignee_id
		WHERE ` + cond +
		` ORDER BY CASE i.severity WHEN '高危' THEN 0 WHEN '建议' THEN 1 ELSE 2 END, i.id DESC LIMIT ? OFFSET ?`
	rows, err := d.Query(q, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Issue{}
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *i)
	}
	return out, total, rows.Err()
}

func (d *DB) GetIssue(id int64) (*Issue, error) {
	return scanIssue(d.QueryRow(`SELECT `+issueCols+` FROM issues i
		LEFT JOIN repositories rc ON rc.id=i.repo_id
		LEFT JOIN users u ON u.id=i.assignee_id WHERE i.id=?`, id))
}

// SetIssueStatus 是状态流转的唯一入口，顺带写操作审计（§5 可追溯性）。
func (d *DB) SetIssueStatus(id int64, status string, operatorID int64, operator, note string) error {
	var prev string
	if err := d.QueryRow(`SELECT status FROM issues WHERE id=?`, id).Scan(&prev); err != nil {
		return err
	}
	if prev == status {
		return nil
	}
	closed := ""
	switch status {
	case StatusFixed, StatusFalsePos:
		closed = Now()
	}
	if _, err := d.Exec(`UPDATE issues SET status=?,closed_at=?,regression=0 WHERE id=?`, status, closed, id); err != nil {
		return err
	}
	_, _ = d.Exec(`INSERT INTO issue_events(issue_id,from_status,to_status,operator_id,operator,note,created_at)
		VALUES(?,?,?,?,?,?,?)`, id, prev, status, operatorID, operator, note, Now())
	d.AppendAudit(operatorID, operator, "issue.status", "issue", fmt.Sprint(id),
		fmt.Sprintf("%s → %s %s", StatusNames[prev], StatusNames[status], note))
	return nil
}

func (d *DB) AssignIssue(id, assigneeID int64, operatorID int64, operator string) error {
	if _, err := d.Exec(`UPDATE issues SET assignee_id=? WHERE id=?`, assigneeID, id); err != nil {
		return err
	}
	d.AppendAudit(operatorID, operator, "issue.assign", "issue", fmt.Sprint(id), fmt.Sprintf("指派给 #%d", assigneeID))
	return nil
}

// BatchIssueOp 支持批量标误报 / 批量指派（§4.5）。
func (d *DB) BatchIssueOp(ids []int64, op string, value int64, operatorID int64, operator string) error {
	for _, id := range ids {
		switch op {
		case "false_positive", "fixed", "confirmed", "pending":
			if err := d.SetIssueStatus(id, op, operatorID, operator, "批量操作"); err != nil {
				return err
			}
		case "assign":
			if err := d.AssignIssue(id, value, operatorID, operator); err != nil {
				return err
			}
		default:
			return fmt.Errorf("未知批量操作 %q", op)
		}
	}
	return nil
}

func (d *DB) AddComment(issueID, userID int64, userName, content string) (int64, error) {
	res, err := d.Exec(`INSERT INTO comments(issue_id,user_id,user_name,content,created_at) VALUES(?,?,?,?,?)`,
		issueID, userID, userName, content, Now())
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	d.AppendAudit(userID, userName, "issue.comment", "issue", fmt.Sprint(issueID), content)
	return id, nil
}

func (d *DB) ListComments(issueID int64) ([]Comment, error) {
	rows, err := d.Query(`SELECT id,issue_id,user_id,user_name,content,created_at FROM comments
		WHERE issue_id=? ORDER BY id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.IssueID, &c.UserID, &c.UserName, &c.Content, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) ListIssueEvents(issueID int64) ([]IssueEvent, error) {
	rows, err := d.Query(`SELECT from_status,to_status,operator,note,created_at FROM issue_events
		WHERE issue_id=? ORDER BY id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IssueEvent{}
	for rows.Next() {
		var e IssueEvent
		if err := rows.Scan(&e.FromStatus, &e.ToStatus, &e.Operator, &e.Note, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- users / sessions / identities ----------

const userCols = `id,feishu_user_id,username,name,role,avatar,last_login_at`

func scanUser(s interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := s.Scan(&u.ID, &u.FeishuID, &u.Username, &u.Name, &u.Role, &u.Avatar, &u.LastLoginAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) UpsertUser(feishuID, name, avatar string) (*User, error) {
	now := Now()
	_, err := d.Exec(`INSERT INTO users(feishu_user_id,name,role,avatar,created_at,last_login_at)
		VALUES(?,?,'user',?,?,?) ON CONFLICT(feishu_user_id) DO UPDATE SET
		name=excluded.name, avatar=excluded.avatar, last_login_at=excluded.last_login_at`,
		feishuID, name, avatar, now, now)
	if err != nil {
		return nil, err
	}
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE feishu_user_id=?`, feishuID))
}

// UserByID 按 id 取携带完整角色的用户对象。
func (d *DB) UserByID(id int64) (*User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

// UserByFeishu 按飞书 id 取用户，通知模块要用它拿 open_id。
func (d *DB) UserByFeishu(feishuID string) (*User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE feishu_user_id=?`, feishuID))
}

// UserByUsername 查账号密码登录用的账号。只有内置管理员有 username。
func (d *DB) UserByUsername(username string) (*User, error) {
	return scanUser(d.QueryRow(`SELECT `+userCols+` FROM users WHERE username=? AND username<>''`, username))
}

// PasswordHash 单独取，不放进 User 结构体——它没有任何理由跟着用户信息
// 一路传到 HTTP 响应里。
func (d *DB) PasswordHash(userID int64) (string, error) {
	var h string
	err := d.QueryRow(`SELECT password_hash FROM users WHERE id=?`, userID).Scan(&h)
	return h, err
}

func (d *DB) SetPassword(userID int64, hash string) error {
	_, err := d.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, userID)
	return err
}

func (d *DB) TouchLogin(userID int64) {
	_, _ = d.Exec(`UPDATE users SET last_login_at=? WHERE id=?`, Now(), userID)
}

func (d *DB) ListUsers() ([]User, error) {
	rows, err := d.Query(`SELECT ` + userCols + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// FeishuID 取用户的 open_id，用于通知里 @ 到人（§4.5 指派通知）。
func (d *DB) FeishuID(userID int64) string {
	var s string
	_ = d.QueryRow(`SELECT feishu_user_id FROM users WHERE id=?`, userID).Scan(&s)
	return s
}

// CountAdmins 用于首次登录时自动授予管理员，避免新部署没人能进配置页。
func (d *DB) CountAdmins() (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n)
	return n, err
}

func (d *DB) SetUserRole(id int64, role string) error {
	_, err := d.Exec(`UPDATE users SET role=? WHERE id=?`, role, id)
	return err
}

func (d *DB) CreateSession(token string, userID int64, expires string) error {
	_, err := d.Exec(`INSERT INTO sessions(token,user_id,created_at,expires_at) VALUES(?,?,?,?)`,
		token, userID, Now(), expires)
	return err
}

func (d *DB) SessionUser(token string) (*User, error) {
	var u User
	err := d.QueryRow(`SELECT u.id,u.feishu_user_id,u.username,u.name,u.role,u.avatar,u.last_login_at
		FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token=? AND s.expires_at>?`, token, Now()).
		Scan(&u.ID, &u.FeishuID, &u.Username, &u.Name, &u.Role, &u.Avatar, &u.LastLoginAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) DeleteSession(token string) error {
	_, err := d.Exec(`DELETE FROM sessions WHERE token=?`, token)
	return err
}

func (d *DB) ListIdentities() ([]Identity, error) {
	rows, err := d.Query(`SELECT ci.id,ci.git_name,ci.user_id,COALESCE(u.name,''),ci.is_bot,ci.note
		FROM committer_identities ci LEFT JOIN users u ON u.id=ci.user_id ORDER BY ci.git_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Identity{}
	for rows.Next() {
		var i Identity
		var bot int
		if err := rows.Scan(&i.ID, &i.GitName, &i.UserID, &i.UserName, &bot, &i.Note); err != nil {
			return nil, err
		}
		i.IsBot = bot == 1
		out = append(out, i)
	}
	return out, rows.Err()
}

func (d *DB) UpsertIdentity(gitName string, userID int64, isBot bool, note string) error {
	_, err := d.Exec(`INSERT INTO committer_identities(git_name,user_id,is_bot,note) VALUES(?,?,?,?)
		ON CONFLICT(git_name) DO UPDATE SET user_id=excluded.user_id,is_bot=excluded.is_bot,note=excluded.note`,
		gitName, userID, b2i(isBot), note)
	return err
}

func (d *DB) DeleteIdentity(id int64) error {
	_, err := d.Exec(`DELETE FROM committer_identities WHERE id=?`, id)
	return err
}

// ResolveCommitter 把 git 身份归并到人（§4.8）。未登记的返回 (0,false)，
// 审核管线会把它当「未归属」，仍写进问题但不参与人员统计。
func (d *DB) ResolveCommitter(gitName string) (int64, bool) {
	var uid int64
	var bot int
	err := d.QueryRow(`SELECT user_id,is_bot FROM committer_identities WHERE git_name=?`, gitName).Scan(&uid, &bot)
	if err != nil {
		return 0, false
	}
	return uid, bot == 1
}

// ---------- subsystems ----------

func (d *DB) ListSubsystems(repoID int64) ([]Subsystem, error) {
	rows, err := d.Query(`SELECT id,repo_id,pattern,name,priority FROM subsystems
		WHERE repo_id=? ORDER BY priority,id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subsystem{}
	for rows.Next() {
		var s Subsystem
		if err := rows.Scan(&s.ID, &s.RepoID, &s.Pattern, &s.Name, &s.Priority); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) ReplaceSubsystems(repoID int64, list []Subsystem) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM subsystems WHERE repo_id=?`, repoID); err != nil {
		return err
	}
	for i, s := range list {
		if s.Pattern == "" || s.Name == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO subsystems(repo_id,pattern,name,priority) VALUES(?,?,?,?)`,
			repoID, s.Pattern, s.Name, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SubsystemMatcher 持有一次查询取回的归类规则，Match 是纯内存操作。
//
// 它存在的理由不只是省查询：连接池上限是 1（SQLite 单写者），在遍历 rows 的过程中
// 再发一次查询，就会去等一个永远不会释放的连接——把整库连同所有 HTTP 请求一起卡死。
// 所以规则必须在开游标之前取好，循环里只做字符串匹配。
type SubsystemMatcher struct{ rules []Subsystem }

// unmappedSubsystem 是规则没命中时的兜底桶名。
const unmappedSubsystem = "未分类"

// LoadMatcher 取一次某仓库的规则。取失败时返回空匹配器，归类全部落到「未分类」，
// 不让一次配置读取失败把整张报表打成 500。
func (d *DB) LoadMatcher(repoID int64) *SubsystemMatcher {
	rules, err := d.ListSubsystems(repoID)
	if err != nil {
		return &SubsystemMatcher{}
	}
	return &SubsystemMatcher{rules: rules}
}

// LoadMatchers 一次取回多个仓库的规则，供跨仓库报表使用。
// 同一份报表里不同仓库各按自己的规则归类，不共用一套。
func (d *DB) LoadMatchers(repoIDs []int64) map[int64]*SubsystemMatcher {
	out := make(map[int64]*SubsystemMatcher, len(repoIDs))
	for _, id := range repoIDs {
		out[id] = d.LoadMatcher(id)
	}
	return out
}

// MatchAny 在跨仓库场景下按问题所属仓库选规则。
func MatchAny(m map[int64]*SubsystemMatcher, repoID int64, filePath string) string {
	if matcher, ok := m[repoID]; ok {
		return matcher.Match(filePath)
	}
	return unmappedSubsystem
}

func (m *SubsystemMatcher) Match(filePath string) string {
	low := strings.ToLower(filePath)
	for _, r := range m.rules {
		if r.Pattern != "" && strings.Contains(low, strings.ToLower(r.Pattern)) {
			return r.Name
		}
	}
	return unmappedSubsystem
}

// MatchSubsystem 是一次性调用的便捷入口。禁止在 rows 遍历过程中调用，
// 那种场景请先 LoadMatcher 再在循环里用 Match。
func (d *DB) MatchSubsystem(repoID int64, filePath string) string {
	return d.LoadMatcher(repoID).Match(filePath)
}

// ---------- audit ----------

func (d *DB) AppendAudit(userID int64, userName, action, targetType, targetID, detail string) {
	_, _ = d.Exec(`INSERT INTO audit_logs(user_id,user_name,action,target_type,target_id,detail,created_at)
		VALUES(?,?,?,?,?,?,?)`, userID, userName, action, targetType, targetID, detail, Now())
}

func (d *DB) ListAudit(limit, offset int) ([]AuditLog, int, error) {
	var total int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.Query(`SELECT id,user_id,user_name,action,target_type,target_id,detail,created_at
		FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AuditLog{}
	for rows.Next() {
		var a AuditLog
		if err := rows.Scan(&a.ID, &a.UserID, &a.UserName, &a.Action, &a.TargetType, &a.TargetID, &a.Detail, &a.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// ---------- helpers ----------

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func joinSets(sets []string) string {
	out := ""
	for i, s := range sets {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
