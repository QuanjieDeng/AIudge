package store

import "fmt"

// ResetResult 记录各表清掉了多少行，让调用方能如实回报而不是笼统说「已清空」。
type ResetResult map[string]int

// businessTables 是「审核跑出来的数据」。配置类表（repositories / model_configs /
// users / committer_identities / subsystems）不在其中——测试阶段反复清档是为了丢掉
// 跑出来的噪音，不是把仓库和网关重配一遍。
var businessTables = []string{
	"report_issues", "issues", "comments", "issue_events",
	"review_reports", "task_logs", "review_tasks",
	"review_marks", "audit_logs",
}

// ResetData 清空业务数据并把自增 id 归零。
//
// 只被管理员在测试阶段使用，所以不做删除前的备份——真要留痕，清完之后写一条审计即可
// （审计表本身也被清了，写在这之后才有意义）。
func (d *DB) ResetData() (ResetResult, error) {
	res := ResetResult{}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, t := range businessTables {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
			return nil, fmt.Errorf("统计 %s 失败: %w", t, err)
		}
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return nil, fmt.Errorf("清空 %s 失败: %w", t, err)
		}
		if n > 0 {
			res[t] = n
		}
	}

	// 自增序列也归零，下次任务从 #1 开始——测试时对着一堆 #47 的数看着累
	for _, t := range businessTables {
		// sqlite_sequence 只在库里有 AUTOINCREMENT 表时才存在，没有时报错忽略即可
		_, _ = tx.Exec(`DELETE FROM sqlite_sequence WHERE name=?`, t)
	}
	return res, tx.Commit()
}

// CountBusinessRows 返回当前业务数据的规模，清档前给界面一个「将有 X 条被删除」的交代。
func (d *DB) CountBusinessRows() (ResetResult, error) {
	res := ResetResult{}
	for _, t := range businessTables {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			res[t] = n
		}
	}
	return res, nil
}
