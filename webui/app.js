/* AIudge 前端。零依赖、零构建：原生 DOM + 手写 SVG 图表。
   最高频场景是「每天扫一遍待处理列表」，所以默认落点是 pending 问题表格，
   视觉表现一律让位于信息密度与扫读速度（需求 §4.9）。 */
'use strict';

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const SEV_ORDER = ['高危', '建议', '风格'];
const CATS = ['安全', '性能', '逻辑', '重复代码', '规范'];
const STATUS = { pending: '待处理', confirmed: '已确认', fixed: '已修复', false_positive: '误报' };
// 语言决定上下文提取方式与 lint 接入。目前只有 Go 有完整的 AST 级能力
const LANGS = [['go', 'Go'], ['cpp', 'C++'], ['python', 'Python'], ['typescript', 'TypeScript'],
  ['java', 'Java'], ['rust', 'Rust'], ['lua', 'Lua'], ['csharp', 'C#']];

const state = {
  user: null, repos: [], users: [],
  // projects 是「我能看到的项目」；null 表示还没拉到（或拉取失败），
  // 与空数组语义不同——空数组要显示「你还没有加入任何项目」的引导。
  projects: null, projectId: 0,
  repoId: 0, from: '', to: '',
  // 这五个筛选都是多选，值是数组，**空数组 = 全选 = 不筛这一项**（所以默认全空，
  // 也就是默认各维度都是「全部」）。负责人里的 '0' 表示「未指派」，
  // 和后端 assignee_id IN (...) 的口径一致。
  issues: { status: [], severity: [], source: [], category: [], assignee_id: [], regression: false, keyword: '', page: 1 },
  selected: new Set(),
};

/* ---------- 网络 ---------- */

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  // 401 只在「会话过期」时才该重绘登录页。登录接口自己的 401 是「凭据不对」，
  // 走这条分支会把整个页面重绘一遍，错误信息还写在已经被丢弃的节点上——
  // 表现出来就是密码输错了「点了没反应」
  const isAuthAttempt = path.startsWith('/api/auth/password') || path.startsWith('/api/auth/dev-login');
  if (res.status === 401 && !isAuthAttempt) {
    state.user = null;
    renderLogin();
    throw new Error('未登录');
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
  if (!res.ok) throw new Error((data && data.error) || `HTTP ${res.status}`);
  return data;
}

const qs = (o) => Object.entries(o).filter(([, v]) => v !== '' && v != null && v !== false)
  .map(([k, v]) => `${k}=${encodeURIComponent(v)}`).join('&');

/* range 是项目页所有列表/统计接口的公共筛选范围，统一从这里取。
   project_id 是数据隔离的口径——漏掉它的页面看着是「当前项目」，数字却是全平台的。
   后端把 project_id 与「我能看到的项目」求交集，所以带一个不属于自己的 id 只会查出空。 */
const range = () => ({
  project_id: state.projectId, repo_id: state.repoId, from: state.from, to: state.to,
});

function toast(msg, isErr) {
  const el = $('#toast');
  el.textContent = msg;
  el.className = 'toast' + (isErr ? ' err' : '');
  el.hidden = false;
  clearTimeout(el._t);
  el._t = setTimeout(() => { el.hidden = true; }, 2800);
}

/* ---------- 小工具 ---------- */

const fmtTime = (s) => (s ? s.slice(5, 16) : '—');

function sevBar(sev) { return `<span class="bar-sev ${esc(sev)}"></span>`; }

const langLabel = (k) => (LANGS.find(([v]) => v === k) || [k, k || 'Go'])[1];

function statusTag(iss) {
  const reg = iss.regression ? '<span class="tag reg">回归</span> ' : '';
  return `${reg}<span class="tag ${esc(iss.status)}">${esc(iss.status_name || STATUS[iss.status])}</span>`;
}

function kv(pairs) {
  return `<dl class="kv">${pairs.filter(([, v]) => v !== '' && v != null)
    .map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join('')}</dl>`;
}

function codeBlock(text) {
  if (!text) return '';
  const lines = String(text).split('\n');
  return `<pre class="code">${lines.map((l, i) =>
    `<span class="ln">${String(i + 1).padStart(4, ' ')}</span>  ${esc(l)}`).join('\n')}</pre>`;
}

/* html() 把 HTML 字符串转成节点。用 append 直接塞字符串会变成转义后的文本节点，
   这里统一收口，避免两套写法混用。 */
function html(s) {
  const d = document.createElement('div');
  d.innerHTML = String(s);
  return d.firstElementChild || d;
}

// cols 里的每一项要么是字符串（纯标题，转义后显示），要么是 {html}——表头里要塞
// 下拉筛选器这类控件时用后者（问题库那几个筛选就挂在表头上）。
function table(cols, rows, rowAttr, emptyHint) {
  if (!rows.length) return emptyBlock('暂无数据', emptyHint || '');
  const head = cols.map((c) => `<th>${typeof c === 'string' ? esc(c) : c.html}</th>`).join('');
  const el = html(`<div class="tbl-wrap"><table><thead><tr>${head}</tr></thead>
    <tbody>${rows.map(rowAttr).join('')}</tbody></table></div>`);
  // 表头默认左对齐，而数字格 td.num 是右对齐的，两者会错开小半个格子——「提交数 12」
  // 看着像没对齐。这里照第一行的实际对齐方式把 num 补回表头，省得每张表都手写一遍，
  // 以后新加数字列也自动是对的。
  const first = el.querySelector('tbody tr');
  if (first) [...first.children].forEach((td, i) => {
    if (td.classList.contains('num')) el.querySelector(`thead th:nth-child(${i + 1})`)?.classList.add('num');
  });
  return el;
}

function emptyBlock(title, sub = '') {
  return html(`<div class="empty"><b>${esc(title)}</b>${sub ? esc(sub) : ''}</div>`);
}

function pager(total, page, limit, onGo) {
  const pages = Math.max(1, Math.ceil(total / limit));
  const el = document.createElement('div');
  el.className = 'pager';
  el.innerHTML = `<span>共 ${total} 条 · 第 ${page}/${pages} 页</span>`;
  const prev = document.createElement('button');
  prev.className = 'btn sm'; prev.textContent = '上一页'; prev.disabled = page <= 1;
  prev.onclick = () => onGo(page - 1);
  const next = document.createElement('button');
  next.className = 'btn sm'; next.textContent = '下一页'; next.disabled = page >= pages;
  next.onclick = () => onGo(page + 1);
  el.append(prev, next);
  return el;
}

/* ---------- 抽屉 ---------- */

