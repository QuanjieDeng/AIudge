package store

import "strings"

// projectPredicate 返回一个**完整的 SQL 谓词**（不含 WHERE 关键字），col 是项目 id 列名。
//
//	all=true        → 返回空串，调用方不加条件
//	无可见项目       → 返回 "1=0"，一条都查不出来
//	否则            → 形如 "col IN (?,?)"
//
// 关键是返回完整谓词而不是片段：早先的版本返回 `IN (?,?)` 这样的片段，
// 被两个不同语境复用（`WHERE <片段>` 和 `project_id <片段>`），
// 空可见集时会拼出 `project_id 1=0` 这种语法错误的 SQL。
func projectPredicate(col string, pids []int64, all bool) (string, []any) {
	if all {
		return "", nil
	}
	if len(pids) == 0 {
		return "1=0", nil
	}
	ph := make([]string, len(pids))
	args := make([]any, len(pids))
	for i, id := range pids {
		ph[i] = "?"
		args[i] = id
	}
	return col + " IN (" + strings.Join(ph, ",") + ")", args
}

// repoVisibleCond 生成「仓库属于可见项目」的谓词，用来过滤以仓库为锚点的表
// （issues / review_reports / review_tasks）。子查询足够简单，不必到处 JOIN。
func repoVisibleCond(repoCol string, pids []int64, all bool) (string, []any) {
	inner, args := projectPredicate("project_id", pids, all)
	if inner == "" {
		return "", nil
	}
	return repoCol + " IN (SELECT id FROM repositories WHERE " + inner + ")", args
}
