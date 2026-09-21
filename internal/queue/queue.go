// Package queue 是审核任务的异步执行与调度（§4.3）。
// 并发上限的用途是避开网关限流，指数退避重试保证不静默失败。
package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"aiudge/internal/config"
	"aiudge/internal/llm"
	"aiudge/internal/notify"
	"aiudge/internal/review"
	"aiudge/internal/store"
)

type Queue struct {
	cfg    *config.Config
	db     *store.DB
	pipe   *review.Pipeline
	feishu *notify.Feishu

	ch       chan int64
	wg       sync.WaitGroup
	closed   bool
	draining bool
	idle     atomic.Int64 // 正在执行的任务数
	mu       sync.Mutex
}

// WebhookFor 解析实际用的飞书收件地址，三层：
//
//	仓库自己的  >  界面配置的全局  >  配置文件里的 AIUDGE_FEISHU_WEBHOOK
//
// 配置文件放最后是兜底；界面能改的放前面，否则「界面上配了
// 却不生效」比不配还难查。web 层发指派通知时也走这里，口径统一。
func (q *Queue) WebhookFor(repoID int64) string {
	if repoID > 0 {
		if hook, err := q.db.RepoWebhook(repoID); err == nil && hook != "" {
			return hook
		}
	}
	if hook, err := q.db.NotifyWebhook(); err == nil && hook != "" {
		return hook
	}
	return q.cfg.FeishuWebhook
}

// SummaryCron 同理：界面配的优先，配置文件里的兜底。
func (q *Queue) SummaryCron() string {
	if c, err := q.db.NotifyConfig(); err == nil && c.SummaryCron != "" {
		return c.SummaryCron
	}
	return q.cfg.SummaryCron
}

// Drain 等正在跑的任务收尾，并在等待期间拒绝新的入队。
// 清档用：不拦的话，跑了一半的任务会在数据被清空之后把报告写回来，
// 于是「清空」之后还剩下最后那一条，看起来像没清干净。
func (q *Queue) Drain(ctx context.Context) {
	q.mu.Lock()
	q.draining = true
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.draining = false
		q.mu.Unlock()
	}()

	// channel 里没有「在跑几个」的计数，用轮询最省事：
	// 清档是低频人工操作，几秒钟的轮询成本可以忽略
	for q.idle.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func New(cfg *config.Config, db *store.DB, pipe *review.Pipeline, feishu *notify.Feishu) *Queue {
	return &Queue{cfg: cfg, db: db, pipe: pipe, feishu: feishu, ch: make(chan int64, 256)}
}

// Start 拉起 cfg.Concurrency 个 worker。并发数写死在启动时，运行期改配置需重启，
// 对 4~5 人的团队规模够用。
func (q *Queue) Start() {
	for i := 0; i < q.cfg.Concurrency; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for id := range q.ch {
				q.runOne(id)
			}
		}()
	}
}

func (q *Queue) Stop() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	close(q.ch)
	q.mu.Unlock()
	q.wg.Wait()
}

func (q *Queue) Submit(taskID int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.draining {
		return
	}
	select {
	case q.ch <- taskID:
	default:
		// 队列满说明积压严重，任务在库里仍是 queued，重启或下轮定时会捡回来
		log.Printf("[queue] 队列已满，任务 #%d 暂缓入队", taskID)
	}
}

