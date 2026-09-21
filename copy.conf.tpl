# AIudge 配置文件（KEY=value，# 开头是注释）
#
# 加载顺序：内置默认值  <  本文件  <  环境变量
# 环境变量永远优先，所以部署时可以用 k8s Secret 覆盖这里的任何一项。
#
# 本文件的路径由 AIUDGE_CONFIG 指定，默认 ./aiudge.conf

# ---------- 服务 ----------
# 监听地址
#AIUDGE_ADDR=:8787
# 对外访问地址。决定飞书登录回调地址和通知里附的后台链接，
# 填成别人能访问到的地址，且要和飞书后台配的「重定向 URL」逐字符一致
# AIUDGE_BASE_URL=
# ---------- 数据 ----------
# 数据目录（SQLite、仓库工作副本），相对路径按启动目录算
#AIUDGE_DATA_DIR=./data
# 仓库工作副本目录，默认 <数据目录>/repos
#AIUDGE_WORK_DIR=./data/repos

# ---------- 密钥 ----------
# 加密主密钥：64 位 hex（openssl rand -hex 32）。
# 不设的话会自动生成到 <数据目录>/secret.key；生产环境请改用 Secret 注入。
# 【注意】写在文件里是明文。需求 §5 要求密钥走环境变量/k8s Secret，
# 这里放一份只是为了本地开发省事，读到会打警告。
#AIUDGE_SECRET_KEY=

# ---------- 模型网关 ----------
# 留空则用界面上「设置 → 模型配置」里填的
# AIUDGE_GATEWAY_URL=
# AIUDGE_GATEWAY_KEY=

# ---------- 飞书 ----------
# 配了 APP_ID + APP_SECRET 就会启用扫码登录，开发登录入口自动关闭
# AIUDGE_FEISHU_APP_ID=
# AIUDGE_FEISHU_APP_SECRET=
# 群机器人 webhook。全局兜底，每个仓库可以在界面上单独配自己的群
# AIUDGE_FEISHU_WEBHOOK=
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
# 每日汇总推送时间（cron），留空则不推。界面上配的优先
#AIUDGE_SUMMARY_CRON=30 18 * * *

# ---------- 规范文档 ----------
# 允许挂载为审核规范文档的绝对路径白名单，冒号分隔。
# 推荐用仓库内相对路径挂载（如 CLAUDE.md），零配置且天然受限，不用配这个
#AIUDGE_SPEC_DIRS=
