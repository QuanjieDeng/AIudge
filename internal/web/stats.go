package web

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"aiudge/internal/store"
)

// ---------- 统计报表（§4.6） ----------

func (s *Server) statsFilter(r *http.Request) store.StatsFilter {
	from, to := timeRange(r)
	pids, all := s.visibleProjects(userFrom(r))
	pids, all = scopeToProject(queryInt64(r, "project_id"), pids, all)
	return store.StatsFilter{
		ProjectIDs:  pids,
		AllProjects: all,
		RepoID:      queryInt64(r, "repo_id"),
		From:        from,
		To:          to,
		Bucket:      defaultStr(r.URL.Query().Get("bucket"), "week"),
	}
}

func (s *Server) handleStatsOverview(w http.ResponseWriter, r *http.Request) {
	o, err := s.db.Overview(s.statsFilter(r))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, o)
}

func (s *Server) handleStatsTrend(w http.ResponseWriter, r *http.Request) {
	points, err := s.db.Trend(s.statsFilter(r))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, points)
}

func (s *Server) handleStatsDistribution(w http.ResponseWriter, r *http.Request) {
	by := defaultStr(r.URL.Query().Get("by"), "category")
	items, err := s.db.Distribution(by, s.statsFilter(r))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, items)
}

func (s *Server) handleStatsAssignees(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.AssigneeStats(s.statsFilter(r))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, items)
}

func (s *Server) handleStatsVersions(w http.ResponseWriter, r *http.Request) {
	from, to := timeRange(r)
	bucket := defaultStr(r.URL.Query().Get("bucket"), "week")
	pids, all := s.visibleProjects(userFrom(r))
	pids, all = scopeToProject(queryInt64(r, "project_id"), pids, all)
	items, err := s.db.VersionTags(queryInt64(r, "repo_id"), from, to, bucket, pids, all)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, items)
}

// handleStatsExport 导出 CSV（§4.6：报表按时间范围、仓库自由筛选导出）。
// 问题明细导出同时承担 §8 的「误报数据反哺 prompt」用途。
func (s *Server) handleStatsExport(w http.ResponseWriter, r *http.Request) {
	kind := defaultStr(r.URL.Query().Get("type"), "issues")
	f := s.statsFilter(r)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=aiudge-%s-%s.csv", kind, time.Now().Format("20060102")))
	// Excel 读 UTF-8 CSV 需要 BOM，否则中文列名乱码
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	defer cw.Flush()

	switch kind {
	case "trend":
		_ = cw.Write([]string{"周期", "严重等级", "数量"})
		points, err := s.db.Trend(f)
		if err != nil {
			return
		}
		for _, p := range points {
			_ = cw.Write([]string{p.Bucket, p.Severity, strconv.Itoa(p.Count)})
		}
	case "distribution":
		by := defaultStr(r.URL.Query().Get("by"), "category")
		_ = cw.Write([]string{by, "数量"})
		items, err := s.db.Distribution(by, f)
		if err != nil {
			return
		}
		for _, it := range items {
			_ = cw.Write([]string{it.Key, strconv.Itoa(it.Count)})
		}
	default:
		_ = cw.Write([]string{"ID", "仓库", "文件", "行号", "来源", "分类", "等级", "状态",
			"回归", "标题", "提交人", "负责人", "首次出现", "最近出现"})
		// ProjectIDs/AllProjects 必须跟着传：它们的零值是「什么都看不到」而不是
		// 「不过滤」，漏传会拼出 1=0，导出的 CSV 永远是空文件。
		issues, _, err := s.db.ListIssues(store.IssueFilter{
			ProjectIDs: f.ProjectIDs, AllProjects: f.AllProjects,
			RepoID: f.RepoID, From: f.From, To: f.To, Limit: 100000,
		})
		if err != nil {
			return
		}
		for _, i := range issues {
			line := strconv.Itoa(i.LineStart)
			if i.LineEnd > i.LineStart {
				line = fmt.Sprintf("%d-%d", i.LineStart, i.LineEnd)
			}
			reg := ""
			if i.Regression {
				reg = "是"
			}
			_ = cw.Write([]string{strconv.FormatInt(i.ID, 10), i.RepoName, i.FilePath, line,
				i.Source, i.Category, i.Severity, i.StatusName, reg, i.Title,
				i.Committer, i.AssigneeName, i.FirstSeenAt, i.LastSeenAt})
		}
	}
}

// ---------- 共用小工具 ----------

func pageParams(r *http.Request) (limit, offset int) {
	limit = queryInt(r, "limit", 50)
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	page := queryInt(r, "page", 1)
	if page < 1 {
		page = 1
	}
	return limit, (page - 1) * limit
}

// timeRange 接受 YYYY-MM-DD 或完整时间戳，展开成库里统一的 UTC 字符串。
func timeRange(r *http.Request) (string, string) {
	parse := func(v string, endOfDay bool) string {
		if v == "" {
			return ""
		}
		if len(v) == 10 {
			if endOfDay {
				v += " 23:59:59"
			} else {
				v += " 00:00:00"
			}
		}
		if t, err := time.ParseInLocation(store.TimeFmt, v, time.Local); err == nil {
			return t.UTC().Format(store.TimeFmt)
		}
		return ""
	}
	return parse(r.URL.Query().Get("from"), false), parse(r.URL.Query().Get("to"), true)
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func timeoutCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
