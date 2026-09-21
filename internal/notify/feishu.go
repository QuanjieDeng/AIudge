// Package notify 只做飞书（§附录决策 12：项目内只有飞书是通的）。
// 消息体格式与 server/base/launch/alarm.go 保持一致，复用同一套 webhook 能力。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aiudge/internal/store"
)

// Feishu 是发送器，不持有某个具体的 webhook。收件地址按仓库走：
// 不同项目发不同的群，仓库没配就回落到全局默认（AIUDGE_FEISHU_WEBHOOK）。
type Feishu struct {
	Default string // 全局兜底
	BaseURL string // 后台地址，通知里附链接
	Client  *http.Client
}

func New(webhook, baseURL string) *Feishu {
	return &Feishu{
		Default: webhook,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 8 * time.Second},
	}
}

// Enabled 表示至少有一个可用的收件地址。全局没配时，仓库自己配了也照样能发，
// 所以这里只看兜底值是不够的——真正的判断在 Send 里。
func (f *Feishu) Enabled() bool { return f != nil && f.Default != "" }

// hook 解析实际收件地址：仓库自己的优先，没配就用全局兜底。
func (f *Feishu) hook(repoHook string) string {
	if f == nil {
		return ""
	}
	if repoHook != "" {
		return repoHook
	}
	return f.Default
}

// Send 发一条纯文本。两个地址都没有时静默跳过，调用方不必到处判断。
// hook 是仓库自己的 webhook，传空则用全局兜底。
func (f *Feishu) Send(ctx context.Context, hook, text string) error {
	to := f.hook(hook)
	if to == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, to, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("飞书 webhook HTTP %d", resp.StatusCode)
	}
	return nil
}

// HighIssue 是高危问题的即时推送（§4.7），带上指派人 @ 与后台链接。
// atUserID 是飞书 open_id；没有时降级为纯文本，不让通知整体失败。
func (f *Feishu) HighIssue(ctx context.Context, hook, repoName string, iss *store.Issue, atUserID string) error {
	var sb strings.Builder
	sb.WriteString("🔴 [高危代码问题]\n")
	if atUserID != "" {
		fmt.Fprintf(&sb, "<at user_id=%q></at> ", atUserID)
	}
	fmt.Fprintf(&sb, "仓库: %s\n", repoName)
	fmt.Fprintf(&sb, "位置: %s:%d", iss.FilePath, iss.LineStart)
	if iss.LineEnd > iss.LineStart {
		fmt.Fprintf(&sb, "-%d", iss.LineEnd)
	}
	fmt.Fprintf(&sb, "\n分类: %s\n", iss.Category)
	fmt.Fprintf(&sb, "问题: %s\n", iss.Title)
	if iss.Description != "" {
		fmt.Fprintf(&sb, "说明: %s\n", capText(iss.Description, 300))
	}
	if iss.Committer != "" {
		fmt.Fprintf(&sb, "提交人: %s\n", iss.Committer)
	}
	fmt.Fprintf(&sb, "详情: %s/#/issues/%d", f.BaseURL, iss.ID)
	return f.Send(ctx, hook, sb.String())
}

// DailySummary 是每日汇总推送（§4.7）。
func (f *Feishu) DailySummary(ctx context.Context, hook, repoName, day string, o *store.Overview) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 [代码审核日报 %s] %s\n", day, repoName)
	fmt.Fprintf(&sb, "新增问题: %d（高危 %d）\n", o.NewCount, o.HighCount)
	fmt.Fprintf(&sb, "待处理: %d\n", o.Pending)
	fmt.Fprintf(&sb, "已处理: %d，误报: %d\n", o.Fixed, o.FalsePos)
	if o.NewCount > 0 {
		fmt.Fprintf(&sb, "误报率: %.1f%%\n", o.FalseRate*100)
	}
	fmt.Fprintf(&sb, "待处理清单: %s/#/issues?status=pending", f.BaseURL)
	return f.Send(ctx, hook, sb.String())
}

// TaskFailed 在重试耗尽后告警，不静默失败（§4.3）。
func (f *Feishu) TaskFailed(ctx context.Context, hook string, task *store.Task, errMsg string) error {
	text := fmt.Sprintf("⚠️ [审核任务失败]\n仓库: %s\n任务: #%d（%s）\n已重试 %d 次\n原因: %s\n日志: %s/#/tasks/%d",
		task.RepoName, task.ID, task.TriggerType, task.RetryCount, capText(errMsg, 400), f.BaseURL, task.ID)
	return f.Send(ctx, hook, text)
}

// SoftTimeout 软超时告警（§5：软超时 10 分钟触发告警，硬超时 30 分钟终止）。
func (f *Feishu) SoftTimeout(ctx context.Context, hook string, task *store.Task, elapsed time.Duration) error {
	text := fmt.Sprintf("⏳ [审核任务超时告警]\n仓库: %s\n任务: #%d 已运行 %.0f 分钟，超过软超时阈值\n日志: %s/#/tasks/%d",
		task.RepoName, task.ID, elapsed.Minutes(), f.BaseURL, task.ID)
	return f.Send(ctx, hook, text)
}

func capText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
