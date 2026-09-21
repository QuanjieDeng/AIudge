// AIudge —— AI 代码审核后台系统。
//
// 单二进制：内嵌前端 + 内嵌 SQLite（纯 Go 驱动，无 cgo），不需要外部中间件。
// 配置来自 -config 指定的文件（默认 ./aiudge.conf），不读环境变量。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"aiudge/internal/config"
	"aiudge/internal/lint"
	"aiudge/internal/notify"
	"aiudge/internal/queue"
	"aiudge/internal/review"
	"aiudge/internal/store"
	"aiudge/internal/web"
)

func main() {
	showVersion := flag.Bool("version", false, "打印版本并退出")
	cfgPath := flag.String("config", config.DefaultConfigName, "配置文件路径")
	flag.Parse()
	// 区分「显式传了 -config」和「用的默认值」：前者指向的文件不存在就该报错，
	// 后者不存在是首次运行，生成模板继续
	explicitConfig := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicitConfig = true
		}
	})
	if *showVersion {
		log.Println("aiudge prompt-" + review.PromptVersion(review.DefaultSystemPrompt()))
		return
	}
	log.SetFlags(log.LstdFlags)

	cfg := config.Load(*cfgPath, explicitConfig)
	db, err := store.Open(cfg.DBPath, cfg.SecretKey)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	reportOverridden(cfg, db)

	feishu := notify.New(cfg.FeishuWebhook, cfg.BaseURL)
	if !feishu.Enabled() {
		log.Println("[warn] 未配置 AIUDGE_FEISHU_WEBHOOK，通知与高危推送将静默跳过")
	}

	pipe := &review.Pipeline{
		Cfg:  cfg,
		DB:   db,
		Lint: lint.Runner{Bin: cfg.LintBin, Timeout: cfg.LintTimeout},
	}
	q := queue.New(cfg, db, pipe, feishu)
	q.Start()

	sched := q.NewScheduler()
	sched.Start()
	defer sched.Stop()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           web.New(cfg, db, q, sched, feishu).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if cfg.ConfigPath != "" {
			log.Printf("AIudge 已启动: %s（数据目录 %s，并发 %d，配置 %s）",
				cfg.Addr, cfg.DataDir, cfg.Concurrency, cfg.ConfigPath)
		} else {
			log.Printf("AIudge 已启动: %s（数据目录 %s，并发 %d，无配置文件）",
				cfg.Addr, cfg.DataDir, cfg.Concurrency)
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务退出: %v", err)
		}
	}()

	// 收到信号后先停 HTTP（不再接任务），再等队列把手上的审核跑完
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	q.Stop()
	log.Println("已退出")
}

// reportOverridden 报出哪些「界面可改」项在配置文件里填了、但被库里的值盖掉了。
//
// 这类项有两条来源，不说清楚就会出现「我在配置文件里改了网关地址怎么没生效」——
// 而真实原因是它早就在界面上被改过，库里存的那份优先。
func reportOverridden(cfg *config.Config, db *store.DB) {
	var shadowed []string

	if mc, err := db.GetModelConfig(); err == nil {
		if mc.GatewayURL != "" && cfg.GatewayURL != "" && mc.GatewayURL != cfg.GatewayURL {
			shadowed = append(shadowed, "模型网关地址（界面: "+mc.GatewayURL+"）")
		}
		if mc.DefaultModel != "" {
			// 默认模型只能从界面配，配置文件里没有这一项，不用比
			_ = mc
		}
	}
	if hook, err := db.NotifyWebhook(); err == nil && hook != "" && cfg.FeishuWebhook != "" {
		shadowed = append(shadowed, "全局通知 webhook（界面已配）")
	}
	if c, err := db.NotifyConfig(); err == nil && c.SummaryCron != "" && cfg.SummaryCron != "" &&
		c.SummaryCron != cfg.SummaryCron {
		shadowed = append(shadowed, "每日汇总时间（界面: "+c.SummaryCron+"）")
	}

	if len(shadowed) > 0 {
		log.Printf("以下项在配置文件 %s 里填了，但已被界面上的设置覆盖: %s。"+
			"以界面为准；想改回配置文件，先去界面上清掉", cfg.ConfigPath, strings.Join(shadowed, "、"))
	}
}
