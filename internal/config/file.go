package config

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 配置只从文件读，不看环境变量。
//
// 格式用 KEY=value 而不是 YAML/TOML：不引依赖，标准库几十行解析完，
// 而且 # 可以写注释——配置是给人改的。
//
// 有一类项（模型网关、通知 webhook、汇总时间）界面上也能改，改完存库。
// 那类项在本文件里填的是「初始值」，一旦在界面上动过，就以库里的为准。
const DefaultConfigName = "aiudge.conf"

// secretKeys 是敏感项。写在这个文件里是明文，读到会警告并收紧文件权限。
var secretKeys = map[string]string{
	"AIUDGE_SECRET_KEY":        "加密仓库凭据与网关密钥的主密钥",
	"AIUDGE_GATEWAY_KEY":       "LiteLLM 网关密钥",
	"AIUDGE_FEISHU_APP_SECRET": "飞书应用密钥",
}

// Values 是配置文件解析结果。读配置统一走它，而不是 os.Getenv——
// 环境变量一旦参与进来，「这个值到底从哪来的」就再也说不清了。
type Values struct {
	m    map[string]string
	Path string
	// 文件不存在、由程序生成模板时为 true。这时所有值都取默认，
	// 且要在日志里说明，否则用户会以为自己的配置生效了
	Created bool
}

func (v *Values) Str(key, def string) string {
	if s, ok := v.m[key]; ok && s != "" {
		return s
	}
	return def
}

func (v *Values) Int(key string, def int) int {
	if s, ok := v.m[key]; ok && s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
		log.Printf("[warn] 配置项 %s=%q 不是数字，按默认值 %d 处理", key, s, def)
	}
	return def
}

// List 按操作系统路径分隔符切分（Linux 上是冒号）。
func (v *Values) List(key string) []string {
	s := v.m[key]
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range filepath.SplitList(s) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Has 报告某项是否在文件里配了值，用来区分「配了空值」和「没配」。
func (v *Values) Has(key string) bool { return v.m[key] != "" }

// LoadFile 读配置文件。
//
// explicit 表示路径是命令行显式指定的（-config 真的传了）。两种情况要区别对待：
//
//	显式指定却不存在 → 直接失败。默默用默认值跑起来会让人以为配置生效了，
//	                  而实际上一个字段都没读到
//	默认路径不存在   → 生成一份全注释的模板，按默认值启动。首次运行本该如此
//
// 去掉环境变量之后没法再用 AIUDGE_CONFIG 判断，这个信息只能由调用方传进来。
func LoadFile(path string, explicit bool) *Values {
	if path == "" {
		path = DefaultConfigName
	}
	f, err := os.Open(path)
	if err != nil {
		if explicit || !os.IsNotExist(err) {
			log.Fatalf("读取配置文件 %s 失败: %v", path, err)
		}
		if err := writeTemplate(path); err != nil {
			log.Printf("[warn] 生成配置文件模板失败: %v", err)
		} else {
			log.Printf("已生成配置文件模板 %s（所有项都是注释状态，按需打开）", path)
		}
		return &Values{m: map[string]string{}, Path: path, Created: true}
	}
	defer f.Close()

	m, err := parse(f)
	if err != nil {
		log.Fatalf("解析配置文件 %s 失败: %v", path, err)
	}
	v := &Values{m: m, Path: path}

	if secrets := secretKeysPresent(m); len(secrets) > 0 {
		log.Printf("[warn] 配置文件里含明文密钥（%s）。本地开发可以，"+
			"这份文件别提交进版本库，也别拷给别人", strings.Join(secrets, ", "))
		// 有密钥就收紧权限，别的用户读到就等于拿到了这些凭据
		if err := os.Chmod(path, 0o600); err != nil {
			log.Printf("[warn] 收紧 %s 权限失败: %v", path, err)
		}
	}
	return v
}

func parse(r *os.File) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 容错 export 前缀和值两端的引号：配置文件是手写的，
		// 不该因为多打了个 export 或引号就启动不了
		line = strings.TrimPrefix(line, "export ")
		k, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("第 %d 行不是 KEY=value 格式: %q", lineNo, line)
		}
		k = strings.TrimSpace(k)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if k == "" {
			return nil, fmt.Errorf("第 %d 行缺少键名", lineNo)
		}
		out[k] = val
	}
	return out, sc.Err()
}

