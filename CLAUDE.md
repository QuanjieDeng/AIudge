# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 这是什么

AIudge 是对每日 git 提交做自动化复审的后台：git 改动 → golangci-lint + 大模型按上下文审核 → 问题入库 → 人工标注/指派/评论 → 统计报表。需求文档在 `doc/需求.md`，**代码注释里的 `§4.3`、`§4.7`、`§5` 等标记全部指向该文档的章节**，改功能前先读对应章节。

单二进制交付：内嵌 SQLite（modernc.org/sqlite，纯 Go 无 cgo）+ 内嵌前端（`go:embed`），运行时零外部依赖、前端零构建（没有 npm/Vite/打包步骤）。Go 1.26。

全仓库代码注释和文档都是中文，新代码保持同样风格。

## 常用命令

```bash
go build -o aiudge ./cmd/aiudge          # 构建
go test ./internal/...                   # 全部测试（README 里也是这条）
go test ./internal/review/ -run TestFingerprint   # 单个测试
go vet ./...

# 本地跑起来（飞书未配置时会自动开放「开发登录」入口）
go run ./cmd/aiudge -config ./aiudge.conf
```

前端冒烟检查单独可跑，`go test ./internal/web` 也会调它（node 不在则跳过）：

```bash
node webui/smoke.mjs
```

改完前端**必须重新 `go build`**——`webui/*` 是 `go:embed` 进二进制的，不重编就还是旧文件。

## 配置

**配置只从文件读，不读环境变量。** 文件路径由 `-config` 指定（默认 `./aiudge.conf`）：
显式指定的文件不存在直接报错退出；默认路径不存在则生成全注释模板并按默认值启动。

优先级：`内置默认值 < aiudge.conf < 界面上的设置`。「界面可改项」（模型网关、全局通知 webhook、汇总时间）存库，一旦在界面上动过就以库里的为准；`cmd/aiudge/main.go` 的 `reportOverridden()` 会在启动日志里点出被覆盖的项。

> 仓库根目录那份 `aiudge.conf` 的文件头注释（"环境变量永远优先"、"由 `AIUDGE_CONFIG` 指定"）是**过期的**，与实现不符。以 `internal/config/` 和 README 为准。

## 架构

```
cmd/aiudge       单二进制入口：装配依赖、启 HTTP、等信号优雅退出
internal/config  只读文件的 KEY=value 配置（file.go 手写解析 + 模板生成）
internal/store   SQLite 持久层 + 报表聚合（全部裸 SQL，无 ORM）
internal/gitx    git 浅克隆、commit 区间、unified diff → 改动行号
internal/gocx    改动行 → 所在函数 + 直接调用链（go/ast）；给 AI 的检索工具箱
internal/lint    golangci-lint 子进程 + JSON 解析
internal/llm     LiteLLM 网关瘦客户端（OpenAI 兼容 + tool calling）
internal/review  审核管线：指纹、prompt、上下文组装、问题入库
internal/queue   并发受限的任务队列、指数退避重试、cron 调度
internal/notify  飞书 webhook
internal/auth    飞书 OAuth
internal/web     REST API + 静态资源 + 鉴权中间件
webui            内嵌前端（原生 JS + 手写 SVG 图表）
```

依赖装配是单向的：`main` 造 `config/store/lint → review.Pipeline → queue.Queue → web.Server`，各层之间靠 `store.DB` 和 `config.Config` 通信，没有接口抽象层。

### 审核管线的执行顺序（`internal/review/review.go`）

`(*Pipeline).Run(ctx, taskID, logf)` 是唯一入口，顺序 **不能随意调换**：

1. 读 task/repo/模型配置（仓库级模型覆盖全局默认），解出网关 URL/Key
2. 克隆或 fetch 工作副本 → `data/repos/repo-<id>`
3. **克隆之后**才解析规范文档——相对路径是相对工作副本解析的
4. 选 commit（`pickCommits`）→ `capCommits` → `Checkout`
5. 逐文件：`CommitDiff` → `gocx.MatchPath` 路径过滤 → `buildFileContext` 组装上下文
6. lint（失败只告警，不中断）
7. 调模型（`callAI`，带工具轮次的循环）
8. 入库：`CreateReport` → 逐条 `buildIssue` + `UpsertIssue` → `MarkReviewed`

### 数据流与去重的核心：问题指纹

`Fingerprint(repoID, filePath, category, Normalize(snippet), linter)`（`internal/review/fingerprint.go`）。

- **行号刻意不参与计算**：插入删除代码会让行号漂移，拿行号当身份会让趋势统计退化成「文件被改频率图」。
- AI 问题的 snippet 会优先回填文件里的**真实行**（`snippetOf`），避免模型改述导致指纹漂移。
- lint 问题额外把 linter 名计入，避免同一行的多个 linter 被折叠成一条。
- 指纹命中 → 更新展示字段、`seen_count+1`；若原状态是 `fixed`/`false_pos`，翻回 `pending` 并打 `regression=1`。这是「回归检测」的全部实现，改 `UpsertIssue` 时不要破坏这条路径。

