package store

// 事实枚举。中文值直接落库，报表分组与界面展示同源，省一层映射表。
const (
	SeverityHigh   = "高危"
	SeverityAdvice = "建议"
	SeverityStyle  = "风格"

	CategorySecurity = "安全"
	CategoryPerf     = "性能"
	CategoryLogic    = "逻辑"
	CategoryDup      = "重复代码"
	CategorySpec     = "规范"

	SourceAI   = "ai"
	SourceLint = "lint"

	StatusPending   = "pending"
	StatusConfirmed = "confirmed"
	StatusFixed     = "fixed"
	StatusFalsePos  = "false_positive"
)

var StatusNames = map[string]string{
	StatusPending:   "待处理",
	StatusConfirmed: "已确认",
	StatusFixed:     "已修复",
	StatusFalsePos:  "误报",
}

type Repository struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	GitURL         string   `json:"git_url"`
	Branch         string   `json:"branch"`
	ProjectID      int64    `json:"project_id"`
	ProjectName    string   `json:"project_name"`
	Team           string   `json:"team"`
	Language       string   `json:"language"` // go | cpp | ...，决定上下文提取与 lint 方式
	IncludeGlobs   []string `json:"include_globs"`
	ExcludeGlobs   []string `json:"exclude_globs"`
	SpecDocs       []string `json:"spec_docs"`
	TriggerWebhook bool     `json:"trigger_webhook"`
	TriggerCron    string   `json:"trigger_cron"`
	DefaultModel   string   `json:"default_model"`
	Enabled        bool     `json:"enabled"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`

	// 只读回显，永不返回明文凭据（§5）
	CredentialMask string `json:"credential_mask"`
	HasCredential  bool   `json:"has_credential"`
	WebhookMask    string `json:"webhook_mask"`
	HasWebhook     bool   `json:"has_webhook"`
}

type ModelConfig struct {
	GatewayURL     string   `json:"gateway_url"`
	GatewayKeyMask string   `json:"gateway_key_mask"`
	HasGatewayKey  bool     `json:"has_gateway_key"`
	TimeoutSec     int      `json:"timeout_sec"`
	Models         []string `json:"models"`
	DefaultModel   string   `json:"default_model"`
	UpdatedAt      string   `json:"updated_at"`
}

type Task struct {
	ID            int64  `json:"id"`
	RepoID        int64  `json:"repo_id"`
	RepoName      string `json:"repo_name"`
	TriggerType   string `json:"trigger_type"`
	CommitFrom    string `json:"commit_from"`
	CommitTo      string `json:"commit_to"`
	TimeFrom      string `json:"time_from"` // 与 CommitFrom/To 互斥，二选一
	TimeTo        string `json:"time_to"`
	Status        string `json:"status"`
	RetryCount    int    `json:"retry_count"`
	ModelName     string `json:"model_name"`
	ModelVersion  string `json:"model_version"`
	PromptVersion string `json:"prompt_version"`
	SpecVersion   string `json:"spec_version"`
	Error         string `json:"error"`
	QueuedAt      string `json:"queued_at"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	ReportID      int64  `json:"report_id"`
}

type TaskLog struct {
	TS      string `json:"ts"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type Report struct {
	ID            int64    `json:"id"`
	TaskID        int64    `json:"task_id"`
	RepoID        int64    `json:"repo_id"`
	RepoName      string   `json:"repo_name"`
	Commits       []string `json:"commits"`
	Files         []string `json:"files"`
	IssueCount    int      `json:"issue_count"`
	HighCount     int      `json:"high_count"`
	ModelName     string   `json:"model_name"`
	ModelVersion  string   `json:"model_version"`
	PromptVersion string   `json:"prompt_version"`
	SpecVersion   string   `json:"spec_version"`
	CreatedAt     string   `json:"created_at"`
}

type Issue struct {
	ID           int64  `json:"id"`
	Fingerprint  string `json:"fingerprint"`
	RepoID       int64  `json:"repo_id"`
	ProjectID    int64  `json:"project_id"`
	RepoName     string `json:"repo_name"`
	ReportID     int64  `json:"report_id"`
	Source       string `json:"source"`
	Category     string `json:"category"`
	Severity     string `json:"severity"`
	FilePath     string `json:"file_path"`
	LineStart    int    `json:"line_start"`
	LineEnd      int    `json:"line_end"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Suggestion   string `json:"suggestion"`
	CodeSnippet  string `json:"code_snippet"`
	Linter       string `json:"linter"`
	CommitSHA    string `json:"commit_sha"`
	Committer    string `json:"committer"`
	CommitterID  int64  `json:"committer_id"`
	AssigneeID   int64  `json:"assignee_id"`
	AssigneeName string `json:"assignee_name"`
	Status       string `json:"status"`
	StatusName   string `json:"status_name"`
	Regression   bool   `json:"regression"`
	RegressionAt string `json:"regression_at"`
	FirstSeenAt  string `json:"first_seen_at"`
	LastSeenAt   string `json:"last_seen_at"`
	SeenCount    int    `json:"seen_count"`
	ClosedAt     string `json:"closed_at"`
	Subsystem    string `json:"subsystem"`
}

type Comment struct {
	ID        int64  `json:"id"`
	IssueID   int64  `json:"issue_id"`
	UserID    int64  `json:"user_id"`
	UserName  string `json:"user_name"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

type IssueEvent struct {
	FromStatus string `json:"from_status"`
	ToStatus   string `json:"to_status"`
	Operator   string `json:"operator"`
	Note       string `json:"note"`
	CreatedAt  string `json:"created_at"`
}

type User struct {
	ID          int64  `json:"id"`
	FeishuID    string `json:"feishu_user_id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Avatar      string `json:"avatar"`
	LastLoginAt string `json:"last_login_at"`
}

type Identity struct {
	ID       int64  `json:"id"`
	GitName  string `json:"git_name"`
	UserID   int64  `json:"user_id"`
	UserName string `json:"user_name"`
	IsBot    bool   `json:"is_bot"`
	Note     string `json:"note"`
}

type AuditLog struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	UserName   string `json:"user_name"`
	Action     string `json:"action"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Detail     string `json:"detail"`
	CreatedAt  string `json:"created_at"`
}

type Subsystem struct {
	ID       int64  `json:"id"`
	RepoID   int64  `json:"repo_id"`
	Pattern  string `json:"pattern"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}

// IssueFilter 是问题列表的筛选条件，零值表示不过滤。
// IssueFilter 的这几个维度都是多选（界面上的筛选项可以勾好几个），
// 空切片表示「这一项不筛」。AssigneeIDs 里的 0 表示「未指派」——
// 它就是 assignee_id 的一个普通取值，不用另开一个标志位。
type IssueFilter struct {
	RepoID      int64
	Statuses    []string
	Severities  []string
	Categories  []string
	Sources     []string
	AssigneeIDs []int64
	Subsystem   string
	Keyword     string
	From        string
	To          string
	Regression  bool

	// 项目可见性。AllProjects 为真时不过滤；否则只看 ProjectIDs 里的。
	// 两者都为零值时按「什么都看不到」处理，不按「不过滤」——后者是数据泄漏。
	ProjectIDs  []int64
	AllProjects bool
	Limit       int
	Offset      int
}