func secretKeysPresent(m map[string]string) []string {
	var out []string
	for k, label := range secretKeys {
		if m[k] != "" {
			out = append(out, k+"（"+label+"）")
		}
	}
	return out
}

func writeTemplate(path string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(template), 0o644)
}

// template 里所有项都是注释状态——写进去就等于替人做了决定，
// 不如把可选项摆出来让人自己勾。
const template = `# AIudge 配置文件（KEY=value，# 开头是注释）
#
# 配置只从这个文件读，不看环境变量。
# 路径由命令行 -config 指定，默认 ./aiudge.conf
#
# 带「界面可改」标记的项在这里填的是初始值：一旦在界面上改过，
# 就以界面上存的为准，本文件里的值只在还没改过时生效。

# ---------- 服务 ----------
# 监听地址
#AIUDGE_ADDR=:8787
# 对外访问地址。决定飞书登录回调地址和通知里附的后台链接，
# 填成别人能访问到的地址，且要和飞书后台配的「重定向 URL」逐字符一致
#AIUDGE_BASE_URL=http://127.0.0.1:8787

# ---------- 数据 ----------
# 数据目录（SQLite、仓库工作副本），相对路径按启动目录算
#AIUDGE_DATA_DIR=./data
# 仓库工作副本目录，默认 <数据目录>/repos
#AIUDGE_WORK_DIR=./data/repos

# ---------- 密钥 ----------
# 加密主密钥：64 位 hex（openssl rand -hex 32）。
# 不设的话自动生成到 <数据目录>/secret.key（0600）。
# 【注意】这个值必须固定下来：换一个，已存的仓库凭据、网关密钥、飞书 webhook
# 就全解不开了，得重新填一遍。多台机器共用一份数据时尤其要固定。
# 写在文件里是明文，别提交进版本库
#AIUDGE_SECRET_KEY=

# ---------- 模型网关（界面可改）----------
# 不填则去界面上「设置 → 模型配置」填
#AIUDGE_GATEWAY_URL=http://127.0.0.1:4000
#AIUDGE_GATEWAY_KEY=

# ---------- 飞书 ----------
# 配了 APP_ID + APP_SECRET 就启用扫码登录，开发登录入口自动关闭
#AIUDGE_FEISHU_APP_ID=
#AIUDGE_FEISHU_APP_SECRET=
# 群机器人 webhook，全局兜底（界面可改）。每个仓库可以在界面上单独配自己的群
#AIUDGE_FEISHU_WEBHOOK=
# 私有化飞书换成自建域名，留空用 open.feishu.cn
#AIUDGE_FEISHU_OAUTH_BASE=

# ---------- 任务 ----------
# 并发审核任务数，用来避开网关限流
#AIUDGE_CONCURRENCY=2
# 失败重试次数（只对 5xx / 429 重试，配置类错误立即失败）
#AIUDGE_MAX_RETRY=3
# 软超时（分钟）：只告警
#AIUDGE_SOFT_TIMEOUT_MIN=10
# 硬超时（分钟）：终止并标记失败
#AIUDGE_HARD_TIMEOUT_MIN=30
# golangci-lint 可执行文件
#AIUDGE_LINT_BIN=golangci-lint
#AIUDGE_LINT_TIMEOUT_MIN=5
# 每日汇总推送时间（cron），空则不推。界面可改，界面上填的优先
#AIUDGE_SUMMARY_CRON=30 18 * * *

# ---------- 规范文档 ----------
# 允许挂载为审核规范文档的绝对路径白名单，冒号分隔。
# 推荐用仓库内相对路径挂载（如 CLAUDE.md），零配置且天然受限，不用配这个
#AIUDGE_SPEC_DIRS=
`