func (q *Queue) runOne(taskID int64) {
	q.idle.Add(1)
	defer q.idle.Add(-1)

	task, err := q.db.GetTask(taskID)
	if err != nil {
		log.Printf("[queue] 任务 #%d 不存在: %v", taskID, err)
		return
	}
	if task.Status == "success" {
		return // 重试定时器与手工触发可能撞车，成功过的不再跑
	}
	if err := q.db.SetTaskStatus(taskID, "running", ""); err != nil {
		log.Printf("[queue] 更新任务状态失败: %v", err)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), q.cfg.HardTimeout)
	defer cancel()

	// 软超时只告警不终止：任务可能正卡在一次大模型请求上，砍掉反而浪费已花的成本
	soft := time.AfterFunc(q.cfg.SoftTimeout, func() {
		if q.db.SoftWarned(taskID) {
			return
		}
		_ = q.db.MarkSoftWarned(taskID)
		q.db.AppendTaskLog(taskID, "warn", fmt.Sprintf("已运行 %.1f 分钟，超过软超时阈值", time.Since(start).Minutes()))
		_ = q.feishu.SoftTimeout(context.Background(), q.WebhookFor(task.RepoID), task, time.Since(start))
	})
	defer soft.Stop()

	logf := func(level, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		q.db.AppendTaskLog(taskID, level, msg)
		log.Printf("[task %d][%s] %s", taskID, level, msg)
	}

	res, err := q.pipe.Run(ctx, taskID, logf)
	if err != nil {
		q.handleFailure(task, err, logf)
		return
	}
	if err := q.db.SetTaskStatus(taskID, "success", ""); err != nil {
		logf("warn", "更新任务状态失败: %v", err)
	}
	q.pushHigh(task, res)
}

// handleFailure 做指数退避重试；重试耗尽后告警，绝不静默失败（§4.3）。
func (q *Queue) handleFailure(task *store.Task, err error, logf review.Logf) {
	if !retryable(err) {
		// 模型名写错、密钥无效这类配置问题，等三分钟再试三次也还是同一个错，
		// 只会把真正的错因埋进一堆重复日志里。直接失败，让人立刻看到。
		logf("error", "配置类错误，不再重试: %v", err)
		_ = q.db.SetTaskStatus(task.ID, "failed", err.Error())
		if t, e := q.db.GetTask(task.ID); e == nil {
			_ = q.feishu.TaskFailed(context.Background(), q.WebhookFor(task.RepoID), t, err.Error())
		}
		return
	}
	if task.RetryCount < q.cfg.MaxRetry {
		backoff := time.Duration(1<<uint(task.RetryCount)) * time.Minute
		logf("warn", "第 %d 次失败（%s 后重试）: %v", task.RetryCount+1, backoff, err)
		if e := q.db.RetryTask(task.ID); e != nil {
			logf("error", "重置任务状态失败: %v", e)
			return
		}
		time.AfterFunc(backoff, func() { q.Submit(task.ID) })
		return
	}
	logf("error", "重试 %d 次后仍失败: %v", task.RetryCount, err)
	_ = q.db.SetTaskStatus(task.ID, "failed", err.Error())
	if t, e := q.db.GetTask(task.ID); e == nil {
		_ = q.feishu.TaskFailed(context.Background(), q.WebhookFor(task.RepoID), t, err.Error())
	}
}

// retryable 判定错误是否值得重试。配置类错误（模型没配、模型名不存在、凭据解不开）
// 重试只是把同一句话再说三遍，还白等 1+2+4 分钟的退避——真跑过才知道有多烦人。
func retryable(err error) bool {
	if errors.Is(err, review.ErrConfig) {
		return false
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	return true // 网络抖动、git 超时等默认仍给重试机会
}

// pushHigh 高危问题即时推送（§4.7）。只推本次新建的高危，重复命中的不重复打扰。
func (q *Queue) pushHigh(task *store.Task, res *review.Result) {
	if res == nil || len(res.NewHigh) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range res.NewHigh {
		iss := &res.NewHigh[i]
		at := ""
		if iss.AssigneeID > 0 {
			at = q.db.FeishuID(iss.AssigneeID)
		} else if iss.CommitterID > 0 {
			at = q.db.FeishuID(iss.CommitterID)
		}
		if err := q.feishu.HighIssue(ctx, q.WebhookFor(task.RepoID), task.RepoName, iss, at); err != nil {
			q.db.AppendTaskLog(task.ID, "warn", fmt.Sprintf("高危推送失败 issue#%d: %v", iss.ID, err))
		}
	}
}
