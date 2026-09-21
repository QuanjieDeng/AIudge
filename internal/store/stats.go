package store

import (
	"sort"
	"strings"
)

// StatsFilter 是报表的公共筛选条件。From/To 作用于 first_seen_at（本期新增口径）。
type StatsFilter struct {
	RepoID  int64
	RepoIDs []int64 // 非空时只统计这几个仓库，用于「没配自己 webhook 的仓库合并成一份日报」
	From    string
	To      string
	Bucket  string // day | week | month

	// 项目可见性，语义同 IssueFilter：AllProjects 为真才不过滤
	ProjectIDs  []int64
	AllProjects bool
}

// where 统一以 i 作为 issues 的别名，调用方的 FROM 必须写 `issues i`。
func (f StatsFilter) where() (string, []any) {
	where, args := []string{"1=1"}, []any{}
	if f.RepoID > 0 {
		where, args = append(where, "i.repo_id=?"), append(args, f.RepoID)
	}
	if cond, a := repoVisibleCond("i.repo_id", f.ProjectIDs, f.AllProjects); cond != "" {
		where, args = append(where, cond), append(args, a...)
	}
	if len(f.RepoIDs) > 0 {
		ph := make([]string, len(f.RepoIDs))
		for i, id := range f.RepoIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		where = append(where, "i.repo_id IN ("+strings.Join(ph, ",")+")")
	}
	if f.From != "" {
		where, args = append(where, "i.first_seen_at>=?"), append(args, f.From)
	}
	if f.To != "" {
		where, args = append(where, "i.first_seen_at<=?"), append(args, f.To)
	}
	return strings.Join(where, " AND "), args
}

type Overview struct {
	NewCount    int     `json:"new_count"`
	HighCount   int     `json:"high_count"`
	AdviceCount int     `json:"advice_count"`
	StyleCount  int     `json:"style_count"`
	Pending     int     `json:"pending"`
	Confirmed   int     `json:"confirmed"`
	Fixed       int     `json:"fixed"`
	FalsePos    int     `json:"false_positive"`
	Regression  int     `json:"regression"`
	HandleRate  float64 `json:"handle_rate"`      // 处理率 =（已修复+误报）/ 总数
	FalseRate   float64 `json:"false_rate"`       // 误报率，用于评估 AI 审核质量（§4.6）
	AvgHandleHr float64 `json:"avg_handle_hours"` // 仅全局聚合，不按人拆（§附录决策 10）
}

func (d *DB) Overview(f StatsFilter) (*Overview, error) {
	cond, args := f.where()
	var o Overview
	// SUM(bool) 在无匹配行时返回 NULL，用 COALESCE 兜成 0，省掉一层 sql.NullXxx
	err := d.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(i.severity='高危'),0), COALESCE(SUM(i.severity='建议'),0), COALESCE(SUM(i.severity='风格'),0),
		COALESCE(SUM(i.status='pending'),0), COALESCE(SUM(i.status='confirmed'),0),
		COALESCE(SUM(i.status='fixed'),0), COALESCE(SUM(i.status='false_positive'),0),
		COALESCE(SUM(i.regression=1),0)
		FROM issues i WHERE `+cond, args...).
		Scan(&o.NewCount, &o.HighCount, &o.AdviceCount, &o.StyleCount,
			&o.Pending, &o.Confirmed, &o.Fixed, &o.FalsePos, &o.Regression)
	if err != nil {
		return nil, err
	}
	if o.NewCount > 0 {
		o.HandleRate = float64(o.Fixed+o.FalsePos) / float64(o.NewCount)
		o.FalseRate = float64(o.FalsePos) / float64(o.NewCount)
	}
	// 平均处理时长：已关闭问题的首见→关闭跨度。全局一个数，不做人员维度
	_ = d.QueryRow(`SELECT COALESCE(AVG((julianday(i.closed_at)-julianday(i.first_seen_at))*24),0)
		FROM issues i WHERE `+cond+` AND i.closed_at<>''`, args...).Scan(&o.AvgHandleHr)
	return &o, nil
}

type TrendPoint struct {
	Bucket   string `json:"bucket"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