### 版本标签

`PromptVersion` / `SpecVersion` 都是**内容 sha256 的前 12 位十六进制**，不是手填版本号（手填必然漂移）。规范文档每个截断到 12000 rune。报告记录 `模型名/模型版本/prompt版本/规范文档版本`，任何一个变了趋势图上就留一个断点。Prompt 占位符是字面量 `{{language}}`/`{{repo}}`，**故意不走 `fmt`**（否则内容里的 `%` 会被当成格式动词吃掉）。

### 几个必须知道的硬约束

**SQLite 是单连接。** `store.Open` 里 `SetMaxOpenConns(1)`，因此——
**绝不能在 `rows` 游标还开着的时候发第二条查询**，它会等那条唯一的连接一直等下去，把整个进程挂死（不是报错，是静默 hang）。
`store/deadlock_test.go` 用 5 秒超时的 watchdog 遍历所有读方法就是为了拦这个。需要「查规则再逐行匹配」时，走 `LoadMatcher`/`LoadMatchers` 把规则读进内存，再用纯函数 `SubsystemMatcher.Match`。

**开发阶段不做数据迁移。** 项目还在开发期，没有线上老库要兼容，所以**不要写任何迁移代码**（曾经有一套 `addColumnIfMissing()` + `upgrade()`，已删）。加列就改 `schema` 常量（`internal/store/db.go`），索引直接跟在它的表后面；要让改动生效就删掉 `data/aiudge.db` 重建。`migrate()` 只做两件事：跑一遍幂等 DDL、首次启动种下内置管理员。**项目不自动创建**——它是权限的锚点，建哪个、谁管由部署的人决定，空库就是空的项目列表。

**时间统一是 `store.TimeFmt`（`2006-01-02 15:04:05`，UTC）的字符串**，`store.Now()` 产出。存成这个格式是为了让字典序比较等价于时间序、且 SQL 里能直接 `strftime` 分桶。用户输入的时间在 `web/stats.go` 的 `timeRange()` 里从本地时区转成该格式。

**注册路由必须加进 `Server.routes()` 那张表**（`internal/web/server.go`），表里的 `access` 枚举决定挂哪层中间件（`accessPublic`/`accessUser`/`accessAdmin`）。`routes_test.go` 会遍历整张表断言「未登录一律 401、非管理员一律 403」——所以「忘了加鉴权」会在测试里立刻暴露，不要绕开这张表手写 `mux.Handle`。

**不设「必须改密」这类进站关卡。** 曾经有过一道 428（没改默认密码前业务接口一律不可用），结果是人能登录、能看见界面，点哪儿都弹「请先修改默认密码」。改密码就只是一个普通功能（右上角用户菜单里），该不该换由人自己判断。

**项目级权限是「角色 + 一张权限清单」**：`project_members.perms` 存 JSON 数组，键写在 `store.PermissionList`（Go 切片，不是表——每加一种权限都得在代码里实现校验，往表里插一行不会生效）。判定入口是 `web/auth.go` 的 `canTriggerRepo` 这类 helper，与 `canAdminRepo`/`canViewRepo` 并排。**给接口加「看得见就行」之外的权限时，要同时收紧后端与前端按钮**（按钮由 `/api/repos` 的 `can_trigger` 驱动）。

**手动触发的时间范围必须在管线里解析。** `review_tasks` 的 `time_from`/`time_to` 存 UTC 字符串，由 `pickCommits` 在 **fetch 之后**调 `gitx.LogSince` 换算——在 handler 里换算成 SHA 用的是上次审核留下的工作副本，落后远端时会静默漏提交。commit 范围与时间范围互斥（同时给报 400）。

**提交下拉的同步只 fetch、不 checkout**（`gitx.SyncBranch`，对比 `Clone`/`Fetch` 末尾那次 `checkout -B`）。审核任务可能正在同一个工作副本上跑，任何 checkout 都会和它抢工作区（读到半新半旧的源码，或 `index.lock` 冲突把任务打成失败）。列表从 `FETCH_HEAD` 取；首次（未克隆）才走整份 `Clone`，那时 FETCH_HEAD 还不存在，要看 `HEAD`。

**新接口的项目隔离口径**：列表接口开头 `pids, all := s.visibleProjects(u)`（失败时 fail-closed），把 `pids/all` 透传进 store 层，由 `projectPredicate`/`repoVisibleCond`（`internal/store/visibility.go`）拼谓词；按 id 直取的详情接口要**单独**再校验归属（`ReportVisible`/`IssueVisible`/`loadRepoForRead`）。注意 `VisibleProjects` 对全局管理员返回 `(nil, true)`——**`nil` 列表 + `all=true` 与空列表语义完全不同**，空列表必须拼成 `1=0` 而不是"不加条件"。

