// Package config 收集运行期配置。配置只从命令行 -config 指定的文件读，
// 不看环境变量——环境变量参与进来之后，「这个值到底从哪来的」就再也说不清了。
//
// 有一类项界面上也能改（模型网关、通知 webhook、汇总时间），那类项在本文件里
// 填的是初始值，一旦在界面上动过就以库里的为准。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	Addr    string // HTTP 监听地址
	DataDir string // 数据目录：SQLite、仓库工作副本
	DBPath  string

	SecretKey []byte // AES-256-GCM 主密钥，加密仓库凭据、webhook、网关密钥

	WorkDir     string // git 浅克隆工作副本根目录
	Concurrency int    // 并发审核任务上限，避免网关限流
	MaxRetry    int
	SoftTimeout time.Duration // 软超时：告警
	HardTimeout time.Duration // 硬超时：终止并标记失败

	LintTimeout time.Duration
	LintBin     string

	// 以下是「界面可改」项的文件初始值。库里存了就以库里的为准，
	// 判定的地方在各自的取用处（web / queue），不在这里。
	GatewayURL    string
	GatewayKey    string
	FeishuWebhook string
	SummaryCron   string

	// 飞书应用凭据只从文件读——它们决定登录方式，属于启动参数
	FeishuAppID     string
	FeishuAppSecret string
	FeishuOAuthBase string
	BaseURL         string // 拼后台链接与飞书回调地址用

	SpecDirs []string // 挂载规范文档时允许读取的绝对路径白名单

	// 实际加载的配置文件路径，启动日志里报出来——「改了没生效」时
	// 第一件要确认的就是它到底读的哪个文件
	ConfigPath string
}

// Load 读配置文件并组装配置。explicit 表示 -config 是显式传的，见 LoadFile。
func Load(path string, explicit bool) *Config {
	v := LoadFile(path, explicit) // 见 file.go

	c := &Config{
		Addr:    v.Str("AIUDGE_ADDR", ":8787"),
		DataDir: v.Str("AIUDGE_DATA_DIR", "./data"),

		Concurrency: v.Int("AIUDGE_CONCURRENCY", 2),
		MaxRetry:    v.Int("AIUDGE_MAX_RETRY", 3),
		SoftTimeout: time.Duration(v.Int("AIUDGE_SOFT_TIMEOUT_MIN", 10)) * time.Minute,
		HardTimeout: time.Duration(v.Int("AIUDGE_HARD_TIMEOUT_MIN", 30)) * time.Minute,
		LintTimeout: time.Duration(v.Int("AIUDGE_LINT_TIMEOUT_MIN", 5)) * time.Minute,
		LintBin:     v.Str("AIUDGE_LINT_BIN", "golangci-lint"),

		GatewayURL:    v.Str("AIUDGE_GATEWAY_URL", ""),
		GatewayKey:    v.Str("AIUDGE_GATEWAY_KEY", ""),
		FeishuWebhook: v.Str("AIUDGE_FEISHU_WEBHOOK", ""),
		SummaryCron:   v.Str("AIUDGE_SUMMARY_CRON", "30 18 * * *"),

		FeishuAppID:     v.Str("AIUDGE_FEISHU_APP_ID", ""),
		FeishuAppSecret: v.Str("AIUDGE_FEISHU_APP_SECRET", ""),
		FeishuOAuthBase: v.Str("AIUDGE_FEISHU_OAUTH_BASE", ""),
		BaseURL:         v.Str("AIUDGE_BASE_URL", "http://127.0.0.1:8787"),

		SpecDirs:   v.List("AIUDGE_SPEC_DIRS"),
		ConfigPath: v.Path,
	}
	c.DBPath = v.Str("AIUDGE_DB", filepath.Join(c.DataDir, "aiudge.db"))
	c.WorkDir = v.Str("AIUDGE_WORK_DIR", filepath.Join(c.DataDir, "repos"))

	for _, d := range []string{c.DataDir, c.WorkDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("创建目录 %s 失败: %v", d, err)
		}
	}
	c.SecretKey = loadSecretKey(v, c.DataDir)
	return c
}

// loadSecretKey 取主密钥：优先配置文件里的 AIUDGE_SECRET_KEY，
// 没配就退化为数据目录下的 0600 随机密钥文件，保证裸机也能跑起来。
func loadSecretKey(v *Values, dataDir string) []byte {
	if s := v.Str("AIUDGE_SECRET_KEY", ""); s != "" {
		k, err := decodeKey(s)
		if err != nil {
			log.Fatalf("配置文件里的 AIUDGE_SECRET_KEY 非法: %v", err)
		}
		return k
	}
	path := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil {
		if k, err := decodeKey(string(b)); err == nil {
			return k
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		log.Fatalf("生成密钥失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)), 0o600); err != nil {
		log.Fatalf("写密钥文件失败: %v", err)
	}
	log.Printf("[warn] 配置文件里没设 AIUDGE_SECRET_KEY，已生成 %s。"+
		"这个值要固定下来：换一个，已存的仓库凭据、网关密钥、飞书 webhook 就全解不开了", path)
	return k
}

func decodeKey(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("需要 64 位 hex 的 32 字节密钥（openssl rand -hex 32）")
	}
	return b, nil
}
