package review

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aiudge/internal/config"
	"aiudge/internal/lint"
	"aiudge/internal/store"
)

// TestABExperiment 是一次性 A/B 脚手架：把生产管线原样跑在隔离库上（/tmp/ab），
// 每个提交跑完把「提示词快照 + 入库的问题 + 任务日志」写到 /tmp/ab/out。
//
//	AIUDGE_AB=1 AB_COMMITS=sha1,sha2 AB_TAG=api8 AB_ROUNDS=8 AB_FAKE=1 go test ./internal/review -run TestABExperiment -v
//
// AB_FAKE=1 换成假网关，只抓提示词、不调模型（零成本）。用完删掉本文件。
func TestABExperiment(t *testing.T) {
	if os.Getenv("AIUDGE_AB") == "" {
		t.Skip("A/B 实验脚手架，设 AIUDGE_AB=1 才跑（会真的调模型花钱）")
	}
	outDir := "/tmp/ab/out"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tag := envOr("AB_TAG", "api")
	if r := os.Getenv("AB_ROUNDS"); r != "" {
		n, err := strconv.Atoi(r)
		if err != nil || n <= 0 {
			t.Fatalf("AB_ROUNDS=%q 不是正整数", r)
		}
		maxToolRounds = n
	}
	t.Logf("arm=%s maxToolRounds=%d", tag, maxToolRounds)

	cfg := config.Load("/tmp/ab/aiudge.conf", true)
	db, err := store.Open(cfg.DBPath, cfg.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := db.GetRepo(1)
	if err != nil || repo.Name != "slg_server" {
		t.Fatalf("隔离库里的仓库不对：%+v %v", repo, err)
	}
	if m := os.Getenv("AB_MODEL"); m != "" {
		// 两个 arm 必须用同一个模型，否则比的就不是「壳」了
		if _, err := db.Exec(`update repositories set default_model=? where id=1`, m); err != nil {
			t.Fatal(err)
		}
	}

	// 网关地址每轮都显式写一遍：capture 那轮会把库里的值改成假网关的临时端口，
	// 不写回来的话下一轮就对着一个已经关掉的端口发请求，5 个任务全秒失败。
	// 假网关：把每次请求的 body 原样存下来，回一句「没问题」结束循环。
	gw := envOr("AB_GATEWAY", "http://10.254.40.145")
	cur := ""
	if os.Getenv("AB_FAKE") != "" {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = os.WriteFile(filepath.Join(outDir, "req-"+cur+".json"), b, 0o644)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"issues\":[]}"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`)
		}))
		defer srv.Close()
		gw = srv.URL
	}
	if _, err := db.Exec(`update model_configs set gateway_url=? where id=1`, gw); err != nil {
		t.Fatal(err)
	}
	t.Logf("gateway=%s", gw)

	p := &Pipeline{Cfg: cfg, DB: db, Lint: lint.Runner{Bin: cfg.LintBin, Timeout: cfg.LintTimeout}}
	for _, sha := range strings.Split(os.Getenv("AB_COMMITS"), ",") {
		sha = strings.TrimSpace(sha)
		if sha == "" {
			continue
		}
		cur = sha
		task := &store.Task{
			RepoID: repo.ID, TriggerType: "manual",
			CommitFrom: sha + "^", CommitTo: sha, // Log 是 since..HEAD 左开，所以起点取父提交
			Status: "queued", QueuedAt: store.Now(),
		}
		if err := db.CreateTask(task); err != nil {
			t.Fatal(err)
		}
		logf := func(level, format string, args ...any) {
			fmt.Fprintf(os.Stdout, "[%s] %s\n", level, fmt.Sprintf(format, args...))
		}
		res, err := p.Run(context.Background(), task.ID, logf)
		if err != nil {
			t.Errorf("%s 跑失败: %v", sha[:8], err)
			continue
		}
		_ = dumpArm(t, db, task.ID, res, filepath.Join(outDir, tag+"-"+sha[:8]+".json"))
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// dumpArm 把这一 arm 的事实落成 JSON：入库的问题、任务日志、以及最终统计。
// 提示词快照由假网关单独写（req-<sha>.json）。
func dumpArm(t *testing.T, db *store.DB, taskID int64, res *Result, path string) error {
	out := map[string]any{"task_id": taskID, "issues_reported": res.Issues, "high": res.High}

	rows, err := db.Query(`select i.id,i.source,i.category,i.severity,i.file_path,i.line_start,i.line_end,
		i.title,i.description,i.suggestion,i.code_snippet,i.linter
		from review_reports r join report_issues ri on ri.report_id=r.id join issues i on i.id=ri.issue_id
		where r.task_id=? order by i.id`, taskID)
	if err != nil {
		return err
	}
	issues := []map[string]any{}
	for rows.Next() {
		var id, ls, le int64
		var source, cat, sev, fp, title, desc, sug, code, linter string
		if err := rows.Scan(&id, &source, &cat, &sev, &fp, &ls, &le, &title, &desc, &sug, &code, &linter); err != nil {
			rows.Close()
			return err
		}
		issues = append(issues, map[string]any{"source": source, "category": cat, "severity": sev,
			"file": fp, "line_start": ls, "line_end": le, "title": title,
			"description": desc, "suggestion": sug, "code": code, "linter": linter})
	}
	rows.Close() // SQLite 单连接：游标不关，后面那条查询会把进程挂死
	out["issues"] = issues

	rows, err = db.Query(`select level,message from task_logs where task_id=? order by id`, taskID)
	if err == nil {
		logs := []string{}
		for rows.Next() {
			var lv, msg string
			if err := rows.Scan(&lv, &msg); err != nil {
				break
			}
			logs = append(logs, lv+": "+msg)
		}
		rows.Close()
		out["logs"] = logs
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	t.Logf("%s → %d 条问题", filepath.Base(path), len(issues))
	return os.WriteFile(path, b, 0o644)
}