function openDrawer(title, node) {
  $('#drawer-title').innerHTML = title;
  const body = $('#drawer-body');
  body.innerHTML = '';
  body.append(node);
  $('#drawer').hidden = false;
}
function closeDrawer() { $('#drawer').hidden = true; }
document.addEventListener('click', (e) => { if (e.target.closest('[data-close]')) closeDrawer(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeDrawer(); });

/* ---------- 图表（手写 SVG，无依赖） ---------- */

const SEV_COLOR = { 高危: '#ff4d6d', 建议: '#ffb020', 风格: '#7c8aa8' };

function stackedTrend(points, breaks, opts = {}) {
  const h = opts.height || 240, pad = { t: 12, r: 12, b: 26, l: 36 };
  const buckets = [...new Set(points.map((p) => p.bucket))].sort();
  if (!buckets.length) return emptyBlock('区间内没有数据');
  const byBucket = {};
  for (const p of points) (byBucket[p.bucket] ||= {})[p.severity] = p.count;
  const totals = buckets.map((b) => SEV_ORDER.reduce((s, k) => s + (byBucket[b][k] || 0), 0));
  const max = Math.max(1, ...totals);
  const W = Math.max(560, buckets.length * 74);
  const iw = W - pad.l - pad.r, ih = h - pad.t - pad.b;
  const bw = Math.min(38, (iw / buckets.length) * 0.62);

  let g = '';
  for (let i = 0; i <= 4; i++) {
    const y = pad.t + (ih * i) / 4;
    g += `<line class="grid-line" x1="${pad.l}" x2="${W - pad.r}" y1="${y}" y2="${y}"/>
      <text x="${pad.l - 6}" y="${y + 3.5}" text-anchor="end">${Math.round((max * (4 - i)) / 4)}</text>`;
  }
  buckets.forEach((b, i) => {
    const x = pad.l + (iw / buckets.length) * (i + 0.5) - bw / 2;
    let acc = 0;
    for (const sev of SEV_ORDER) {
      const v = byBucket[b][sev] || 0;
      if (!v) continue;
      const bh = (v / max) * ih;
      const y = pad.t + ih - acc - bh;
      acc += bh;
      g += `<rect x="${x}" y="${y}" width="${bw}" height="${Math.max(1, bh)}" fill="${SEV_COLOR[sev]}" rx="2" opacity=".92">
        <title>${esc(b)} ${esc(sev)}: ${v}</title></rect>`;
    }
    g += `<text x="${x + bw / 2}" y="${h - 8}" text-anchor="middle">${esc(b.replace(/^\d{4}-/, ''))}</text>`;
  });

  // 版本断点：模型/prompt/规范任一变化，前后曲线不可比（§附录决策 6）
  (breaks || []).forEach((br) => {
    const idx = buckets.findIndex((b) => b >= br.bucket);
    if (idx < 0) return;
    const x = pad.l + (iw / buckets.length) * idx;
    g += `<line class="brk" x1="${x}" x2="${x}" y1="${pad.t}" y2="${pad.t + ih}"/>
      <text class="brk-t" x="${x + 3}" y="${pad.t + 10}">${esc(br.label)}</text>`;
  });

  const wrap = document.createElement('div');
  wrap.innerHTML = `<div class="legend">${SEV_ORDER.map((s) =>
    `<span><i style="background:${SEV_COLOR[s]}"></i>${s}</span>`).join('')}</div>
    <svg class="chart" viewBox="0 0 ${W} ${h}" width="100%" height="${h}" preserveAspectRatio="xMidYMid meet">${g}</svg>`;
  return wrap;
}

function distBars(items, opts = {}) {
  if (!items.length) return emptyBlock('暂无数据');
  const max = Math.max(...items.map((i) => i.count));
  const wrap = document.createElement('div');
  wrap.innerHTML = items.slice(0, opts.limit || 12).map((i) =>
    `<div class="dist-row"><span class="k" title="${esc(i.key)}">${esc(i.key || '未分类')}</span>
      <span class="v" style="width:${Math.max(2, (i.count / max) * 100)}%"></span>
      <span class="n">${i.count}</span></div>`).join('');
  return wrap;
}

/* ---------- 路由 ----------
   两层结构：
     #/home、#/workbench            首页（介绍 + 工作台）
     #/p/<id>/<page>                项目页，项目 id 进 URL
   项目 id 落在 URL 上，刷新、收藏、前进后退才都对得上；只用内存里的「当前项目」
   一刷新就回退到别的项目，而且收藏链接分享出去是别人的上下文。 */

const PAGES = {
  dashboard: { title: '概览', render: viewDashboard },
  issues: { title: '问题库', render: viewIssues },
  reports: { title: '审核报告', render: viewReports },
  tasks: { title: '任务队列', render: viewTasks },
  stats: { title: '统计报表', render: viewStats },
  // 项目页的「设置」就是项目设置（仓库/成员/信息），不再混平台配置——
  // 那是独立一页，见 #/admin。两套设置各自单一用途，谁也不藏在谁里面。
  settings: { title: '设置', render: viewProjects },
};

const HOME_TABS = ['home', 'workbench'];

// 旧的单层 hash（#/dashboard 等）映射到项目页，别让存下来的旧链接直接白屏。
const LEGACY = { dashboard: 'dashboard', issues: 'issues', reports: 'reports',
  tasks: 'tasks', stats: 'stats', projects: 'settings', settings: 'settings' };

const isAdmin = () => (state.user || {}).role === 'admin';

function parseRoute() {
  const parts = location.hash.replace(/^#\/?/, '').split('/');
  // 平台设置不属于任何项目：全局管理员可能一个项目都没加入，
  // 把它挂在项目页下面的话，这种人永远够不着。
  if (parts[0] === 'admin') return { home: false, admin: true };
  if (parts[0] === 'p') {
    const pid = +parts[1];
    // 没带 id 的项目页地址（#/p、#/p/0）没有意义，回工作台挑一个
    if (!pid) return { home: true, tab: 'workbench' };
    return { home: false, pid, page: PAGES[parts[2]] ? parts[2] : 'dashboard' };
  }
  if (HOME_TABS.includes(parts[0])) return { home: true, tab: parts[0] };
  if (LEGACY[parts[0]]) return { home: false, pid: state.projectId, page: LEGACY[parts[0]] };
  return { home: true, tab: 'home' };
}

const go = (hash) => { if (location.hash === hash) route(); else location.hash = hash; };

async function loadProjects() {
  try { state.projects = await api('/api/projects'); }
  catch { state.projects = null; } // null = 没拿到，别据此说「你没有项目」
  return state.projects;
}

/* syncChrome 把侧栏和顶栏调成当前层级该有的样子：
     首页      只有「首页」按钮和用户浮层
     项目页    项目切换 + 六个页签
     平台设置  没有项目上下文，只有「系统设置」这一项
   「系统设置」对所有全局管理员常驻，跟有没有项目无关。 */
function syncChrome(r) {
  $('#nav').hidden = r.home || !!r.admin;
  $('#home-tabs').hidden = !r.home;
  $('#page-title').hidden = r.home;
  $('#topbar-tools').hidden = r.home || !!r.admin;
  $('#home-btn').classList.toggle('on', r.home);
  const sw = $('#proj-switch');
  sw.hidden = r.home || !!r.admin;
  $('#admin-nav').hidden = !isAdmin();
  $('#admin-nav').classList.toggle('on', !!r.admin);

  if (r.home) {
    $$('#home-tabs a').forEach((a) => a.classList.toggle('on', a.dataset.tab === r.tab));
    return;
  }
  if (r.admin) {
    $('#page-title').textContent = '系统设置';
    return;
  }
  $('#page-title').textContent = PAGES[r.page].title;
  $$('#nav a').forEach((a) => {
    a.href = `#/p/${r.pid}/${a.dataset.page}`;
    a.classList.toggle('on', a.dataset.page === r.page);
  });

  const list = state.projects || [];
  const cur = list.find((p) => p.id === r.pid);
  sw.innerHTML = `<span>${esc(cur ? cur.name : '项目 #' + r.pid)}</span><em>切换</em>`;
  $('#projpop-body').innerHTML = `<div class="pop-list">
    ${list.map((p) => `<button class="pop-item ${p.id === r.pid ? 'on' : ''}" data-pid="${p.id}">
      ${esc(p.name)}<span>${p.repo_count}仓 · ${p.member_count}人</span></button>`).join('')}
    <button class="pop-item" data-go="#/workbench">← 工作台</button></div>`;
  $('#projpop-body').onclick = (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    // 开关状态不对时 hidePopover 会抛，抛了就轮不到下面的跳转——
    // 表现成「点了没反应」，正是最难查的那类前端 bug
    const pop = $('#projpop');
    if (pop.matches(':popover-open')) pop.hidePopover();
    go(b.dataset.pid ? `#/p/${b.dataset.pid}/dashboard` : b.dataset.go);
  };
}

// enterProject 只在真的换了项目时才重置——每次导航都重置会把仓库筛选和勾选清掉。
async function enterProject(pid) {
  if (state._pid === pid) return;
  state._pid = pid;
  state.projectId = pid;
  state.repoId = 0; // 上一个项目的仓库 id 拿到新项目里不成立
  state.selected.clear();
  $('#view')._tab = null; // 设置页签回到「项目」，别停在上个角色才有的页签上
  // 项目列表在 boot 时已经拉过，切项目不需要重拉（新建项目那条路径会自己刷新）
  if (!state.projects) await loadProjects();
  await fillRepos();
}

// noProjectHint 用户一个项目都没参与时，各页面数据显示为空是正常的，
// 但光看空表格会以为系统坏了——给一句明确的指引
async function noProjectHint() {
  if (!state.projects || state.projects.length) return null;
  const me = state.user || {};
  const el = document.createElement('div');
  el.className = 'card glow';
  el.style.marginBottom = '14px';
  el.innerHTML = `<b>你还没有加入任何项目</b><br>
    <span class="muted">这个系统按项目隔离数据——没有归属项目就什么都看不到，这是正常的。</span><br>
    <span class="muted">去<a href="#/workbench" style="color:var(--blue)">工作台</a>新建一个项目，
      你会自动成为它的项目管理员${me.role === 'admin' ? '' : '；也可以让管理员把你加进现有的项目'}。</span>`;
  return el;
}

async function route() {
  const r = parseRoute();
  closeDrawer();
  // 旧链接或手改的地址指向项目页，但当前没有项目上下文 → 去工作台挑一个。
  // 平台设置不属于项目，不能跟着一起被送回工作台。
  if (!r.home && !r.pid && !r.admin) return go('#/workbench');
  syncChrome(r);

  const view = $('#view');
  view.innerHTML = '<div class="card"><span class="spin"></span> 加载中…</div>';
  try {
    if (r.home) {
      if (r.tab === 'workbench') await viewWorkbench(view);
      else viewHomeIntro(view);
      return;
    }
    // 平台设置不依赖项目。手敲地址进来的非管理员直接送回首页——
    // 让他进去吃一屏 403 不如不给进
    if (r.admin) {
      if (!isAdmin()) return go('#/home');
      await viewAdminSettings(view);
      return;
    }
    await enterProject(r.pid);
    syncChrome(r); // 项目名要等 list 拉回来才填得上
    // 项目 id 不在可见列表里（被删了、或是条过期的链接）就明说。
    // 否则每个页面都拿一个不存在的 id 去查，安静地全返回空——看着像「这个项目没数据」
    if (state.projects && !state.projects.some((p) => p.id === r.pid)) {
      view.innerHTML = '';
      view.append(emptyBlock('项目不存在或你无权访问', '它可能已经被删除，去「工作台」重新选一个'));
      return;
    }
    const hint = await noProjectHint().catch(() => null);
    if (hint) { view.innerHTML = ''; view.append(hint); return; }
    await PAGES[r.page].render(view);
  } catch (e) {
    view.innerHTML = `<div class="card">加载失败：${esc(e.message)}</div>`;
  }
}

/* ---------- 首页 / 工作台 ---------- */

function viewHomeIntro(el) {
  el.innerHTML = '';
  const card = document.createElement('div');
  card.className = 'card glow intro';
  const steps = [
    ['1', '采集改动', '每天定时（或 GitLab webhook 触发）把仓库拉成工作副本，取出这批 commit 的 diff 和改动行号。'],
    ['2', '两路检查', 'golangci-lint 抓确定的问题；大模型读改动行所在的函数、同包被调函数，以及这个仓库自己挂载的规范文档，找「重复造轮子」「N+1 查询」这类静态检查看不见的问题。'],
    ['3', '去重入库', '每条问题按「仓库+文件+分类+代码内容」算指纹。行号刻意不参与，改一行代码不会让趋势统计退化成「文件被改频率图」；指纹命中就累加次数，已修复的问题再次出现会标记为回归。'],
    ['4', '人工闭环', '在问题库里标注、指派、评论，状态流转全程留痕。统计报表按当前状态算处理率与误报率。'],
  ];
  const points = [
    ['按项目隔离', '仓库归属项目，你在项目内有角色。看不到的项目，数据一条都不会出现。'],
    ['趋势可解释', '模型、Prompt、规范文档任一变更都会在趋势图上留一条断点——前后曲线本就不可比。'],
    ['成本可控', '上下文有硬预算，超出的 diff 会被丢弃并告警，不会为一个巨大提交烧掉整晚的额度。'],
    ['语言能力', '目前只有 Go 有完整的函数级上下文与 lint 接入；其他语言能跑，但跨文件问题的检出率会明显下降。'],
  ];
  card.innerHTML = `
    <h2>AIudge · 代码审核台</h2>
    <div class="intro-cols">
      <div>
        <p class="lead">把每天的 git 提交自动过一遍：确定性的问题交给 golangci-lint，
          需要读懂上下文的问题交给大模型——按每个仓库自己挂载的规范文档来判，
          而不是套一份通用规则。找出来的问题入库、指派、跟踪到关闭，最后汇成报表。</p>
        <div class="steps">${steps.map(([n, t, d]) =>
          `<div class="step"><i>${n}</i><div><b>${esc(t)}</b><p>${esc(d)}</p></div></div>`).join('')}</div>
      </div>
      <div class="points">${points.map(([t, d]) =>
        `<div class="point"><b>${esc(t)}</b><span>${esc(d)}</span></div>`).join('')}</div>
    </div>
    <p class="hint muted" style="margin:22px 0 0">
      从<a href="#/workbench" style="color:var(--blue)">工作台</a>选一个项目开始；
      还没有项目就新建一个，你会自动成为它的项目管理员。</p>`;
  el.append(card);
}

// viewWorkbench 是「我所在的项目」的总入口。建项目对任何登录用户开放，
// 建完自动成为该项目的项目管理员（后端 handleProjectCreate 里写好了）。
async function viewWorkbench(el) {
  const projects = await loadProjects();
  el.innerHTML = '';
  const bar = document.createElement('div');
  bar.className = 'toolbar';
  bar.innerHTML = `<span class="muted">${projects ? '你所在的项目 · ' + projects.length + ' 个' : ''}</span>
    <span class="spacer"></span>
    <button class="btn primary" id="np">+ 新建项目</button>`;
  el.append(bar);
  bar.onclick = (e) => { if (e.target.id === 'np') projectForm(null); };

  if (!projects || !projects.length) {
    el.append(emptyBlock('你还没有加入任何项目',
      '点右上角「新建项目」建一个，你会自动成为它的项目管理员'));
    return;
  }
  const grid = document.createElement('div');
  grid.className = 'proj-grid';
  grid.innerHTML = projects.map((p) => `
    <a class="proj-card" href="#/p/${p.id}/dashboard">
      <div class="row"><b>${esc(p.name)}</b>
        <span class="tag ${p.can_admin ? 'confirmed' : '风格'}">${esc(p.role_name || '成员')}</span></div>
      <p class="muted">${esc(p.description || '暂无说明')}</p>
      <div class="meta">${p.repo_count} 个仓库 · ${p.member_count} 位成员</div>
    </a>`).join('');
  el.append(grid);
}

/* ---------- 概览 ---------- */

async function viewDashboard(el) {
  const [ov, trend, byCat, bySrc, bySub, versions] = await Promise.all([
    api('/api/stats/overview?' + qs(range())),
    api('/api/stats/trend?' + qs({ ...range(), bucket: 'week' })),
    api('/api/stats/distribution?' + qs({ ...range(), by: 'category' })),
    api('/api/stats/distribution?' + qs({ ...range(), by: 'source' })),
    api('/api/stats/distribution?' + qs({ ...range(), by: 'subsystem' })),
    api('/api/stats/versions?' + qs({ ...range(), bucket: 'week' })),
  ]);

  const cards = document.createElement('div');
  cards.className = 'cards';
  const kpi = (n, l, cls = '') => `<div class="card kpi ${cls}"><span class="n">${n}</span><span class="l">${esc(l)}</span></div>`;
  cards.innerHTML =
    kpi(ov.pending, '待处理', ov.pending ? 'high' : 'ok') +
    kpi(ov.high_count, '高危问题', 'high') +
    kpi(ov.new_count, '本期新增', 'info') +
    kpi(ov.fixed + ov.false_positive, '已处理', 'ok') +
    kpi((ov.handle_rate * 100).toFixed(0) + '%', '处理率') +
    kpi((ov.false_rate * 100).toFixed(1) + '%', '误报率') +
    kpi(ov.regression, '回归问题', ov.regression ? 'high' : '') +
    kpi(ov.avg_handle_hours.toFixed(1) + 'h', '平均处理时长');

  const t = document.createElement('div');
  t.className = 'card glow';
  t.innerHTML = '<h3>问题趋势（按周 · 堆叠）</h3>';
  t.append(stackedTrend(trend, versionBreaks(trend, versions)));

  const cat = document.createElement('div');
  cat.className = 'card';
  cat.innerHTML = '<h3>按分类</h3>';
  cat.append(distBars(byCat));

  const src = document.createElement('div');
  src.className = 'card';
  src.innerHTML = '<h3>按来源</h3>';
  src.append(distBars(bySrc));

  const sub = document.createElement('div');
  sub.className = 'card';
  sub.innerHTML = '<h3>按子系统</h3>';
  sub.append(distBars(bySub));

  const grid = document.createElement('div');
  grid.className = 'grid3';
  grid.append(cat, src, sub);
  grid.style.marginTop = '14px';

  const stack = document.createElement('div');
  stack.className = 'stack';
  stack.append(cards, t, grid);
  el.innerHTML = '';
  el.append(stack);
}

/* versionBreaks 把版本标签转成趋势图上的断点：标签变更的那一周期打一条虚线。
   断点落在哪一柱由后端用与趋势图相同的 SQLite 表达式算出（v.bucket），
   前端不自己推算周数——两套口径差一点，断线就会落在错误的柱子上。 */
function versionBreaks(points, versions) {
  if (!versions || versions.length < 2) return [];
  const buckets = new Set(points.map((p) => p.bucket));
  const breaks = [];
  let prev = versions[0];
  for (const v of versions.slice(1)) {
    const changed = v.model_name !== prev.model_name || v.prompt_version !== prev.prompt_version ||
      v.spec_version !== prev.spec_version || v.model_version !== prev.model_version;
    if (changed && buckets.has(v.bucket)) {
      const delta = [];
      if (v.model_name !== prev.model_name) delta.push(`模型→${v.model_name}`);
      if (v.prompt_version !== prev.prompt_version) delta.push(`prompt→${v.prompt_version}`);
      if (v.spec_version !== prev.spec_version) delta.push('规范文档变更');
      breaks.push({ bucket: v.bucket, label: delta.join(' / ') || '版本变更' });
    }
    prev = v;
  }
  return breaks;
}

/* ---------- 问题库 ---------- */

async function viewIssues(el) {
  const f = state.issues;
  // 多选参数用逗号分隔（值都是枚举名或数字，本身不含逗号）；空数组拼出空串，
  // qs() 会把空串丢掉 = 不筛这一项
  const params = {
    ...f, ...range(),
    page: f.page, limit: 50, regression: f.regression ? 1 : '',
    status: f.status.join(','), severity: f.severity.join(','),
    category: f.category.join(','), source: f.source.join(','),
    assignee_id: f.assignee_id.join(','),
  };
  const [data, members] = await Promise.all([api('/api/issues?' + qs(params)), assignableMembers()]);

  const tools = document.createElement('div');
  tools.className = 'toolbar';
  tools.innerHTML = `<input class="inp" id="kw" placeholder="搜索标题 / 路径 / 代码" value="${esc(f.keyword)}" style="width:260px">
    <button class="btn" id="kw-go">搜索</button>
    <button class="chip ${f.regression ? 'on' : ''}" id="reg">仅回归</button>
    <span class="spacer"></span>
    <span class="muted" id="sel-info"></span>
    <button class="btn sm" id="batch-fp">批量标误报</button>
    <button class="btn sm" id="batch-assign">批量指派</button>
    <button class="btn sm" id="csv">导出 CSV</button>`;

  // 仓库不在这儿再放一份：顶栏那个下拉就是全站的仓库范围，两处一个状态容易对不上
  const cols = ['',
    multiSelect('severity', '等级', SEV_ORDER.map((s) => [s, s]), f.severity),
    multiSelect('category', '分类', CATS.map((c) => [c, c]), f.category),
    '位置', '问题',
    multiSelect('source', '来源', [['ai', 'AI'], ['lint', 'lint']], f.source), '提交人',
    // 「未指派」就是 assignee_id=0，和后端 IN 的口径一致，也能和具体的人一起勾
    multiSelect('assignee_id', '负责人',
      [[0, '未指派'], ...members.map((m) => [m.user_id, m.name || '#' + m.user_id])], f.assignee_id),
    multiSelect('status', '状态',
      [['pending', '待处理'], ['confirmed', '已确认'], ['fixed', '已修复'], ['false_positive', '误报']], f.status),
    '最近出现'];

  const rowAttr = (i) => `
    <tr data-id="${i.id}">
      <td><input type="checkbox" data-pick="${i.id}" ${state.selected.has(i.id) ? 'checked' : ''}></td>
      <td><span class="tag ${esc(i.severity)}">${esc(i.severity)}</span></td>
      <td class="muted">${esc(i.category)}</td>
      <td class="path">${esc(i.file_path)}:${i.line_start}${i.line_end > i.line_start ? '-' + i.line_end : ''}
        <br><small class="muted">${esc(i.subsystem || '')}</small></td>
      <td><div class="title-cell"><b>${sevBar(i.severity)}${esc(i.title)}</b>
        <small>${esc((i.description || '').slice(0, 90))}</small></div></td>
      <td class="muted">${i.source === 'lint' ? 'lint·' + esc(i.linter) : 'AI'}</td>
      <td class="muted">${esc(i.committer || '—')}</td>
      <td class="muted">${esc(i.assignee_name || '—')}</td>
      <td>${statusTag(i)}</td>
      <td class="mono muted">${fmtTime(i.last_seen_at)}</td>
    </tr>`;

  // 一条都没筛出来也要把表头留下：筛选器就长在表头里，整张表换成「暂无数据」
  // 等于把筛条件一起藏了，人会卡在空结果里出不去
  const rows = data.items.length
    ? table(cols, data.items, rowAttr)
    : table(cols, [0], () => `<tr><td colspan="${cols.length}" class="muted" style="padding:18px 12px;text-align:center">
        没有符合条件的问题，换个筛选条件试试</td></tr>`);

  el.innerHTML = '';
  el.append(tools, rows);
  el.append(pager(data.total, f.page, 50, (p) => { f.page = p; route(); }));

  const syncSel = () => {
    $('#sel-info') && ($('#sel-info').textContent = state.selected.size ? `已选 ${state.selected.size} 项` : '');
  };

  tools.onclick = (e) => {
    if (e.target.id === 'kw-go') { f.keyword = $('#kw').value.trim(); f.page = 1; route(); }
    if (e.target.id === 'reg') { f.regression = !f.regression; f.page = 1; state.selected.clear(); route(); }
    if (e.target.id === 'csv') {
      location.href = '/api/stats/export?' + qs({ type: 'issues', ...range() });
    }
    if (e.target.id === 'batch-fp') batch('false_positive');
    if (e.target.id === 'batch-assign') batchAssign();
  };
  tools.querySelector('#kw').onkeydown = (e) => { if (e.key === 'Enter') { f.keyword = e.target.value.trim(); f.page = 1; route(); } };

  const tbody = el.querySelector('tbody');
  if (tbody) tbody.onclick = (e) => {
    const pick = e.target.closest('[data-pick]');
    if (pick) {
      const id = +pick.dataset.pick;
      pick.checked ? state.selected.add(id) : state.selected.delete(id);
      pick.closest('tr').classList.toggle('sel', pick.checked);
      syncSel();
      return;
    }
    const tr = e.target.closest('tr[data-id]');
    if (tr) openIssue(+tr.dataset.id);
  };
  syncSel();
}

async function batch(op, value) {
  const ids = [...state.selected];
  if (!ids.length) return toast('先勾选问题项', true);
  if (op === 'false_positive' && !confirm(`把 ${ids.length} 个问题标记为误报？`)) return;
  try {
    await api('/api/issues/batch', { method: 'POST', body: { ids, op, value } });
    toast(`已处理 ${ids.length} 项`);
    state.selected.clear();
    route();
  } catch (e) { toast(e.message, true); }
}

async function batchAssign() {
  const ids = [...state.selected];
  if (!ids.length) return toast('先勾选问题项', true);
  // 按项目成员选，而不是全局用户表：指派给一个看不到这个项目的人没有意义
  const members = await assignableMembers(null);
  if (!members.length) return toast('本项目还没有其他成员可指派', true);
  const uid = await pickAssignee(members, { title: `批量指派 ${ids.length} 项给…` });
  if (!uid) return;
  batch('assign', uid);
}

/* ---------- 表头多选筛选器 ---------- */

// multiSelect 生成「按钮 + 勾选浮层」。用原生 popover：点击别处自动收起（light dismiss）、
// 渲染在顶层——表头外面套着 .tbl-wrap{overflow:auto}，自己画的绝对定位浮层会被裁掉。
// 勾选**不**立刻重排，等浮层关掉再一次性应用：一次筛选一次请求，也免得点第二下时
// 浮层被重渲染冲掉。
//
// sel 为空 = 全选 = 这一项不筛。所以默认状态下每个框都是勾上的（「全选」是常态，
// 不是要人手动点一下才生效），取消勾选才是筛选动作。
function multiSelect(key, name, list, sel) {
  const all = sel.length === 0; // 空 = 全勾
  const opts = list.map(([v, l]) => `<label class="ms-item"><input type="checkbox" value="${esc(v)}"
    ${all || sel.includes(String(v)) ? 'checked' : ''}> ${esc(l)}</label>`).join('');
  return { html: `
    <label class="th-name">${esc(name)}</label>
    <button class="ms-btn" type="button" popovertarget="ms-${key}" data-ms="${key}">${esc(msLabel(list, sel, `全部${name}`))} ▾</button>
    <div class="ms-pop" id="ms-${key}" popover data-ms="${key}">
      <button class="ms-all" type="button" data-ms-all="1">${all ? '清空' : '全选'}</button>${opts}</div>` };
}

// MS_NONE 代表「这一项一个都没勾」。它在任何枚举里都不可能存在，于是进到
// IN (...) 里天然筛出 0 条。之所以要这么个哨兵：全勾和全不勾在传输层上都会变成
// 空串，而这两件事的含义正好相反（全勾=不筛，全不勾=一个都不要）。
const MS_NONE = '__none__';

// msVals 把浮层里的勾选归一成筛选值：
//   全勾   → 空数组 = 不筛这一项。存空数组而不是「所有值的列表」，是为了不带冗余参数，
//            将来后端加了新枚举值也自动落在「全部」里（存列表的话新值会被漏掉）。
//   全不勾 → 哨兵值 = 一条都不要
//   勾一部分 → 就筛这几个
function msVals(boxes) {
  const on = boxes.filter((n) => n.checked).map((n) => n.value);
  if (on.length === boxes.length) return [];
  return on.length ? on : [MS_NONE];
}

// msLabel 是按钮上的文案：不筛显示「全部X」，只留一个显示它，留多个显示第一个「+N」，
// 一个都没勾显示「未选择」（列表会是空的，得让人知道为什么）。
// 全列出来会把表头顶宽。
function msLabel(list, sel, all) {
  if (!sel.length) return all;
  if (sel.length === 1 && sel[0] === MS_NONE) return '未选择';
  const label = (v) => ((list.find(([x]) => String(x) === String(v)) || [])[1]) || String(v);
  return sel.length === 1 ? label(sel[0]) : `${label(sel[0])} +${sel.length - 1}`;
}

// 浮层默认被 UA 样式居中在视口，打开时贴到按钮下面
document.addEventListener('toggle', (e) => {
  const p = e.target;
  if (!p.classList || !p.classList.contains('ms-pop')) return;
  if (e.newState === 'open') {
    const r = p.previousElementSibling.getBoundingClientRect();
    Object.assign(p.style, { top: `${r.bottom + 6}px`, left: `${r.left}px` });
  } else {
    msApply(p);
  }
}, true);

// msApply 把浮层里的勾选落进筛选条件。light dismiss 每次关闭都会走到这儿，
// 没变就不重排（否则「点开又点掉」也会白刷一次列表）。
function msApply(p) {
  const key = p.dataset.ms;
  const vals = msVals([...p.querySelectorAll('input')]);
  if (vals.join(',') === (state.issues[key] || []).join(',')) return;
  state.issues[key] = vals;
  state.issues.page = 1;
  state.selected.clear(); // 换条件后旧勾选可能不在列表里，批量操作会误伤看不见的行
  route();
}

// 「全选 / 清空」是个开关：全勾着就清空，否则全勾上。只改勾选、不当场生效——
// 这个按钮最常见的用法是「清空，再勾一个」，当场重排会把浮层换掉，第二下就没得点了。
document.addEventListener('click', (e) => {
  const b = e.target.closest && e.target.closest('[data-ms-all]');
  if (!b) return;
  const p = b.closest('.ms-pop');
  const boxes = [...p.querySelectorAll('input')];
  const turnOn = !boxes.every((n) => n.checked);
  boxes.forEach((n) => { n.checked = turnOn; });
  b.textContent = turnOn ? '清空' : '全选';
});

/* ---------- 指派：搜索式人选选择器（§4.5） ---------- */

// pickAssignee 弹出「搜索框 + 人选列表」，resolve 选中的 user_id（选「不指派」得 0）。
// 搜索同时匹配中文名、登录名、以及身份归并里绑到这个人的 git 身份（dengquanjie1 这类英文名）——
// 指派的人往往只记得住对方「平时被怎么叫」，中文名未必想得起来。
//
// 关掉抽屉（点遮罩/Esc）就不 resolve：await 一直挂着，等于「什么都没做」，这是对的。
function pickAssignee(members, opts = {}) {
  return new Promise((resolve) => {
    const body = document.createElement('div');
    body.className = 'stack';
    body.innerHTML = `
      <div class="field"><label for="as-kw">搜索成员</label>
        <input class="inp" id="as-kw" autocomplete="off"
          placeholder="中文名 / 登录名 / git 身份，如 邓全杰、dengquanjie1"></div>
      <div class="pick-list" id="as-list"></div>`;
    openDrawer(opts.title || '指派给…', body);

    const list = $('#as-list', body);
    const kw = $('#as-kw', body);
    const row = (uid, name, sub, on) => `<button class="pick-item${on ? ' on' : ''}" data-uid="${uid}">
      <b>${esc(name)}</b>${sub ? `<small>${esc(sub)}</small>` : ''}</button>`;

    const render = () => {
      const q = kw.value.trim().toLowerCase();
      const hit = members.filter((m) => !q
        || [m.name, m.username, ...(m.aliases || [])].some((s) => (s || '').toLowerCase().includes(q)));
      list.innerHTML = (opts.allowClear ? row(0, '不指派', '清空负责人') : '')
        + hit.map((m) => row(m.user_id, m.name || '#' + m.user_id,
          [m.username, ...(m.aliases || [])].filter(Boolean).join(' · '), m.user_id === opts.current)).join('')
        + (hit.length ? '' : '<div class="muted" style="padding:8px 2px">没有匹配的成员</div>');
    };
    kw.oninput = render;
    list.onclick = (e) => {
      const b = e.target.closest('[data-uid]');
      if (!b) return;
      closeDrawer();
      resolve(+b.dataset.uid);
    };
    render();
    kw.focus(); // 打开就能直接打字，不用先点一下输入框
  });
}

// assignableMembers 取该项目里可被指派的人。projectID 为 0 时用当前项目。
async function assignableMembers(projectID) {
  const pid = projectID || state.projectId;
  if (!pid) return [];
  try { return await api(`/api/projects/${pid}/members`); } catch { return []; }
}

/* ---------- 问题详情 ---------- */

async function openIssue(id) {
  const d = await api('/api/issues/' + id);
  const i = d.issue;
  const body = document.createElement('div');
  body.className = 'stack';
  body.innerHTML = `
    <div class="row">
      <span class="tag ${esc(i.severity)}">${esc(i.severity)}</span>
      <span class="tag ${esc(i.status)}">${esc(i.status_name)}</span>
      ${i.regression ? '<span class="tag reg">回归</span>' : ''}
      <span class="muted">${esc(i.category)} · ${i.source === 'lint' ? 'golangci-lint / ' + esc(i.linter) : 'AI'}</span>
    </div>
    <h2 style="margin:0;font-size:17px">${esc(i.title)}</h2>
    ${kv([
      ['位置', `<span class="path">${esc(i.file_path)}:${i.line_start}${i.line_end > i.line_start ? '-' + i.line_end : ''}</span>`],
      ['子系统', esc(i.subsystem)],
      ['仓库', esc(i.repo_name)],
      ['提交', `<span class="mono">${esc((i.commit_sha || '').slice(0, 8) || '—')}</span> ${esc(i.committer || '')}`],
      ['归属人', esc(i.assignee_name || '未指派')],
      ['首次出现', esc(i.first_seen_at)],
      ['最近出现', `${esc(i.last_seen_at)}（累计命中 ${i.seen_count} 次）`],
    ])}
    ${i.description ? `<div><h3 class="muted" style="font-size:12px">问题说明</h3><p style="margin:4px 0 0">${esc(i.description)}</p></div>` : ''}
    ${i.suggestion ? `<div><h3 class="muted" style="font-size:12px">修复建议</h3><p style="margin:4px 0 0">${esc(i.suggestion)}</p></div>` : ''}
    ${i.code_snippet ? `<div><h3 class="muted" style="font-size:12px">代码片段</h3>${codeBlock(i.code_snippet)}</div>` : ''}
    <div><h3 class="muted" style="font-size:12px">状态流转</h3>
      <div class="row">
        ${Object.entries(STATUS).map(([k, v]) =>
          `<button class="btn sm ${k === i.status ? 'primary' : ''}" data-st="${k}">${v}</button>`).join('')}
        <button class="btn sm" id="assign-btn">指派给…</button>
      </div>
    </div>
    <div><h3 class="muted" style="font-size:12px">操作记录</h3>
      <div class="timeline">${d.events.length ? d.events.map((e) =>
        `<div class="ev"><time>${fmtTime(e.created_at)}</time><span>${esc(e.operator || '系统')}：
          ${esc(STATUS[e.from_status] || e.from_status || '—')} → ${esc(STATUS[e.to_status] || e.to_status)}
          ${e.note ? '· ' + esc(e.note) : ''}</span></div>`).join('') : '<span class="muted">暂无</span>'}</div>
    </div>
    <div><h3 class="muted" style="font-size:12px">讨论</h3>
      <div class="stack" id="comments">${d.comments.map((c) =>
        `<div class="comment"><div class="meta">${esc(c.user_name || '匿名')} · ${esc(c.created_at)}</div>${esc(c.content)}</div>`).join('')}</div>
      <textarea class="inp" id="cmt" placeholder="写点结论，比如『已确认，下个版本修』" style="margin-top:10px"></textarea>
      <div class="row" style="margin-top:8px"><button class="btn primary" id="cmt-go">发表评论</button></div>
    </div>`;

  openDrawer(`问题 #${i.id}`, body);

  body.querySelectorAll('[data-st]').forEach((b) => b.onclick = async () => {
    try {
      await api(`/api/issues/${id}/status`, { method: 'POST', body: { status: b.dataset.st } });
      toast('状态已更新');
      openIssue(id);
    } catch (e) { toast(e.message, true); }
  });
  $('#assign-btn', body).onclick = async () => {
    const members = await assignableMembers(i.project_id);
    if (!members.length) return toast('本项目还没有其他成员可指派', true);
    const uid = await pickAssignee(members, {
      title: `指派问题 #${i.id} 给…`, current: i.assignee_id, allowClear: !!i.assignee_id,
    });
    try {
      await api(`/api/issues/${id}/assign`, { method: 'POST', body: { assignee_id: uid } });
      toast(uid ? '已指派并通知飞书' : '已清空负责人');
      openIssue(id);
    } catch (e) { toast(e.message, true); }
  };
  $('#cmt-go', body).onclick = async () => {
    const content = $('#cmt', body).value.trim();
    if (!content) return;
    try {
      await api(`/api/issues/${id}/comments`, { method: 'POST', body: { content } });
      openIssue(id);
    } catch (e) { toast(e.message, true); }
  };
}

/* ---------- 报告 ---------- */

async function viewReports(el) {
  const data = await api('/api/reports?' + qs({ ...range(), page: 1, limit: 50 }));
  const cols = ['#', '仓库', '提交数', '文件数', '问题', '高危', '模型', 'prompt', '时间'];
  el.innerHTML = '';
  el.append(table(cols, data.items, (r) => `
    <tr data-id="${r.id}">
      <td class="mono">${r.id}</td>
      <td>${esc(r.repo_name)}</td>
      <td class="num">${r.commits.length}</td>
      <td class="num">${r.files.length}</td>
      <td class="num">${r.issue_count}</td>
      <td class="num" style="color:${r.high_count ? 'var(--high)' : 'inherit'}">${r.high_count}</td>
      <td class="muted">${esc(r.model_name)}</td>
      <td class="muted mono">${esc(r.prompt_version)} / ${esc(r.spec_version)}</td>
      <td class="mono muted">${fmtTime(r.created_at)}</td>
    </tr>`));
  const tb = el.querySelector('tbody');
  if (tb) tb.onclick = (e) => {
    const tr = e.target.closest('tr[data-id]');
    if (tr) openReport(+tr.dataset.id);
  };
}

async function openReport(id) {
  const d = await api('/api/reports/' + id);
  const r = d.report;
  const body = document.createElement('div');
  body.className = 'stack';
  body.innerHTML = `
    ${kv([
      ['报告', '#' + r.id],
      ['仓库', esc(r.repo_name)],
      ['版本标签', `<span class="mono">${esc(r.model_name)} · ${esc(r.model_version || 'n/a')} · prompt ${esc(r.prompt_version)} · 规范 ${esc(r.spec_version)}</span>`],
      ['统计', `问题 ${r.issue_count} 条，其中高危 <b style="color:var(--high)">${r.high_count}</b> 条`],
      ['时间', esc(r.created_at)],
    ])}
    <div><h3 class="muted" style="font-size:12px">涉及的提交（${r.commits.length}）</h3>
      <div class="mono muted" style="font-size:12px;line-height:1.9">${r.commits.map((c) => esc(c.slice(0, 10))).join('<br>') || '—'}</div></div>
    <div><h3 class="muted" style="font-size:12px">涉及的文件（${r.files.length}）</h3>
      <div class="path" style="line-height:1.9">${r.files.map(esc).join('<br>') || '—'}</div></div>
    <div><h3 class="muted" style="font-size:12px">问题清单（${d.issues.length}）</h3>
      ${d.issues.map((i) => `<div class="comment" style="cursor:pointer" data-issue="${i.id}">
        <div class="meta"><span class="tag ${esc(i.severity)}">${esc(i.severity)}</span> ${esc(i.category)} · ${esc(i.source)}</div>
        <b>${esc(i.title)}</b><br><span class="path">${esc(i.file_path)}:${i.line_start}</span></div>`).join('') || '<span class="muted">无</span>'}
    </div>`;
  openDrawer(`报告 #${r.id}`, body);
  body.querySelectorAll('[data-issue]').forEach((n) => n.onclick = () => openIssue(+n.dataset.issue));
}

/* ---------- 任务 ---------- */

async function viewTasks(el) {
  const data = await api('/api/tasks?' + qs({ ...range(), page: 1, limit: 50 }));
  const cols = ['#', '仓库', '触发', '状态', '重试', '模型', '排队', '耗时', ''];
  el.innerHTML = '';
  el.append(table(cols, data.items, (t) => {
    const dur = t.started_at && t.finished_at
      ? ((new Date(t.finished_at.replace(' ', 'T') + 'Z') - new Date(t.started_at.replace(' ', 'T') + 'Z')) / 60000).toFixed(1) + ' 分'
      : (t.started_at ? '进行中' : '—');
    return `<tr data-id="${t.id}">
      <td class="mono">${t.id}</td>
      <td>${esc(t.repo_name)}</td>
      <td class="muted">${esc(t.trigger_type)}</td>
      <td><span class="tag ${t.status === 'success' ? 'fixed' : t.status === 'failed' ? '高危' : t.status === 'running' ? 'pending' : '风格'}">${esc(t.status)}</span>
        ${t.error ? `<br><small class="muted">${esc(t.error.slice(0, 70))}</small>` : ''}</td>
      <td class="num">${t.retry_count}</td>
      <td class="muted">${esc(t.model_name || '—')}</td>
      <td class="mono muted">${fmtTime(t.queued_at)}</td>
      <td class="mono muted">${dur}</td>
      <td class="actions"><button class="btn sm" data-retry="${t.id}">重试</button>
        ${t.report_id ? `<button class="btn sm" data-report="${t.report_id}">报告</button>` : ''}</td>
    </tr>`;
  }));
  const tb = el.querySelector('tbody');
  if (tb) tb.onclick = async (e) => {
    const rt = e.target.closest('[data-retry]');
    if (rt) {
      e.stopPropagation();
      await api(`/api/tasks/${rt.dataset.retry}/retry`, { method: 'POST' });
      toast('已重新入队');
      route();
      return;
    }
    const rp = e.target.closest('[data-report]');
    if (rp) { e.stopPropagation(); openReport(+rp.dataset.report); return; }
    const tr = e.target.closest('tr[data-id]');
    if (tr) openTaskLogs(+tr.dataset.id);
  };
}

async function openTaskLogs(id) {
  const [task, logs] = await Promise.all([api('/api/tasks/' + id), api(`/api/tasks/${id}/logs`)]);
  const body = document.createElement('div');
  body.className = 'stack';
  body.innerHTML = kv([
    ['任务', '#' + task.id],
    ['仓库', esc(task.repo_name)],
    ['状态', `${esc(task.status)}（重试 ${task.retry_count} 次）`],
    ['版本', `<span class="mono">${esc(task.model_name)} · prompt ${esc(task.prompt_version)} · 规范 ${esc(task.spec_version)}</span>`],
    // 手动触发可能给的是时间范围，这时 commit_from/to 是空的，别显示成「HEAD~ → HEAD」
    ['区间', task.time_from || task.time_to
      ? `<span class="mono">${esc(task.time_from || '不限')} → ${esc(task.time_to || '不限')}</span>（按时间筛）`
      : `<span class="mono">${esc((task.commit_from || 'HEAD~').slice(0, 8))} → ${esc((task.commit_to || 'HEAD').slice(0, 8))}</span>`],
  ]) + `<div class="logbox">${logs.map((l) =>
    `<div class="${esc(l.level)}"><time>${esc(l.ts.slice(11))}</time>${esc(l.message)}</div>`).join('') || '暂无日志'}</div>`;
  openDrawer(`任务 #${task.id}`, body);
}

/* ---------- 统计报表 ---------- */

async function viewStats(el) {
  const bucket = el._bucket || 'week';
  const by = el._by || 'category';
  const [trend, ov, dist, assignees, versions] = await Promise.all([
    api('/api/stats/trend?' + qs({ ...range(), bucket })),
    api('/api/stats/overview?' + qs(range())),
    api('/api/stats/distribution?' + qs({ ...range(), by })),
    api('/api/stats/assignees?' + qs(range())),
    api('/api/stats/versions?' + qs({ ...range(), bucket })),
  ]);

  el.innerHTML = '';
  const head = document.createElement('div');
  head.className = 'toolbar';
  head.innerHTML = `<div class="chips">
      ${[['day', '按天'], ['week', '按周'], ['month', '按月']].map(([v, l]) =>
        `<button class="chip ${bucket === v ? 'on' : ''}" data-bucket="${v}">${l}</button>`).join('')}
    </div><span class="spacer"></span>
    <button class="btn sm" data-csv="trend">导出趋势</button>`;

  const t = document.createElement('div');
  t.className = 'card glow';
  t.innerHTML = `<h3>问题趋势（${bucket === 'day' ? '按天' : bucket === 'month' ? '按月' : '按周'} · 堆叠）</h3>`;
  t.append(stackedTrend(trend, versionBreaks(trend, versions)));

  const ver = document.createElement('div');
  ver.className = 'card';
  ver.innerHTML = `<h3>版本标签断点（模型 / prompt / 规范文档）</h3>` + (versions.length
    ? `<div class="tbl-wrap"><table><thead><tr><th>模型</th><th>模型版本</th><th>prompt</th><th>规范文档</th><th>生效时间</th><th class="num">报告数</th></tr></thead>
      <tbody>${versions.map((v) => `<tr><td>${esc(v.model_name)}</td><td class="mono muted">${esc(v.model_version || 'n/a')}</td>
        <td class="mono">${esc(v.prompt_version)}</td><td class="mono muted">${esc(v.spec_version)}</td>
        <td class="mono muted">${esc(v.first_at)}</td><td class="num">${v.report_count}</td></tr>`).join('')}</tbody></table></div>`
    : '<span class="muted">暂无数据</span>');

  const distCard = document.createElement('div');
  distCard.className = 'card';
  distCard.innerHTML = `<h3>分布</h3>
    <div class="chips" style="margin-bottom:12px">
      ${[['category', '按分类'], ['source', '按来源'], ['repo', '按仓库'], ['subsystem', '按子系统']].map(([v, l]) =>
        `<button class="chip ${by === v ? 'on' : ''}" data-by="${v}">${l}</button>`).join('')}
    </div>`;
  distCard.append(distBars(dist, { limit: 20 }));
  const csvBtn = document.createElement('button');
  csvBtn.className = 'btn sm';
  csvBtn.textContent = '导出分布';
  csvBtn.style.marginTop = '12px';
  csvBtn.onclick = () => {
    location.href = '/api/stats/export?' + qs({ type: 'distribution', by, ...range() });
  };
  distCard.append(csvBtn);

  // 效能类：仅全局聚合，不按人拆分（§附录决策 10）
  const eff = document.createElement('div');
  eff.className = 'card';
  eff.innerHTML = `<h3>效能</h3>` + kv([
    ['本期新增', String(ov.new_count)],
    ['处理率', (ov.handle_rate * 100).toFixed(1) + '%'],
    ['误报率', (ov.false_rate * 100).toFixed(1) + '%'],
    ['平均处理时长', ov.avg_handle_hours.toFixed(1) + ' 小时'],
    ['回归次数', String(ov.regression)],
  ]) + '<div class="hint muted" style="margin-top:8px;font-size:11.5px">处理时长为全局聚合，不按人拆分。</div>';

  const ppl = document.createElement('div');
  ppl.className = 'card';
  ppl.innerHTML = `<h3>人员（处理数量）</h3>` + (assignees.length
    ? `<div class="tbl-wrap"><table><thead><tr><th>负责人</th><th class="num">分派</th><th class="num">已处理</th></tr></thead>
      <tbody>${assignees.map((a) => `<tr><td>${esc(a.name || '#' + a.user_id)}</td>
        <td class="num">${a.total}</td><td class="num">${a.handled}</td></tr>`).join('')}</tbody></table></div>`
    : '<span class="muted">暂无指派数据</span>');

  const grid = document.createElement('div');
  grid.className = 'grid2';
  grid.append(distCard, eff);
  const grid2 = document.createElement('div');
  grid2.className = 'grid2';
  grid2.style.marginTop = '14px';
  grid2.append(ppl, ver);

  const stack = document.createElement('div');
  stack.className = 'stack';
  stack.append(head, t, grid, grid2);
  el.append(stack);

  head.onclick = (e) => {
    const b = e.target.closest('[data-bucket]');
    if (b) { el._bucket = b.dataset.bucket; viewStats(el); }
    if (e.target.closest('[data-csv]')) {
      location.href = '/api/stats/export?' + qs({ type: 'trend', bucket, ...range() });
    }
  };
  distCard.onclick = (e) => {
    const b = e.target.closest('[data-by]');
    if (b) { el._by = b.dataset.by; viewStats(el); }
  };
}

/* ---------- 仓库表单（项目页里调用） ---------- */

function repoForm(repo, projectID) {
  const v = (k, d = '') => esc(repo ? (repo[k] ?? d) : d);
  const list = (a) => esc((a || []).join('\n'));
  const body = document.createElement('div');
  body.className = 'stack';
  body.innerHTML = `
    <div class="grid2">
      <div class="field"><label>仓库名称 *</label><input class="inp" id="f-name" value="${v('name')}"></div>
      <div class="field"><label>所属项目 *</label>
        <select class="inp sel" id="f-project">${(state.projects || []).map((p) =>
          `<option value="${p.id}" ${p.id === (repo ? repo.project_id : projectID) ? 'selected' : ''}>${esc(p.name)}</option>`).join('')}</select></div>
    </div>
    <div class="field"><label>所属团队</label><input class="inp" id="f-team" value="${v('team')}"></div>
    <div class="field"><label>Git 地址 *</label><input class="inp" id="f-url" value="${v('git_url')}" placeholder="http://gitlab.xxx/slg/server.git"></div>
    <div class="grid3">
      <div class="field"><label>分支</label><input class="inp" id="f-branch" value="${v('branch', 'master')}"></div>
      <div class="field"><label>语言</label>
        <select class="inp sel" id="f-lang">
          ${LANGS.map(([k, n]) => `<option value="${k}" ${(repo?.language || 'go') === k ? 'selected' : ''}>${n}</option>`).join('')}
        </select>
        <span class="hint">目前只有 Go 有完整的函数级上下文与 lint 接入。其他语言可跑，但只能看到改动块本身，跨文件问题（重复造轮子/N+1）检出率会明显下降。</span></div>
      <div class="field"><label>默认模型</label><input class="inp" id="f-model" value="${v('default_model')}" placeholder="留空则用全局默认"></div>
    </div>
    <div class="field"><label>访问凭据（只读 token）</label>
      <input class="inp" id="f-cred" placeholder="${repo && repo.has_credential ? '已配置 ' + esc(repo.credential_mask) + '，留空表示不修改' : '不填则为公开仓库'}">
      <span class="hint">加密存储，界面不回显，日志不打印。同一条也用作 webhook 的 Secret token。</span></div>
    <div class="field"><label>飞书机器人 Webhook</label>
      <div class="row">
        <input class="inp" id="f-hook" style="flex:1" placeholder="${repo && repo.has_webhook ? '已配置 ' + esc(repo.webhook_mask) + '，留空表示不修改' : '不填则回落全局（设置 → 通知）'}">
        <button class="btn" id="f-hook-test" type="button">发送测试</button>
      </div>
      <span class="hint" id="f-hook-msg">这个仓库的审核通知发到哪个群。留空则回落到全局默认。
        URL 里带着 token（拿到就能往群里发消息），所以和代码凭据一样加密存储、界面只回显形态。</span></div>
    <div class="grid2">
      <div class="field"><label>路径包含 glob（每行一个，留空=全部）</label><textarea class="inp" id="f-inc">${list(repo && repo.include_globs)}</textarea></div>
      <div class="field"><label>路径排除 glob（每行一个）</label><textarea class="inp" id="f-exc">${list(repo && repo.exclude_globs)}</textarea>
        <span class="hint">默认排除 autogen / *.pb.go / /tbl/，与 .golangci.yml 一致。生成代码占变更约 45%。</span></div>
    </div>
    <div class="field"><label>挂载的审核规范文档（每行一个路径）</label>
      <textarea class="inp" id="f-spec">${list(repo && repo.spec_docs)}</textarea>
      ${repo ? '<div class="row" style="margin-top:8px"><button class="btn sm" id="f-spec-pick" type="button">从仓库点选</button></div>' : ''}
      <div id="f-spec-cands"></div>
      <span class="hint">相对路径在仓库内解析（如 CLAUDE.md、docs/review-checklist.md），推荐用这种，零配置且天然受限。绝对路径需要配 AIUDGE_SPEC_DIRS 白名单。文档内容参与版本号计算，改动会在趋势图上留下断点。</span></div>
    <div class="grid3">
      <div class="field"><label>Webhook 触发</label>
        <label class="row"><input type="checkbox" id="f-wh" ${!repo || repo.trigger_webhook ? 'checked' : ''}> 启用</label></div>
      <div class="field"><label>定时兜底 cron</label><input class="inp" id="f-cron" value="${v('trigger_cron')}" placeholder="0 22 * * *"></div>
      <div class="field"><label>启用审核</label>
        <label class="row"><input type="checkbox" id="f-on" ${!repo || repo.enabled ? 'checked' : ''}> 启用</label></div>
    </div>
    <div class="row">
      <button class="btn primary" id="save">保存</button>
      ${repo ? '<button class="btn danger" id="del">删除仓库</button>' : ''}
    </div>
    ${repo ? `<div class="hint muted">Webhook 地址：<span class="mono">${esc(location.origin)}/api/webhook/${repo.id}</span>（GitLab 侧 Secret token 填上面的访问凭据）</div>` : ''}`;

  if (!state.projects || !state.projects.length) {
    api('/api/projects').then((ps) => { state.projects = ps; });
  }
  openDrawer(repo ? `编辑仓库 ${repo.name}` : '新增仓库', body);
  const val = (id) => $('#' + id, body).value.trim();

  // 输入框里填了就先试那个；留空则让后端按仓库解析已存的地址。
  // 仓库没单独配时会落到全局兜底，这时后端会回报 via_global，如实告诉人测的是哪个。
  $('#f-hook-test', body).onclick = async () => {
    const msg = $('#f-hook-msg', body);
    msg.textContent = '发送中…';
    msg.style.color = '';
    try {
      const r = await api('/api/notify-config/test', {
        method: 'POST',
        body: { webhook: val('f-hook'), repo_id: repo ? repo.id : 0 },
      });
      msg.style.color = 'var(--ok)';
      msg.textContent = r.via_global
        ? `已发送到 ${r.sent_to}（这是全局兜底的那个群，本仓库还没单独配）`
        : `已发送到 ${r.sent_to}，去群里看看`;
    } catch (e) {
      msg.style.color = 'var(--high)';
      msg.textContent = e.message;
    }
  };
  const lines = (id) => val(id).split('\n').map((s) => s.trim()).filter(Boolean);

  // 从仓库拉一份文档清单让人点选：路径是相对工作副本的，正好是管线读文档时用的写法
  const pick = $('#f-spec-pick', body);
  if (pick) pick.onclick = async () => {
    const box = $('#f-spec-cands', body);
    box.innerHTML = '<div class="card"><span class="spin"></span> 扫描中…</div>';
    let r;
    try { r = await api(`/api/repos/${repo.id}/spec-docs`); }
    catch (e) { box.innerHTML = `<div class="card">${esc(e.message)}</div>`; return; }
    if (!r.items.length) {
      box.innerHTML = '<div class="card">仓库里没扫到文档（.md / .markdown / .rst / .txt / .adoc）</div>';
      return;
    }
    const mounted = new Set(lines('f-spec'));
    box.innerHTML = `<div class="card"><h3>仓库里的文档（${r.items.length}），勾选后加入</h3>
      <input class="inp" id="f-spec-filter" placeholder="过滤路径…">
      <div class="tbl-wrap" style="max-height:260px;margin-top:8px"><table><tbody>${r.items.map((p) =>
        `<tr data-p="${esc(p)}"><td><input type="checkbox" ${mounted.has(p) ? 'checked disabled' : ''}></td>
          <td class="path">${esc(p)}</td></tr>`).join('')}</tbody></table></div>
      <div class="row" style="margin-top:10px"><button class="btn primary" id="f-spec-apply">加入</button>
        <button class="btn" id="f-spec-close">收起</button></div></div>`;
    const trs = [...box.querySelectorAll('tbody tr')];
    box.querySelector('#f-spec-filter').focus();
    box.querySelector('#f-spec-filter').oninput = (e) => {
      const q = e.target.value.trim().toLowerCase();
      // 只隐藏行、不重绘：重绘会把勾选状态一起丢掉
      trs.forEach((tr) => { tr.style.display = !q || tr.dataset.p.toLowerCase().includes(q) ? '' : 'none'; });
    };
    box.querySelector('#f-spec-close').onclick = () => { box.innerHTML = ''; };
    box.querySelector('#f-spec-apply').onclick = () => {
      const picked = trs.filter((tr) => {
        const cb = tr.querySelector('input');
        return cb.checked && !cb.disabled; // 已在列表里的不重复加
      }).map((tr) => tr.dataset.p);
      if (!picked.length) return toast('先勾几个文档', true);
      const ta = $('#f-spec', body);
      ta.value = (ta.value.trim() ? ta.value.trimEnd() + '\n' : '') + picked.join('\n') + '\n';
      box.innerHTML = '';
      toast(`已加入 ${picked.length} 个文档，记得点保存`);
    };
  };

  $('#save', body).onclick = async () => {
    const payload = {
      project_id: +$('#f-project', body).value,
      name: val('f-name'), git_url: val('f-url'), branch: val('f-branch') || 'master',
      team: val('f-team'), language: $('#f-lang', body).value, default_model: val('f-model'),
      include_globs: lines('f-inc'), exclude_globs: lines('f-exc'), spec_docs: lines('f-spec'),
      trigger_webhook: $('#f-wh', body).checked, trigger_cron: val('f-cron'),
      enabled: $('#f-on', body).checked,
    };
    const cred = val('f-cred');
    if (cred || !repo) payload.credential = cred;
    const hook = val('f-hook');
    if (hook || !repo) payload.feishu_webhook = hook;
    if (!payload.name || !payload.git_url) return toast('名称与 Git 地址必填', true);
    if (!payload.project_id) return toast('请选择所属项目', true);
    try {
      if (repo) await api('/api/repos/' + repo.id, { method: 'PUT', body: payload });
      else await api('/api/repos', { method: 'POST', body: payload });
      toast('已保存');
      closeDrawer();
      route();
    } catch (e) { toast(e.message, true); }
  };
  const del = $('#del', body);
  if (del) del.onclick = async () => {
    if (!confirm(`删除仓库 ${repo.name}？历史报告与问题会保留。`)) return;
    try { await api('/api/repos/' + repo.id, { method: 'DELETE' }); closeDrawer(); route(); }
    catch (e) { toast(e.message, true); }
  };
}

/* ---------- 子系统规则（按仓库） ---------- */

async function subsystemRules(repo) {
  const list = await api(`/api/repos/${repo.id}/subsystems`);
  const body = document.createElement('div');
  body.className = 'stack';
  body.innerHTML = `
    <div class="hint muted">规则只在「${esc(repo.name)}」这个仓库内生效，其他仓库各配各的。
      按顺序匹配文件路径的子串，命中第一条即归类，没命中的归「未分类」。</div>
    <div class="row">
      <button class="btn" id="s-scan">扫描仓库目录，生成候选</button>
      <button class="btn sm" id="s-add">+ 加一行</button>
      <span class="spacer"></span>
      <span class="muted" id="s-count">${list.length} 条</span>
    </div>
    <div id="s-cands"></div>
    <div class="field"><label>规则（每行「路径子串=子系统名」，自上而下优先）</label>
      <textarea class="inp" id="s-rules" style="min-height:320px;font-family:var(--mono);font-size:12px">${esc(list.map((r) => `${r.pattern}=${r.name}`).join('\n'))}</textarea></div>
    <div class="row"><button class="btn primary" id="s-save">保存</button></div>`;

  openDrawer(`子系统规则 · ${repo.name}`, body);

  const textarea = $('#s-rules', body);
  $('#s-add', body).onclick = () => { textarea.value += '\n='; textarea.focus(); };
  $('#s-save', body).onclick = async () => {
    const rules = textarea.value.split('\n').map((l) => l.trim()).filter(Boolean)
      .map((l) => { const i = l.indexOf('='); return { pattern: l.slice(0, i).trim(), name: l.slice(i + 1).trim() }; })
      .filter((r) => r.pattern && r.name);
    try {
      await api(`/api/repos/${repo.id}/subsystems`, { method: 'PUT', body: rules });
      toast(`已保存 ${rules.length} 条规则`);
      closeDrawer();
    } catch (e) { toast(e.message, true); }
  };

  // 扫目录只给候选，名称留空——目录名离「战斗系统」这种业务名词还差一层人工判断
  $('#s-scan', body).onclick = async () => {
    const box = $('#s-cands', body);
    box.innerHTML = '<div class="card"><span class="spin"></span> 扫描中…</div>';
    try {
      const r = await api(`/api/repos/${repo.id}/subsystems/scan`, { method: 'POST' });
      if (!r.candidates.length) { box.innerHTML = ''; return toast('没扫到合适的目录', true); }
      const existing = new Set(textarea.value.split('\n').map((l) => l.split('=')[0].trim()));
      box.innerHTML = `<div class="card"><h3>候选（${r.candidates.length}，按文件数排序）——填上名字再点「加入规则」</h3>
        <div class="tbl-wrap" style="max-height:260px"><table><thead><tr><th></th><th>路径</th><th class="num">文件</th><th>名称</th></tr></thead>
        <tbody>${r.candidates.map((c, i) => `<tr>
          <td><input type="checkbox" data-c="${i}" ${existing.has(c.pattern) ? '' : 'checked'}></td>
          <td class="path" title="${esc(c.sample)}">${esc(c.pattern)}</td>
          <td class="num">${c.files}</td>
          <td><input class="inp" data-n="${i}" style="width:100%" placeholder="如 战斗系统"></td>
        </tr>`).join('')}</tbody></table></div>
        <div class="row" style="margin-top:10px"><button class="btn primary" id="s-apply">加入规则</button>
          <button class="btn" id="s-close">收起</button></div></div>`;
      box.querySelector('#s-close').onclick = () => { box.innerHTML = ''; };
      box.querySelector('#s-apply').onclick = () => {
        const picked = [];
        box.querySelectorAll('[data-c]').forEach((cb) => {
          if (!cb.checked) return;
          const i = +cb.dataset.c;
          const name = box.querySelector(`[data-n="${i}"]`).value.trim();
          if (name) picked.push(`${r.candidates[i].pattern}=${name}`);
        });
        if (!picked.length) return toast('至少给一个候选填上名称', true);
        textarea.value = textarea.value.trimEnd() + '\n' + picked.join('\n') + '\n';
        $('#s-count', body).textContent = `${textarea.value.split('\n').filter(Boolean).length} 条`;
        box.innerHTML = '';
        toast(`加入 ${picked.length} 条，记得点保存`);
      };
    } catch (e) {
      box.innerHTML = `<div class="card">${esc(e.message)}</div>`;
    }
  };
}

/* ---------- 手动触发审核（§4.1） ---------- */

// 选提交范围时，候选来自服务端的 /commits：它每次打开都先同步一次远端（只 fetch，
// 不动工作副本，所以不会和正在跑的审核抢工作区）。同步失败就退回本地副本，
// 并在下面如实写清「以下列表不是最新的」，不假装成功。
async function triggerForm(repo) {
  const body = document.createElement('div');
  body.className = 'stack';
  body.innerHTML = `
    <div class="row">
      <label class="row"><input type="radio" name="tg" value="commits" checked> 提交范围</label>
      <label class="row"><input type="radio" name="tg" value="times"> 时间范围</label>
      <span class="spacer"></span>
      <span class="hint muted">本次最多审 30 条提交，超出的只取最近 30 条</span>
    </div>
    <div id="t-commits">
      <div class="grid2">
        <div class="field"><label for="t-from">起始提交（不含）</label>
          <select class="inp sel" id="t-from"><option value="">（同步中…）</option></select>
          <span class="hint">不选起始 = 只审「结束」这一条</span></div>
        <div class="field"><label for="t-to">结束提交</label>
          <select class="inp sel" id="t-to"><option value="">（同步中…）</option></select></div>
      </div>
      <div class="row"><span class="sync-note" id="t-sync" aria-live="polite">同步中…</span>
        <span class="spacer"></span>
        <button class="btn sm" id="t-resync" type="button">重新同步</button></div>
    </div>
    <div id="t-times" hidden>
      <div class="grid2">
        <div class="field"><label for="t-tfrom">起始时间</label>
          <input class="inp" type="datetime-local" id="t-tfrom"></div>
        <div class="field"><label for="t-tto">结束时间</label>
          <input class="inp" type="datetime-local" id="t-tto"></div>
      </div>
      <span class="hint">按提交时间筛（本地时区）。留空表示不限那一端；两端都留空会捞出整段历史，只取最近 30 条。</span>
    </div>
    <div class="row"><button class="btn primary" id="t-go" type="button">开始审核</button></div>
    <div class="hint muted">触发的是一次真实的模型审核，会消耗网关额度。任务排队后可在「任务」页看进度。</div>`;

  openDrawer(`手动触发审核 · ${esc(repo.name)}`, body);

  const $1 = (s) => $(s, body);
  const load = async () => {
    const sync = $1('#t-sync');
    // 同步最长可能是一整次克隆（分钟级），这段时间按钮要禁用：连点会发出多次
    // fetch/克隆，同时抢工作副本
    const btn = $1('#t-resync');
    btn.disabled = true;
    // 同步最长是一整次克隆（分钟级）。先把下拉置回占位项：空白的下拉看起来像坏了
    sync.textContent = '同步中…';
    const pending = '<option value="">（同步中…）</option>';
    $1('#t-from').innerHTML = pending;
    $1('#t-to').innerHTML = pending;
    let r;
    try {
      r = await api(`/api/repos/${repo.id}/commits?limit=50`);
    } catch (e) {
      sync.textContent = '同步失败：' + e.message;
      btn.disabled = false;
      return;
    }
    const items = (r && r.items) || [];
    sync.textContent = r && r.synced
      ? `已同步远端 · ${fmtTime(r.synced_at)} · 共 ${items.length} 条`
      : `未能同步远端（${(r && r.sync_error) || '未知原因'}），以下是本地副本的提交`;
    // 提交标题可能很长，截断要看得见：切了就得带省略号，否则像是原文就这么短
    const label = (c) => `${esc(c.sha.slice(0, 8))} ${esc(c.subject.length > 60 ? c.subject.slice(0, 60) + '…' : c.subject)}`;
    const opts = items.map((c) => `<option value="${esc(c.sha)}" title="${esc(c.subject)}">${label(c)}</option>`).join('');
    if (!items.length) {
      $1('#t-from').innerHTML = '<option value="">（没有可用提交）</option>';
      $1('#t-to').innerHTML = '<option value="">（没有可用提交）</option>';
      btn.disabled = false;
      return;
    }
    $1('#t-from').innerHTML = '<option value="">（不选 = 只审结束那一条）</option>' + opts;
    // 结束默认最新一条（列表本身就是新到旧），也就是「只审最新一条」
    $1('#t-to').innerHTML = opts;
    $1('#t-to').selectedIndex = 0;
    btn.disabled = false;
  };
  $1('#t-resync').onclick = load;

  for (const rb of body.querySelectorAll('[name=tg]')) {
    rb.onchange = () => {
      const mode = body.querySelector('[name=tg]:checked').value;
      $1('#t-commits').hidden = mode !== 'commits';
      $1('#t-times').hidden = mode !== 'times';
    };
  }

  $1('#t-go').onclick = async () => {
    const go = $1('#t-go');
    // 浏览器不会给 disabled 的按钮派发 click，但「花两次钱」这件事值得多一道显式闸门：
    // 回车键、程序化触发、以及将来有人把按钮包进 form 里，都绕得过 disabled 那一层
    if (go.disabled) return;
    const mode = body.querySelector('[name=tg]:checked').value;
    const payload = {};
    if (mode === 'commits') {
      payload.from = $1('#t-from').value;
      payload.to = $1('#t-to').value;
      if (!payload.to) return toast('请选择结束提交', true);
      // 起始那一端是「不含」的边界：和结束选成同一条，区间就是空的，
      // 任务会「成功但什么都没审」——后端不会报错，所以在这里拦一下
      if (payload.from && payload.from === payload.to) {
        return toast('起始和结束是同一条提交，区间是空的', true);
      }
    } else {
      payload.time_from = $1('#t-tfrom').value;
      payload.time_to = $1('#t-tto').value;
      if (!payload.time_from && !payload.time_to) {
        if (!confirm('两端都没填，会捞出整段历史（只取最近 30 条）。继续？')) return;
      }
    }
    // 提交期间禁用按钮并显示进度：连点两下就是两次真实的大模型审核，
    // 是钱的问题，不只是难看
    go.disabled = true;
    const label = go.textContent;
    go.textContent = '排队中…';
    try {
      const r = await api(`/api/repos/${repo.id}/trigger`, { method: 'POST', body: payload });
      toast(`已排队：任务 #${r.task_id}`);
      closeDrawer();
    } catch (e) {
      toast(e.message, true);
      go.disabled = false;
      go.textContent = label;
    }
  };

  load();
}

/* ---------- 平台设置（#/admin） ----------
   模型网关、通知、Prompt、用户、身份归并、审计、维护——全平台共用，只有全局管理员能动。
   刻意不挂在项目页下面：全局管理员可能一个项目都没加入，挂进去他就永远够不着。
   项目那边的「设置」是另一回事（仓库/成员/信息），见 viewProjects。 */

async function viewAdminSettings(el) {
  const tabs = [['model', '模型配置'], ['notify', '通知'], ['prompt', '审核 Prompt'],
    ['users', '用户'], ['identities', '身份归并'], ['audit', '审计日志'], ['maint', '维护']];
  const cur = tabs.some(([v]) => v === el._tab) ? el._tab : 'model';
  el._tab = cur;
  el.innerHTML = '';
  const bar = document.createElement('div');
  bar.className = 'toolbar';
  bar.innerHTML = `<div class="chips">${tabs.map(([v, l]) =>
    `<button class="chip ${cur === v ? 'on' : ''}" data-tab="${v}">${l}</button>`).join('')}</div>`;
  const pane = document.createElement('div');
  el.append(bar, pane);
  bar.onclick = (e) => {
    const b = e.target.closest('[data-tab]');
    if (b) { el._tab = b.dataset.tab; viewAdminSettings(el); }
  };
  await ({ model: settingsModel, notify: settingsNotify, prompt: settingsPrompt,
    users: settingsUsers, identities: settingsIdentities,
    audit: settingsAudit, maint: settingsMaint }[cur])(pane);
}

async function settingsModel(pane) {
  let mc = await api('/api/model-config');
  // 地址配好了但列表是空的（首次进来、或上次拉取失败），自动拉一次，
  // 不用人先想到去点按钮——手填模型名正是最容易填错的地方
  let autoErr = '';
  if (mc.gateway_url && !mc.models.length) {
    pane.innerHTML = '<div class="card"><span class="spin"></span> 正在从网关拉取模型列表…</div>';
    try {
      await api('/api/model-config/refresh', { method: 'POST' });
      mc = await api('/api/model-config');
    } catch (e) { autoErr = e.message; }
  }

  const manual = !mc.models.length; // 拉不到就退回手填，不把人卡死
  const opts = mc.models.map((m) => `<option value="${esc(m)}" ${m === mc.default_model ? 'selected' : ''}>${esc(m)}</option>`).join('');
  pane.innerHTML = `<div class="card" style="max-width:720px">
    <h3>模型网关（内网 LiteLLM）</h3>
    <div class="field"><label>网关地址
        ${mc.gateway_source === '配置文件' ? '<span class="tag 风格">来自配置文件</span>' : ''}</label>
      <div class="row"><input class="inp" id="m-url" value="${esc(mc.gateway_url)}" placeholder="http://litellm.内网:4000" style="flex:1">
        <button class="btn" id="m-pull">从网关拉取</button></div>
      <span class="hint">根地址即可，/v1 有没有都行（两种路径都会试）。
        ${mc.gateway_source === '配置文件' ? '当前用的是配置文件里的值；在这里保存后会覆盖它。' : ''}</span></div>
    <div class="field"><label>网关密钥</label>
      <input class="inp" id="m-key" placeholder="${mc.has_gateway_key ? '已配置 ' + esc(mc.gateway_key_mask) + '，留空表示不修改' : '未配置'}">
      <span class="hint">加密存储；也可以在配置文件的 AIUDGE_GATEWAY_KEY 里填，界面只回显末四位。</span></div>
    <div class="grid2">
      <div class="field"><label>超时（秒）</label><input class="inp" id="m-timeout" type="number" value="${mc.timeout_sec}"></div>
      <div class="field"><label>默认模型</label>
        ${manual
          ? `<input class="inp" id="m-default" value="${esc(mc.default_model)}" placeholder="拉不到列表，手动填写">`
          : `<select class="inp sel" id="m-default">
               ${mc.default_model && !mc.models.includes(mc.default_model) ? `<option value="${esc(mc.default_model)}" selected>${esc(mc.default_model)}（不在列表中）</option>` : '<option value="">未选择</option>'}
               ${opts}</select>`}
        <span class="hint">按仓库单独指定的模型优先于这里的默认值。</span></div>
    </div>
    ${manual ? '' : `<div class="field"><label>可用模型（${mc.models.length} 个，来自网关）</label>
      <div class="chips">${mc.models.map((m) => `<span class="chip" style="cursor:default">${esc(m)}</span>`).join('')}</div></div>`}
    ${autoErr ? `<div class="field"><span class="hint" style="color:var(--advice)">自动拉取失败：${esc(autoErr)}</span></div>` : ''}
    <div class="row"><button class="btn primary" id="m-save">保存</button>
      <span class="muted" id="m-msg"></span></div>
  </div>`;

  $('#m-save', pane).onclick = async () => {
    const url = $('#m-url', pane).value.trim();
    const def = $('#m-default', pane).value.trim();
    const body = {
      gateway_url: url,
      timeout_sec: +$('#m-timeout', pane).value || 600,
      default_model: def,
      models: mc.models,
    };
    const k = $('#m-key', pane).value.trim();
    if (k) body.gateway_key = k;
    try {
      await api('/api/model-config', { method: 'PUT', body });
      toast('已保存');
      // 地址改过或默认模型是手填的，存完立刻核对一次，把错拦在跑任务之前
      if (!mc.models.length || (url !== mc.gateway_url)) {
        try {
          const r = await api('/api/model-config/refresh', { method: 'POST' });
          if (r.warn) toast(r.warn, true);
          else toast(`已从网关拉到 ${r.models.length} 个模型`);
        } catch (e) { toast('已保存，但拉取模型列表失败：' + e.message, true); }
      }
      viewAdminSettings(pane.parentElement);
    } catch (e) { toast(e.message, true); }
  };
  $('#m-pull', pane).onclick = async () => {
    // 把输入框里的地址直接交给拉取接口试，不先保存——拉取是只读的，
    // 试错了不该把已经存好的默认模型和列表改坏
    $('#m-msg', pane).textContent = '拉取中…';
    const key = $('#m-key', pane).value.trim();
    try {
      const r = await api('/api/model-config/refresh', {
        method: 'POST',
        body: { gateway_url: $('#m-url', pane).value.trim(), gateway_key: key || undefined },
      });
      toast(`拉到 ${r.models.length} 个模型`);
      if (r.warn) toast(r.warn, true);
      viewAdminSettings(pane.parentElement);
    } catch (e) { $('#m-msg', pane).textContent = e.message; }
  };
}

async function settingsUsers(pane) {
  const users = await api('/api/users');
  pane.innerHTML = `<div class="card" style="max-width:860px"><h3>用户与权限</h3></div>`;
  const box = document.createElement('div');
  box.append(table(['用户', '飞书 ID', '角色', '最近登录', ''], users, (u) => `
    <tr><td><b>${esc(u.name)}</b></td><td class="mono muted">${esc(u.feishu_user_id)}</td>
      <td>${u.role === 'admin' ? '<span class="tag confirmed">管理员</span>' : '<span class="tag 风格">普通用户</span>'}</td>
      <td class="mono muted">${fmtTime(u.last_login_at)}</td>
      <td class="actions"><button class="btn sm" data-role="${u.id}" data-cur="${u.role}">
        设为${u.role === 'admin' ? '普通用户' : '管理员'}</button></td></tr>`));
  pane.firstChild.append(box);
  box.onclick = async (e) => {
    const b = e.target.closest('[data-role]');
    if (b) {
      try {
        await api(`/api/users/${b.dataset.role}/role`, { method: 'PUT', body: { role: b.dataset.cur === 'admin' ? 'user' : 'admin' } });
        viewAdminSettings(pane.parentElement);
      } catch (err) { toast(err.message, true); }
      return;
    }
    const pw = e.target.closest('[data-pw]');
    if (pw) {
      if (!confirm('重置为默认密码？该账号下次登录会被要求改密。')) return;
      try {
        const r = await api(`/api/users/${pw.dataset.pw}/reset-password`, { method: 'POST' });
        toast(`已重置为 ${r.password}`);
      } catch (err) { toast(err.message, true); }
    }
  };
}

async function settingsIdentities(pane) {
  const [ids, users] = await Promise.all([api('/api/identities'), api('/api/users')]);
  pane.innerHTML = `<div class="card" style="max-width:960px">
    <h3>git 身份归并</h3>
    <p class="hint muted" style="margin-top:-6px">同一个人有多个 git 身份时在此合并；CI 机器人勾上「机器人」后不计入任何统计。</p>
    <div class="grid3">
      <div class="field"><label>git 身份（作者名）</label><input class="inp" id="i-name" placeholder="dengquanjie1"></div>
      <div class="field"><label>对应的人</label><select class="inp sel" id="i-user"><option value="0">不归属</option>
        ${users.map((u) => `<option value="${u.id}">${esc(u.name)}</option>`).join('')}</select></div>
      <div class="field"><label>机器人账号</label>
        <label class="row"><input type="checkbox" id="i-bot"> 不计入统计</label></div>
    </div>
    <button class="btn primary" id="i-add">添加映射</button></div>`;
  const box = document.createElement('div');
  box.style.marginTop = '14px';
  box.append(table(['git 身份', '归属人', '类型', ''], ids, (i) => `
    <tr><td class="mono">${esc(i.git_name)}</td><td>${esc(i.user_name || '—')}</td>
      <td>${i.is_bot ? '<span class="tag 风格">机器人</span>' : ''}</td>
      <td class="actions"><button class="btn sm danger" data-del="${i.id}">删除</button></td></tr>`));
  pane.append(box);
  $('#i-add', pane).onclick = async () => {
    const git_name = $('#i-name', pane).value.trim();
    if (!git_name) return toast('填 git 身份', true);
    try {
      await api('/api/identities', {
        method: 'POST',
        body: { git_name, user_id: +$('#i-user', pane).value, is_bot: $('#i-bot', pane).checked },
      });
      viewAdminSettings(pane.parentElement);
    } catch (e) { toast(e.message, true); }
  };
  box.onclick = async (e) => {
    const d = e.target.closest('[data-del]');
    if (!d) return;
    try { await api('/api/identities/' + d.dataset.del, { method: 'DELETE' }); viewAdminSettings(pane.parentElement); }
    catch (err) { toast(err.message, true); }
  };
}

async function settingsAudit(pane) {
  const d = await api('/api/audit?page=1&limit=100');
  pane.append(table(['时间', '操作人', '动作', '对象', '详情'], d.items, (a) => `
    <tr><td class="mono muted">${esc(a.created_at)}</td><td>${esc(a.user_name)}</td>
      <td class="mono">${esc(a.action)}</td><td class="mono muted">${esc(a.target_type)} ${esc(a.target_id)}</td>
      <td class="muted">${esc((a.detail || '').slice(0, 120))}</td></tr>`));
}

/* ---------- 通知 ---------- */

async function settingsNotify(pane) {
  const c = await api('/api/notify-config');
  pane.innerHTML = `<div class="card" style="max-width:780px">
    <h3>飞书通知</h3>
    <p class="hint muted" style="margin-top:-6px">
      这里配的是<b>全局兜底</b>。每个仓库可以在仓库配置里单独指定自己的群，优先级更高。
      留空则回落到配置文件里的 <code>AIUDGE_FEISHU_WEBHOOK</code>。</p>
    ${kv([
      ['当前生效来源', `<b>${esc(c.source)}</b>`],
      ['已单独配群的仓库', `${c.repo_overrides} 个`],
    ])}
    <div class="field" style="margin-top:14px"><label>全局机器人 Webhook</label>
      <input class="inp" id="n-hook" placeholder="${c.has_webhook ? '已配置 ' + esc(c.webhook_mask) + '，留空表示不修改' : '不填则用配置文件里的'}">
      <span class="hint">URL 里带着 token（拿到就能往群里发消息），加密存储、界面只回显形态、日志不打印。</span></div>
    ${c.file_configured && !c.has_webhook ? `<div class="field"><span class="hint muted">
      配置文件里已经配了一个（${esc(c.file_mask)}），不填就继续用它。</span></div>` : ''}
    <div class="field"><label>每日汇总推送时间（cron）</label>
      <input class="inp" id="n-cron" value="${esc(c.summary_cron)}" placeholder="留空则用配置文件里的，都没有就默认 30 18 * * *">
      <span class="hint">当前生效：<code>${esc(c.effective_cron || '未启用')}</code>。
        留空会退回配置文件里的 AIUDGE_SUMMARY_CRON。日报按仓库分流，各自发到各自的群。</span></div>
    <div class="row">
      <button class="btn primary" id="n-save">保存</button>
      <button class="btn" id="n-test">发送测试消息</button>
      ${c.has_webhook ? '<button class="btn danger" id="n-clear">清空全局 webhook</button>' : ''}
      <span class="muted" id="n-msg"></span>
    </div>
    <div id="n-result"></div>
  </div>`;

  const showResult = (ok, text) => {
    $('#n-result', pane).innerHTML = `<div class="field" style="margin-top:12px">
      <span class="${ok ? 'hint' : 'hint'}" style="color:${ok ? 'var(--ok)' : 'var(--high)'}">${esc(text)}</span></div>`;
  };

  $('#n-save', pane).onclick = async () => {
    const hook = $('#n-hook', pane).value.trim();
    const body = { summary_cron: $('#n-cron', pane).value.trim() };
    if (hook) body.webhook = hook; // 留空 = 不修改，清空走单独的按钮
    try {
      await api('/api/notify-config', { method: 'PUT', body });
      toast('已保存');
      viewAdminSettings(pane.parentElement);
    } catch (e) { toast(e.message, true); }
  };

  $('#n-test', pane).onclick = async () => {
    const typed = $('#n-hook', pane).value.trim();
    $('#n-msg', pane).textContent = '发送中…';
    try {
      // 输入框里有内容就先试那个，省得「先保存再发现有错」
      const r = await api('/api/notify-config/test', { method: 'POST', body: { webhook: typed } });
      $('#n-msg', pane).textContent = '';
      showResult(true, `已发送到 ${r.sent_to}，去群里看看`);
    } catch (e) {
      $('#n-msg', pane).textContent = '';
      showResult(false, e.message);
    }
  };

  const clr = $('#n-clear', pane);
  if (clr) clr.onclick = async () => {
    if (!confirm('清空全局 webhook？之后未单独配群的仓库将回落到配置文件里的。')) return;
    try { await api('/api/notify-config/clear', { method: 'POST' }); toast('已清空'); viewAdminSettings(pane.parentElement); }
    catch (e) { toast(e.message, true); }
  };
}

/* ---------- 审核 Prompt ---------- */

async function settingsPrompt(pane) {
  const p = await api('/api/prompt-config');
  // 自定义过就编辑自定义的；没自定义过就编辑内置默认的内容——
  // 让人在默认基础上改，比对着空白框从头写现实得多
  const editing = p.customized ? p.custom : p.default;
  pane.innerHTML = `<div class="card" style="max-width:900px">
    <h3>通用审核 Prompt
      <span class="tag ${p.customized ? 'confirmed' : '风格'}" style="margin-left:8px">
        ${p.customized ? '已自定义' : '内置默认'}</span>
      <span class="mono muted" style="font-size:12px;font-weight:400;margin-left:8px">版本 ${esc(p.version)}</span>
    </h3>
    <p class="hint muted" style="margin-top:-6px">
      这段是发给模型的系统提示骨架。<b>领域细节不要写在这里</b>——那是每个仓库各自挂载的规范文档的活，
      写在这儿会强加给所有仓库。<br>
      可用占位符：${p.placeholders.map((x) => `<code>${esc(x.token)}</code>（${esc(x.desc)}）`).join('　')}<br>
      版本号是内容哈希，自动跟着内容走，不用手填——手填必然会有「改了忘改版本」的一天，
      历史曲线就再也解释不清了。改动会在趋势图上留下断点。
    </p>
    <div class="field"><label>Prompt 模板</label>
      <textarea class="inp" id="p-text" style="min-height:420px;font-family:var(--mono);font-size:12.5px">${esc(editing)}</textarea></div>
    <div class="row">
      <button class="btn primary" id="p-save">保存</button>
      <button class="btn" id="p-reset">恢复内置默认</button>
      <button class="btn ghost" id="p-view">预览实际发出的内容</button>
      <span class="muted" id="p-msg"></span>
    </div>
    <div id="p-preview"></div>
  </div>`;

  $('#p-save', pane).onclick = async () => {
    const text = $('#p-text', pane).value;
    if (!text.trim()) return toast('内容不能为空，要恢复默认请点「恢复内置默认」', true);
    try {
      const r = await api('/api/prompt-config', { method: 'PUT', body: { system_prompt: text } });
      toast(`已保存，新版本 ${r.version}`);
      viewAdminSettings(pane.parentElement);
    } catch (e) { toast(e.message, true); }
  };
  $('#p-reset', pane).onclick = async () => {
    if (!confirm('恢复成内置默认 prompt？自定义内容会丢掉。')) return;
    try {
      await api('/api/prompt-config', { method: 'PUT', body: { system_prompt: '' } });
      toast('已恢复内置默认');
      viewAdminSettings(pane.parentElement);
    } catch (e) { toast(e.message, true); }
  };
  $('#p-view', pane).onclick = () => {
    const box = $('#p-preview', pane);
    if (box.innerHTML) { box.innerHTML = ''; return; }
    // 前端做替换只是为了预览；真正生效的是后端拿仓库的语言和名字渲染
    const rendered = $('#p-text', pane).value
      .split('{{language}}').join('Go').split('{{repo}}').join('slg_server');
    box.innerHTML = `<div class="field"><label>实际发给模型的系统提示（加上各仓库挂载的规范文档）</label>
      ${codeBlock(rendered)}</div>`;
  };
}

/* ---------- 维护（测试阶段的清档） ---------- */

const TABLE_NAMES = {
  issues: '问题', review_tasks: '任务', task_logs: '任务日志', review_reports: '报告',
  report_issues: '报告-问题关联', comments: '评论', issue_events: '状态流转记录',
  review_marks: '已审核 commit 标记', audit_logs: '审计日志',
};

async function settingsMaint(pane) {
  const p = await api('/api/admin/reset-preview');
  const rows = Object.entries(p.counts).sort((a, b) => b[1] - a[1]);
  pane.innerHTML = `<div class="card" style="max-width:760px">
    <h3>清空业务数据</h3>
    <p class="hint muted" style="margin-top:-6px">
      测试阶段用。只清审核跑出来的数据，<b>仓库配置、模型网关、用户、身份归并、子系统规则都保留</b>，
      不用重配。清空后任务编号从 #1 重新开始。</p>
    <div class="tbl-wrap" style="margin:12px 0">
      <table><thead><tr><th>将清空</th><th class="num">条数</th></tr></thead><tbody>
      ${rows.length ? rows.map(([k, v]) => `<tr><td>${esc(TABLE_NAMES[k] || k)}</td><td class="num">${v}</td></tr>`).join('')
        : '<tr><td colspan="2" class="muted">当前没有业务数据</td></tr>'}
      </tbody></table>
    </div>
    <div class="field"><label class="row"><input type="checkbox" id="m-wt"> 同时删除本地仓库副本</label>
      <span class="hint">勾上会删掉 data/repos 下的克隆，下次审核重新拉。仓库大的话（几百 MB）会慢几分钟，
        但能顺带验证一遍「从零克隆」的路径。</span></div>
    <div class="field"><label>输入「清空」确认</label><input class="inp" id="m-confirm" placeholder="清空"></div>
    <div class="row"><button class="btn danger" id="m-reset">清空业务数据</button>
      <span class="muted" id="m-msg"></span></div>
  </div>`;

  $('#m-reset', pane).onclick = async () => {
    const confirmText = $('#m-confirm', pane).value.trim();
    if (confirmText !== '清空') return toast('请在输入框里打「清空」两个字', true);
    if (p.total > 0 && !window.confirm(`将删除 ${p.total} 行业务数据，此操作不可撤销。继续？`)) return;
    $('#m-msg', pane).textContent = '正在等待执行中的任务收尾…';
    try {
      const r = await api('/api/admin/reset', {
        method: 'POST',
        body: { confirm: '清空', worktrees: $('#m-wt', pane).checked },
      });
      const total = Object.values(r.counts).reduce((a, b) => a + b, 0);
      let msg = `已清空 ${total} 行`;
      if (r.worktrees_removed) msg += `，删除本地仓库副本 ${r.worktrees_removed} 个`;
      toast(msg);
      viewAdminSettings(pane.parentElement);
    } catch (e) {
      $('#m-msg', pane).textContent = '';
      toast(e.message, true);
    }
  };
}

/* ---------- 登录 ---------- */

async function renderLogin(err) {
  $('#boot').hidden = true;
  $('#app').hidden = true;
  const view = $('#view');
  $('#app').hidden = false;
  $('#page-title').textContent = '登录';
  let cfg = { feishu_enabled: false, dev_login: true };
  try { cfg = await api('/api/auth/config'); } catch { /* 配置接口都挂了就只显示提示 */ }
  view.innerHTML = '';
  const card = document.createElement('div');
  card.className = 'card glow';
  card.style.cssText = 'max-width:400px;margin:12vh auto 0';
  card.innerHTML = `
    <h3 style="font-size:16px;color:var(--fg)">AIudge 代码审核台</h3>
    ${err ? `<p style="color:var(--high)">${esc(err)}</p>` : ''}
    ${cfg.feishu_enabled
      ? `<p class="muted">使用飞书扫码登录。系统以飞书身份作为唯一标识，用于问题指派与通知。</p>
         <a class="btn primary" href="/api/auth/login">飞书扫码登录</a>`
      : `<p class="muted">飞书登录未配置（缺少 AIUDGE_FEISHU_APP_ID / SECRET），当前为开发模式。</p>
         <div class="field"><label>你的名字</label><input class="inp" id="dev-name" placeholder="邓全杰"></div>
         <button class="btn primary" id="dev-go">进入</button>
         <p class="hint muted">开发模式下首位登录者自动成为管理员。生产部署请配置飞书登录。</p>`}
    <hr style="border:0;border-top:1px solid var(--line);margin:18px 0">
    <div class="field"><label>管理员账号登录</label>
      <input class="inp" id="pw-user" placeholder="admin" autocomplete="username" style="margin-bottom:8px">
      <input class="inp" id="pw-pass" type="password" placeholder="密码" autocomplete="current-password"></div>
    <button class="btn" id="pw-go">登录</button>
    <span class="muted" id="pw-msg" style="margin-left:10px"></span>
    <p class="hint muted">账号密码只给管理员兜底——飞书没配或临时挂了时还能进后台。
      普通用户走飞书，@ 通知依赖飞书的身份映射。</p>`;
  view.append(card);
  const pwGo = $('#pw-go', view);
  const pwSubmit = async () => {
    const msg = $('#pw-msg', view);
    msg.textContent = '';
    try {
      await api('/api/auth/password', {
        method: 'POST',
        body: { username: $('#pw-user', view).value.trim(), password: $('#pw-pass', view).value },
      });
      location.reload();
    } catch (e) { msg.textContent = e.message; msg.style.color = 'var(--high)'; }
  };
  pwGo.onclick = pwSubmit;
  $('#pw-pass', view).onkeydown = (e) => { if (e.key === 'Enter') pwSubmit(); };

  const go = $('#dev-go');
  if (go) go.onclick = async () => {
    const name = $('#dev-name').value.trim();
    if (!name) return toast('填个名字', true);
    try {
      await api('/api/auth/dev-login', { method: 'POST', body: { name } });
      location.hash = '#/home';
      location.reload();
    } catch (e) { toast(e.message, true); }
  };
}

/* ---------- 修改密码 ----------
   就是个普通功能，不再是进站必过的关卡。默认口令是写在代码里的，该换，
   但换不换由人自己决定——把人拦在功能之外只会逼出「随便改一个应付过去」。 */

function passwordForm() {
  const body = document.createElement('div');
  body.className = 'stack';
  body.innerHTML = `
    <p class="hint muted" style="margin:0">内置管理员用的是写在代码里的默认口令，
      而这个账号权限不小，建议换掉。不换也能正常用其他功能。</p>
    <div class="field"><label>当前密码</label><input class="inp" id="cp-old" type="password" autocomplete="current-password"></div>
    <div class="field"><label>新密码（至少 8 位）</label><input class="inp" id="cp-new" type="password" autocomplete="new-password"></div>
    <div class="field"><label>再输一次</label><input class="inp" id="cp-new2" type="password" autocomplete="new-password"></div>
    <div class="row"><button class="btn primary" id="cp-go">保存</button>
      <span class="muted" id="cp-msg"></span></div>`;
  openDrawer('修改密码', body);

  const submit = async () => {
    const msg = $('#cp-msg', body);
    const n1 = $('#cp-new', body).value, n2 = $('#cp-new2', body).value;
    msg.style.color = '';
    if (n1 !== n2) { msg.style.color = 'var(--high)'; msg.textContent = '两次输入不一致'; return; }
    msg.textContent = '提交中…';
    try {
      await api('/api/auth/change-password', {
        method: 'POST',
        body: { old_password: $('#cp-old', body).value, new_password: n1 },
      });
      closeDrawer();
      toast('密码已修改');
    } catch (e) { msg.style.color = 'var(--high)'; msg.textContent = e.message; }
  };
  $('#cp-go', body).onclick = submit;
  $('#cp-new2', body).onkeydown = (e) => { if (e.key === 'Enter') submit(); };
}

/* ---------- 项目设置 ----------
   当前项目由路由决定（#/p/<id>/...），这里不再自己做一排项目页签——
   那是「工作台」和左侧切换按钮的活。这里只渲染这一个项目的仓库与成员。
   函数名保持 viewProjects：webui/smoke.mjs 的用例直接调它，守的是
   「不同角色不该去碰无权访问的接口」那个连出两次的真实事故。 */

async function viewProjects(el) {
  const [projects, me] = await Promise.all([api('/api/projects'), api('/api/me')]);
  el.innerHTML = '';
  if (!projects.length) {
    el.append(emptyBlock('你还没有加入任何项目', '去「工作台」新建一个，你会自动成为它的项目管理员'));
    return;
  }
  // 只认 state.projectId 指的那个。以前找不到就回落到 projects[0]，
  // 结果是 URL 写着 A、页面显示的全是 B 的仓库和成员——「明明有仓库却显示没有」
  // 就是这么来的。宁可说清楚，也不要默默换成另一个项目。
  const cur = projects.find((p) => p.id === state.projectId);
  if (!cur) {
    el.append(emptyBlock('项目不存在或你无权访问', '它可能已经被删除，去「工作台」重新选一个'));
    return;
  }
  await renderProjectDetail(el, cur, me);
}

async function renderProjectDetail(el, p, me) {
  if (!p) return;
  // 「还能加谁进来」走项目自己的候选人接口，不是全局用户表。
  // 那个接口只要项目管理员权限——项目管理员是管这个项目的人，不是全局管理员
  const [repos, members, addable, perms] = await Promise.all([
    api('/api/repos'),
    api(`/api/projects/${p.id}/members`),
    p.can_admin ? api(`/api/projects/${p.id}/candidates`) : Promise.resolve([]),
    api('/api/permissions').catch(() => []), // 权限清单只是用来渲染勾选项，拿不到就不渲染
  ]);
  const mine = repos.filter((r) => r.project_id === p.id);

  el.innerHTML = '';
  const head = document.createElement('div');
  head.className = 'toolbar';
  head.innerHTML = `<h3 style="margin:0;font-size:15px">${esc(p.name)}</h3>
    <span class="muted" style="font-size:12.5px">${esc(p.description || '')}</span>
    <span class="spacer"></span>
    ${p.can_admin ? '<button class="btn sm" id="ep">编辑项目</button>' : ''}
    ${p.can_admin && mine.length === 0 ? '<button class="btn sm danger" id="dp">删除项目</button>' : ''}
    ${p.can_admin ? '<button class="btn primary sm" id="nr">+ 新增仓库</button>' : ''}`;
  el.append(head);

  const repoBox = document.createElement('div');
  repoBox.append(table(['仓库', '分支', '语言', '触发', '通知群', '状态', ''],
    mine.map((r) => ({ ...r, _canAdmin: r.can_admin })), (r) => `
    <tr data-repo="${r.id}">
      <td><b>${esc(r.name)}</b><br><span class="path" style="font-size:11px">${esc(r.git_url)}</span></td>
      <td class="mono">${esc(r.branch || '—')}</td>
      <td><span class="tag 风格">${esc(langLabel(r.language))}</span></td>
      <td class="muted">${r.trigger_webhook ? 'webhook' : ''}${r.trigger_webhook && r.trigger_cron ? ' + ' : ''}${esc(r.trigger_cron || '')}</td>
      <td class="muted" style="font-size:11px">${esc(r.webhook_mask || '全局')}</td>
      <td>${r.enabled ? '<span class="tag fixed">启用</span>' : '<span class="tag 风格">停用</span>'}</td>
      <td class="actions">
        ${r.can_trigger ? `<button class="btn sm" data-trigger="${r.id}">手动触发</button>` : ''}
        ${r.can_admin ? `<button class="btn sm" data-rules="${r.id}">子系统规则</button>
          <button class="btn sm" data-edit="${r.id}">编辑</button>` : ''}
      </td>
    </tr>`, '这个项目下还没有仓库'));
  el.append(repoBox);

  const memHead = document.createElement('h3');
  memHead.style.cssText = 'font-size:15px;margin:22px 0 10px';
  memHead.textContent = '项目成员';
  el.append(memHead);

  // 权限勾选项按服务端给的清单通用渲染：以后加权限类型只改后端，这张表不用动。
  const permList = perms || [];
  const memBox = document.createElement('div');
  memBox.append(table(['成员', '项目角色', '额外权限', ''], members, (m) => `
    <tr data-user="${m.user_id}">
      <td><b>${esc(m.name || '#' + m.user_id)}</b></td>
      <td><span class="tag ${m.role === 'project_admin' ? 'confirmed' : '风格'}">${esc(m.role_name)}</span></td>
      <td>${permList.length ? permList.map((pm) => `<label class="row${p.can_admin ? '' : ' off'}" title="${p.can_admin ? '授予后该成员可以手动触发本项目的审核任务' : '只有项目管理员能改权限'}">
          <input type="checkbox" data-perm="${esc(pm.key)}"
            ${(m.perms || []).includes(pm.key) ? 'checked' : ''} ${p.can_admin ? '' : 'disabled'}> ${esc(pm.name)}</label>`).join('')
        : '<span class="muted">—</span>'}</td>
      <td class="actions">${p.can_admin ? `<button class="btn sm" data-role="${m.user_id}" data-cur="${m.role}">
        设为${m.role === 'project_admin' ? '成员' : '项目管理员'}</button>
        <button class="btn sm danger" data-drop="${m.user_id}">移出</button>` : ''}</td>
    </tr>`, '还没有成员，谁都看不到这个项目的数据'));
  el.append(memBox);

  if (p.can_admin && addable.length) {
    const add = document.createElement('div');
    add.className = 'row';
    add.style.marginTop = '12px';
    add.innerHTML = `<select class="inp sel" id="mu">${addable.map((u) => `<option value="${u.id}">${esc(u.name)}</option>`).join('')}</select>
      <select class="inp sel" id="mr">
        <option value="member">成员</option><option value="project_admin">项目管理员</option></select>
      <button class="btn primary" id="ma">加入项目</button>`;
    el.append(add);
    $('#ma', add).onclick = async () => {
      try {
        await api(`/api/projects/${p.id}/members`, {
          method: 'POST',
          body: { user_id: +$('#mu', add).value, role: $('#mr', add).value },
        });
        toast('已加入');
        viewProjects(el);
      } catch (e) { toast(e.message, true); }
    };
  }

  head.onclick = async (e) => {
    if (e.target.id === 'nr') repoForm(null, p.id);
    if (e.target.id === 'ep') projectForm(p);
    if (e.target.id === 'dp') {
      if (!confirm(`删除项目「${p.name}」？成员关系会一并删掉。`)) return;
      try { await api(`/api/projects/${p.id}`, { method: 'DELETE' }); toast('已删除'); viewProjects(el); }
      catch (err) { toast(err.message, true); }
    }
  };
  repoBox.onclick = (e) => {
    const id = e.target.closest('[data-repo]')?.dataset.repo;
    if (!id) return;
    const repo = mine.find((r) => r.id === +id);
    if (e.target.closest('[data-edit]')) repoForm(repo, p.id);
    if (e.target.closest('[data-rules]')) subsystemRules(repo);
    if (e.target.closest('[data-trigger]')) triggerForm(repo);
  };
  memBox.onclick = async (e) => {
    const rb = e.target.closest('[data-role]');
    if (rb) {
      try {
        await api(`/api/projects/${p.id}/members`, {
          method: 'POST',
          body: { user_id: +rb.dataset.role, role: rb.dataset.cur === 'project_admin' ? 'member' : 'project_admin' },
        });
        viewProjects(el);
      } catch (err) { toast(err.message, true); }
      return;
    }
    const db = e.target.closest('[data-drop]');
    if (db) {
      if (!confirm('移出该成员？他将看不到这个项目的任何数据。')) return;
      try {
        await api(`/api/projects/${p.id}/members?user_id=${db.dataset.drop}`, { method: 'DELETE' });
        viewProjects(el);
      } catch (err) { toast(err.message, true); }
    }
  };
  // 权限勾选挂 change 而不是复用上面的 click：点 label 上的文字时，浏览器会把点击
  // 转发给里面的 input，click 会到两次（label 一次、input 一次），那样会发两遍请求。
  memBox.addEventListener('change', async (e) => {
    const cb = e.target.closest('[data-perm]');
    if (!cb) return;
    const row = cb.closest('[data-user]');
    const uid = +row.dataset.user;
    const m = members.find((x) => x.user_id === uid) || {};
    // 整个行上的勾选状态一起提交：接口是覆盖式的，只发被点的那一个会把别的权限抹掉
    const keys = [...row.querySelectorAll('[data-perm]')].filter((n) => n.checked).map((n) => n.dataset.perm);
    try {
      await api(`/api/projects/${p.id}/members`, {
        method: 'POST', body: { user_id: uid, role: m.role, permissions: keys },
      });
      toast(keys.length ? '已授权' : '已取消授权');
      viewProjects(el);
    } catch (err) { toast(err.message, true); viewProjects(el); }
  });
}

function projectForm(p) {
  const body = document.createElement('div');
  body.className = 'stack';
  body.innerHTML = `
    <div class="field"><label>项目名称 *</label><input class="inp" id="pj-name" value="${esc(p?.name || '')}"></div>
    <div class="field"><label>说明</label><input class="inp" id="pj-desc" value="${esc(p?.description || '')}"></div>
    ${p ? `<div class="field"><label class="row"><input type="checkbox" id="pj-on" ${p.enabled ? 'checked' : ''}> 启用</label></div>` : ''}
    <div class="row"><button class="btn primary" id="pj-save">保存</button></div>`;
  openDrawer(p ? `编辑项目 ${p.name}` : '新建项目', body);
  $('#pj-save', body).onclick = async () => {
    const name = $('#pj-name', body).value.trim();
    if (!name) return toast('项目名称必填', true);
    const payload = { name, description: $('#pj-desc', body).value.trim() };
    if (p) payload.enabled = $('#pj-on', body).checked;
    try {
      let newId = 0;
      if (p) await api(`/api/projects/${p.id}`, { method: 'PUT', body: payload });
      else newId = (await api('/api/projects', { method: 'POST', body: payload })).id;
      toast('已保存');
      closeDrawer();
      await loadProjects();
      // 新建完直接进这个项目——建完还停在原地，人会以为没成功
      if (newId) go(`#/p/${newId}/dashboard`); else route();
    } catch (e) { toast(e.message, true); }
  };
}

/* ---------- 启动 ---------- */

function topbarRange() {
  const from = $('#date-from').value, to = $('#date-to').value;
  state.from = from; state.to = to;
}
$('#date-from').onchange = $('#date-to').onchange = () => { topbarRange(); route(); };
$('#refresh').onclick = () => { state.selected.clear(); route(); };
$('#repo-filter').onchange = () => { state.repoId = +$('#repo-filter').value; route(); };

// fillRepos 只列当前项目下的仓库——项目页的数据范围就是这个下拉定的，
// 把别的项目的仓库放进来就等于把数据范围放出去了。
// 第一项是「本项目全部仓库」（值为 0），后端在这个项目内聚合。
// projectRepos 当前项目下的仓库。页面上的仓库筛选都从这儿取，
// 数据范围就是这个列表定的——把别的项目的仓库放进来等于把数据范围放出去了。
const projectRepos = () => (state.repos || []).filter((r) => r.project_id === state.projectId);

async function fillRepos() {
  state.repos = await api('/api/repos');
  const sel = $('#repo-filter');
  const mine = projectRepos();
  sel.innerHTML = '<option value="0">本项目全部仓库</option>' +
    mine.map((r) => `<option value="${r.id}">${esc(r.name)}</option>`).join('');
  // 上个项目选中的仓库在新项目里不存在了，退回「全部」
  if (!mine.some((r) => r.id === state.repoId)) state.repoId = 0;
  sel.value = String(state.repoId);
}

async function boot() {
  let me;
  try { me = await api('/api/me'); } catch { return; }
  state.user = me;
  $('#boot').hidden = true;
  $('#app').hidden = false;
  $('#who').textContent = `${me.name}${me.role === 'admin' ? ' · 全局管理员' : ''}`;
  // 「修改密码」只给有密码的账号看（内置管理员这类 username 非空的）。
  // 飞书/开发登录的用户没有密码可改，给了按钮点进去只会吃一句「原密码不对」。
  const pop = $('#userpop-body');
  pop.innerHTML = `<div class="stack"><div><b>${esc(me.name)}</b><br>
    <span class="muted mono" style="font-size:11.5px">${esc(me.feishu_user_id || me.username)}</span></div>
    ${me.username ? '<button class="btn" id="chpw">修改密码</button>' : ''}
    <button class="btn" id="logout">退出登录</button></div>`;
  const chpw = pop.querySelector('#chpw');
  if (chpw) chpw.onclick = () => { $('#userpop').hidePopover(); passwordForm(); };
  pop.querySelector('#logout').onclick = async () => {
    await api('/api/auth/logout', { method: 'POST' });
    location.reload();
  };
  $('#home-btn').onclick = () => go('#/home');

  topbarRange();
  await loadProjects();
  await route();
}

window.addEventListener('hashchange', route);
document.addEventListener('DOMContentLoaded', boot);