前端选中某个项目时，`project_id` 会作为查询参数带上来，由 `scopeToProject(projectID, pids, all)`（`internal/web/auth.go`）把范围收窄成一个元素。**它必须与可见集求交集，不能直接拿 `project_id` 当谓词**——那是用户可控的参数，直接当谓词就等于「改个 URL 看别人的项目」。新增任何按项目筛选的接口都要走这一步。

**凭据处理**：AES-256-GCM 加密落库（`internal/crypto`），界面只回显 `Mask()` 的末四位，日志用 `gitx.Redact` 抹掉 URL 里的 token。新增任何打印仓库 URL / webhook 的地方都要过一遍这两者。

### 队列与调度（`internal/queue`）

`Queue` 是固定 `cfg.Concurrency` 个 worker 从 channel 取任务，队列满时任务留在库里 `queued` 状态，靠重启或下轮定时捡回。失败重试只对 **5xx/429 和网络类错误**生效（`retryable()`），`review.ErrConfig` 标记的配置类错误立即失败并告警——重试只是把同一句话重复三遍还白等退避。退避是 `1<<retryCount` 分钟。

软超时只告警不终止（任务可能正卡在一次大模型请求上，砍掉浪费已花的成本），硬超时才终止。

`Scheduler.Reload()` 整体重建全部 cron 条目，仓库配置改动后调用，避免为了改 cron 重启进程。

**清档（`store.Reset`）前必须先 `q.Drain()`**，否则跑了一半的任务会在数据清空之后把报告写回来。`Drain` 用 200ms 轮询等 `idle` 归零——这是低频人工操作，轮询比维护计数简单。

### 前端（`webui/`）

三个文件：`index.html` / `app.js` / `style.css`，`app.js` 是全部逻辑（含手写 SVG 图表，无图表库）。

- 路由是 hash 路由，分两层：`#/home`、`#/workbench`（首页 / 工作台），`#/p/<id>/<page>`（项目页，`<page>` 取自 `app.js` 的 `PAGES`），`#/admin`（平台设置，仅全局管理员）。**项目 id 必须落在 URL 上**——只放内存里的话刷新和收藏链接就指向别处了。旧的单层 hash 由 `LEGACY` 表兜底重定向。
- **两套「设置」别混**：项目页的「设置」是项目自己的（仓库/成员/信息，`viewProjects`，项目管理员可操作、成员只读）；`#/admin` 是平台级的（模型/通知/Prompt/用户/审计，`viewAdminSettings`，仅全局管理员）。平台设置**不能挂在项目页下面**——全局管理员可能一个项目都没加入，挂进去他就永远够不着。`viewProjects` 这个函数名被 `smoke.mjs` 的用例直接调用着，改名等于删掉那几条覆盖。
- 所有请求走 `api()` 包装，它处理 401 拦截和错误提示。
- 静态资源不能设长缓存（URL 不变、内容随二进制变），改用**内容哈希做 ETag** 实现 `no-cache` + 304，`webui.Version()` 就是 app.js 的 ETag，健康检查 `/api/health` 会回报它，用来确认「现在跑的是哪个构建」。
- `webui/smoke.mjs` 用最小 DOM 桩加载真实 `app.js`，覆盖那些在浏览器里只表现为「点了没反应」、从后端日志什么都看不出的前端 bug（`api()` 401 拦截重绘丢弃错误节点的那个经典坑就是这么发现的）。

## 能力边界（改代码时会撞到的）

- **只有 Go 有完整 AST 能力**：`gocx` 用 `go/parser` 定位「改动行所在函数」，`find_symbol` 按语言正则声明模式检索（go/cpp/python/ts/java/rust/lua），lint 只接 golangci-lint。非 Go 仓库会退化成「改动块 ±3 行」上下文、`find_symbol` 返回空、lint 直接跳过并记一条日志。未知语言的符号索引**故意留空**（返回「没找到」），避免给出「这个轮子不存在」的假信号。
- 上下文预算写死在 `review.go` 顶部：`maxCommits=30`、`maxFilesPerCommit=15`、`maxCalleesPerFile=5`、`maxContextBytes=160KB`（硬截断，超出的 diff 静默丢弃并告警）、`maxToolRounds=8`（用完补一轮强制 JSON 输出）。被调函数只在**同目录（同包）且 ≤60 行**时才内联展开。
- `resolveCallee` 按名字匹配顶层声明，不同 receiver 上同名的方法会撞车——已知精度损失，不是 bug。
- 报表的处理率/误报率按问题**当前状态**统计，不是历史快照。
