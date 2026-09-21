package queue

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"aiudge/internal/store"
)

// Scheduler 管两类定时任务：仓库的定时兜底扫描（§4.1）与每日汇总推送（§4.7）。
// 仓库配置改变后调用 Reload 重建，避免为了改 cron 表达式重启进程。
type Scheduler struct {
	q *Queue
	c *cron.Cron
}

func (q *Queue) NewScheduler() *Scheduler {
	return &Scheduler{q: q, c: cron.New()}
}

func (s *Scheduler) Start() {
	if err := s.Reload(); err != nil {
		log.Printf("[sched] 载入定时任务失败: %v", err)
	}
	s.c.Start()
}

func (s *Scheduler) Stop() {
	ctx := s.c.Stop()
	<-ctx.Done()
}

// Reload 重建全部定时任务。仓库数量是个位数，整体重建比增量维护简单得多。
func (s *Scheduler) Reload() error {
	for _, e := range s.c.Entries() {
		s.c.Remove(e.ID)
	}

	repos, err := s.q.db.ListAllRepos()
	if err != nil {
		return err
	}
	for _, r := range repos {
		if !r.Enabled || r.TriggerCron == "" {
			continue
		}
		repo := r
		if _, err := s.c.AddFunc(repo.TriggerCron, func() { s.runRepoScan(repo) }); err != nil {
			log.Printf("[sched] 仓库 %s 的 cron 表达式 %q 非法: %v", repo.Name, repo.TriggerCron, err)
			continue
		}
		log.Printf("[sched] 仓库 %s 定时兜底: %s", repo.Name, repo.TriggerCron)
	}

	if expr := s.q.SummaryCron(); expr != "" {
		if _, err := s.c.AddFunc(expr, s.dailySummary); err != nil {
			log.Printf("[sched] 汇总推送 cron %q 非法: %v", expr, err)
		}
	}
	return nil
}

// runRepoScan 只建任务入队，扫描范围（24 小时内未被覆盖的 commit）由管线判定，
// 避免在调度线程里做任何网络或 git 操作。
func (s *Scheduler) runRepoScan(repo store.Repository) {
	task := &store.Task{
		RepoID:      repo.ID,
		TriggerType: "cron",
		Status:      "queued",
		ModelName:   repo.DefaultModel,
	}
	if err := s.q.db.CreateTask(task); err != nil {
		log.Printf("[sched] 创建定时任务失败 仓库=%s: %v", repo.Name, err)
		return
	}
	log.Printf("[sched] 定时兜底入队 仓库=%s 任务=#%d", repo.Name, task.ID)
	s.q.Submit(task.ID)
}

func (s *Scheduler) dailySummary() {
	day := time.Now().Format("2006-01-02")
	from, to := day+" 00:00:00", day+" 23:59:59"

	repos, err := s.q.db.ListAllRepos()
	if err != nil {
		log.Printf("[sched] 读取仓库失败: %v", err)
		return
	}

	// 日报按仓库分流：各自发到各自的群。没配自己 webhook 的仓库合并成一份发全局兜底，
	// 否则多个仓库共用一个群时会收到一串格式雷同、只有仓库名不同的消息。
	var ownHooks, fallback []store.Repository
	for _, r := range repos {
		if !r.Enabled {
			continue
		}
		if h, err := s.q.db.RepoWebhook(r.ID); err == nil && h != "" {
			ownHooks = append(ownHooks, r)
		} else {
			fallback = append(fallback, r)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, r := range ownHooks {
		o, err := s.q.db.Overview(store.StatsFilter{RepoID: r.ID, From: from, To: to})
		if err != nil {
			log.Printf("[sched] 仓库 %s 汇总失败: %v", r.Name, err)
			continue
		}
		if err := s.q.feishu.DailySummary(ctx, s.q.WebhookFor(r.ID), r.Name, day, o); err != nil {
			log.Printf("[sched] 仓库 %s 日报推送失败: %v", r.Name, err)
		}
	}

	if len(fallback) == 0 {
		return
	}
	ids := make([]int64, 0, len(fallback))
	names := make([]string, 0, len(fallback))
	for _, r := range fallback {
		ids = append(ids, r.ID)
		names = append(names, r.Name)
	}
	o, err := s.q.db.Overview(store.StatsFilter{RepoIDs: ids, From: from, To: to})
	if err != nil {
		log.Printf("[sched] 汇总失败: %v", err)
		return
	}
	label := names[0]
	if len(names) > 1 {
		label = fmt.Sprintf("%d 个仓库（%s）", len(names), strings.Join(names, "、"))
	}
	if err := s.q.feishu.DailySummary(ctx, "", label, day, o); err != nil {
		log.Printf("[sched] 日报推送失败: %v", err)
	}
}