// Trend 返回按严重等级分组的堆叠趋势（§4.6 趋势类）。
func (d *DB) Trend(f StatsFilter) ([]TrendPoint, error) {
	cond, args := f.where()
	rows, err := d.Query(`SELECT `+bucketExpr(f.Bucket)+` AS b, i.severity, COUNT(*)
		FROM issues i WHERE `+cond+` GROUP BY b, i.severity ORDER BY b`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrendPoint{}
	for rows.Next() {
		var p TrendPoint
		if err := rows.Scan(&p.Bucket, &p.Severity, &p.Count); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func bucketExpr(bucket string) string {
	switch bucket {
	case "day":
		return `strftime('%Y-%m-%d', i.first_seen_at)`
	case "month":
		return `strftime('%Y-%m', i.first_seen_at)`
	default:
		return `strftime('%Y-W%W', i.first_seen_at)`
	}
}

type DistItem struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// Distribution 支持 by=repo|category|source|subsystem。
// subsystem 需要路径→分类的规则匹配，SQL 表达不了，取回明细在 Go 侧归并。
func (d *DB) Distribution(by string, f StatsFilter) ([]DistItem, error) {
	cond, args := f.where()
	if by == "subsystem" {
		return d.subsystemDistribution(cond, args)
	}
	// repo 要 COALESCE：仓库被删之后历史问题还在，子查询会返回 NULL，
	// 扫描到 string 直接报错，整张分布图变 500
	expr := map[string]string{
		"category": "i.category",
		"source":   "i.source",
		"repo":     "COALESCE((SELECT name FROM repositories r WHERE r.id=i.repo_id),'(已删除仓库)')",
		"project":  "COALESCE((SELECT p.name FROM projects p JOIN repositories r2 ON r2.project_id=p.id WHERE r2.id=i.repo_id),'(未归属)')",
	}[by]
	if expr == "" {
		expr = "i.category"
	}
	rows, err := d.Query(`SELECT `+expr+` AS k, COUNT(*) FROM issues i WHERE `+cond+` GROUP BY k ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DistItem{}
	for rows.Next() {
		var it DistItem
		if err := rows.Scan(&it.Key, &it.Count); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (d *DB) subsystemDistribution(cond string, args []any) ([]DistItem, error) {
	// 规则必须在开游标之前取好：循环里再查库会等一个被当前游标占住的连接，
	// 连接池只有 1 条，整库连同所有请求会一起卡死（见 SubsystemMatcher 的说明）
	matchers, err := d.matchersFor(cond, args)
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT i.repo_id, i.file_path FROM issues i WHERE `+cond, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var repoID int64
		var p string
		if err := rows.Scan(&repoID, &p); err != nil {
			return nil, err
		}
		counts[MatchAny(matchers, repoID, p)]++
	}
	out := make([]DistItem, 0, len(counts))
	for k, v := range counts {
		out = append(out, DistItem{Key: k, Count: v})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Count > out[b].Count })
	return out, rows.Err()
}

type AssigneeStat struct {
	UserID  int64  `json:"user_id"`
	Name    string `json:"name"`
	Total   int    `json:"total"`   // 指派给他且在本期内的
	Handled int    `json:"handled"` // 已修复 + 误报
}

// AssigneeStats 只出处理数量，不出处理时长、不做排名化展示（§附录决策 10）。
func (d *DB) AssigneeStats(f StatsFilter) ([]AssigneeStat, error) {
	cond, args := f.where()
	rows, err := d.Query(`SELECT i.assignee_id, COALESCE(u.name,''), COUNT(*),
		COALESCE(SUM(i.status IN ('fixed','false_positive')),0)
		FROM issues i LEFT JOIN users u ON u.id=i.assignee_id
		WHERE i.assignee_id<>0 AND `+cond+` GROUP BY i.assignee_id ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssigneeStat{}
	for rows.Next() {
		var s AssigneeStat
		if err := rows.Scan(&s.UserID, &s.Name, &s.Total, &s.Handled); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// VersionTag 是 §4.6 要求的版本断点：模型/prompt/规范文档任一变更，
// 前后的趋势曲线就不可比，前端据此在时间轴上打标记。
type VersionTag struct {
	ModelName     string `json:"model_name"`
	ModelVersion  string `json:"model_version"`
	PromptVersion string `json:"prompt_version"`
	SpecVersion   string `json:"spec_version"`
	Bucket        string `json:"bucket"`
	FirstAt       string `json:"first_at"`
	ReportCount   int    `json:"report_count"`
}

// VersionTags 的 Bucket 由 SQLite 用与 Trend 完全相同的表达式算出，
// 保证断点落在真实存在的柱子上——前端自己按周数推算会和 %W 的口径错位。
func (d *DB) VersionTags(repoID int64, from, to, bucket string, pids []int64, all bool) ([]VersionTag, error) {
	where, args := []string{"1=1"}, []any{}
	if repoID > 0 {
		where, args = append(where, "repo_id=?"), append(args, repoID)
	}
	if cond, a := repoVisibleCond("repo_id", pids, all); cond != "" {
		where, args = append(where, cond), append(args, a...)
	}
	if from != "" {
		where, args = append(where, "created_at>=?"), append(args, from)
	}
	if to != "" {
		where, args = append(where, "created_at<=?"), append(args, to)
	}
	expr := strings.ReplaceAll(bucketExpr(bucket), "i.first_seen_at", "created_at")
	rows, err := d.Query(`SELECT model_name,model_version,prompt_version,spec_version,
		MIN(`+expr+`),MIN(created_at),COUNT(*) FROM review_reports WHERE `+strings.Join(where, " AND ")+
		` GROUP BY model_name,model_version,prompt_version,spec_version ORDER BY MIN(created_at)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionTag{}
	for rows.Next() {
		var v VersionTag
		if err := rows.Scan(&v.ModelName, &v.ModelVersion, &v.PromptVersion, &v.SpecVersion,
			&v.Bucket, &v.FirstAt, &v.ReportCount); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SubsystemOf 给问题详情用，界面要显示所属子系统。
func (d *DB) SubsystemOf(repoID int64, filePath string) string {
	return d.MatchSubsystem(repoID, filePath)
}

// matchersFor 找出这次统计会涉及哪些仓库，一次性把它们的规则取回来。
// 规则按仓库走：同一份跨仓库报表里，每个仓库用自己那套分类。
func (d *DB) matchersFor(cond string, args []any) (map[int64]*SubsystemMatcher, error) {
	rows, err := d.Query(`SELECT DISTINCT i.repo_id FROM issues i WHERE `+cond, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// rows 在这里已经关闭，下面再查规则表是安全的
	return d.LoadMatchers(ids), nil
}
