// 前端冒烟检查：用最小 DOM 桩加载真实的 app.js，跑几条容易坏、又不容易在浏览器里
// 一眼看出来的路径。
//
// 存在的原因：登录页那个「密码错了看起来没反应」的 bug，是 api() 里的 401 拦截器
// 把整个页面重绘了、错误信息写进了已经被丢弃的节点。这种问题在浏览器里表现为
// 「点了没动静」，从后端日志上什么都看不出来——只能在前端这一侧测。
//
// 运行：node webui/smoke.mjs    （go test ./internal/web 也会调它，node 不在就跳过）
import fs from 'fs';
import path from 'path';
import vm from 'vm';
import { fileURLToPath } from 'url';

const here = path.dirname(fileURLToPath(import.meta.url));

// ---------- 最小 DOM 桩 ----------
function mkEl(tag = 'div') {
  const e = {
    tagName: tag, children: [], _html: '', textContent: '', style: {}, dataset: {},
    className: '', hidden: false, value: '', onclick: null, onkeydown: null,
    _reg: {},
    // 真实的 classList 用一个局部 Set 顶着就够了：冒烟检查不需要浏览器那样的语义
    classList: (() => {
      const s = new Set();
      return {
        toggle: (c, on) => (on ? s.add(c) : s.delete(c)),
        add: (c) => s.add(c), remove: (c) => s.delete(c), contains: (c) => s.has(c),
      };
    })(),
    get innerHTML() { return this._html; },
    set innerHTML(v) { this._html = v; this.children = []; this.renderCount = (this.renderCount || 0) + 1; },
    // 真实 DOM 里 append 会建立父子关系，桩也要设上：重渲染的代码依赖
    // pane.parentElement 往回找容器，不设就会在「保存后刷新」那条路径上炸掉
    append(...n) { for (const c of n) { if (c && typeof c === 'object') c.parentElement = this; this.children.push(c); } },
    appendChild(n) { if (n && typeof n === 'object') n.parentElement = this; this.children.push(n); return n; },
    querySelectorAll: () => [],
    addEventListener() {}, remove() {}, removeAttribute() {}, focus() {},
    get firstElementChild() { return this.children[0] || null; },
    // 只按 id 找；找不到就造一个并缓存，保证同一个选择器总是拿到同一个节点
    querySelector(sel) {
      const id = sel.replace(/^#/, '');
      return (this._reg[id] = this._reg[id] || mkEl());
    },
  };
  return e;
}

function makeCtx(routes) {
  const view = mkEl();
  view._id = 'view';
  const byID = {};
  const document = {
    querySelector: (s) => (s === '#view' ? view : (byID[s] = byID[s] || mkEl())),
    querySelectorAll: () => [],
    createElement: mkEl,
    addEventListener: () => {},
  };
  const calls = [];
  const fetchStub = async (url, opt) => {
    calls.push({ url, method: (opt && opt.method) || 'GET' });
    const r = routes(url, opt) || { status: 404, body: {} };
    return { ok: r.status < 400, status: r.status, text: async () => JSON.stringify(r.body) };
  };
  const ctx = vm.createContext({
    document, fetch: fetchStub, console, setTimeout, clearTimeout,
    confirm: () => true, alert: () => {},
    location: { reload() {}, hash: '', origin: 'http://test' },
    window: { addEventListener() {}, location: { reload() {}, hash: '' } },
  });
  ctx.globalThis = ctx;
  vm.runInContext(fs.readFileSync(path.join(here, 'app.js'), 'utf8'), ctx, { filename: 'app.js' });
  return { ctx, view, calls };
}

// textOf 递归收集节点树里的文本。视图容器常常是 append 子节点而不是写 innerHTML，
// 只看 _html 会漏掉真正渲染出来的内容——断言就会假阴性。
function textOf(n, acc = []) {
  if (!n || typeof n !== 'object') return acc;
  if (n._html) acc.push(n._html);
  if (n.textContent) acc.push(String(n.textContent));
  (n.children || []).forEach((c) => textOf(c, acc));
  return acc.join(' ');
}

// ---------- 断言 ----------
let failed = 0;
function check(name, got, want) {
  if (got === want) {
    console.log(`  ✓ ${name}`);
  } else {
    console.log(`  ✗ ${name}\n      实际: ${JSON.stringify(got)}\n      期望: ${JSON.stringify(want)}`);
    failed++;
  }
}

// ---------- 用例 1：密码错误要看得见，且不能把登录页重绘掉 ----------
// 拦截器把 401 一律当成「会话过期」时，页面会被重绘，错误信息写进已丢弃的节点，
// 用户看到的就是「点了没反应」。
async function testLoginErrorVisible() {
  const { ctx, view } = makeCtx((url) => {
    if (url.includes('/api/auth/config')) return { status: 200, body: { feishu_enabled: true, dev_login: false } };
    if (url.includes('/api/auth/password')) return { status: 401, body: { error: '账号或密码错误' } };
    return { status: 404, body: {} };
  });

  await ctx.renderLogin();
  const rendersBefore = view.renderCount;

  view.querySelector('#pw-user').value = 'admin';
  view.querySelector('#pw-pass').value = 'wrong';
  await view.querySelector('#pw-go').onclick();

  check('密码错误时显示后端的错误文案',
    view.querySelector('#pw-msg').textContent, '账号或密码错误');
  check('密码错误时不该重绘登录页（重绘会把错误信息冲掉）',
    view.renderCount, rendersBefore);
}

// ---------- 用例 2：会话过期的 401 仍要重绘登录页 ----------
// 上一条修的是「登录接口的 401」，不能把「会话过期」那一条也一起放过。
async function testSessionExpiredStillRedirects() {
  const { ctx, view } = makeCtx((url) => {
    if (url.includes('/api/auth/config')) return { status: 200, body: { feishu_enabled: true, dev_login: false } };
    return { status: 401, body: { error: '未登录' } };
  });

  await ctx.renderLogin();
  const rendersBefore = view.renderCount;
  let threw = false;
  try {
    await ctx.api('/api/issues'); // 任意业务接口，模拟会话过期
  } catch { threw = true; }
  // renderLogin 是异步的（它要先拉一次 auth/config），拦截器不 await 它。
  // 等一个 tick 再看它有没有真的渲染
  await new Promise((r) => setTimeout(r, 10));

  check('业务接口 401 应抛出（调用方需知道没拿到数据）', threw, true);
  check('业务接口 401 应重绘登录页', view.renderCount > rendersBefore, true);
}

// ---------- 用例 3：飞书已配置时，账号密码入口仍要在 ----------
// 管理员兜底入口被条件分支吃掉过一次，表现是「配了飞书之后 admin 登不进来」。
async function testPasswordFormAlwaysRendered() {
  const { ctx, view } = makeCtx((url) => {
    if (url.includes('/api/auth/config')) return { status: 200, body: { feishu_enabled: true, dev_login: false } };
    return { status: 404, body: {} };
  });
  await ctx.renderLogin();
  const html = view.children[0]._html;
  check('配了飞书时仍渲染账号密码表单',
    html.includes('id="pw-user"') && html.includes('id="pw-go"'), true);
  check('配了飞书时渲染扫码入口', html.includes('/api/auth/login'), true);
}

// ---------- 用例 4：三种角色打开项目页，各自不该碰无权访问的接口 ----------
// 真实事故连着出了两次，都是「页面里混进一个当前用户无权调的接口，整页挂掉」：
//   1) 普通成员 → /api/users（全局管理员专属）
//   2) 改成 can_admin 才调之后，项目管理员也被带进来 → 还是 /api/users
// 根因是把「能管这个项目」和「是全局管理员」当成了同一件事。
const ADMIN_ONLY = ['/api/users', '/api/identities', '/api/model-config',
  '/api/notify-config', '/api/prompt-config', '/api/audit'];

async function projectPageAs(role, canAdmin) {
  const { ctx, calls } = makeCtx((url) => {
    if (url.includes('/candidates')) return { status: 200, body: [{ id: 9, name: '候选人' }] };
    if (url.includes('/members')) return { status: 200, body: [{ user_id: 2, name: '张三', role, role_name: 'x' }] };
    if (url.includes('/api/projects')) {
      return { status: 200, body: [{ id: 1, name: 'P', repo_count: 0, member_count: 1,
        role, role_name: 'x', can_admin: canAdmin }] };
    }
    if (url.includes('/api/repos')) return { status: 200, body: [] };
    if (url.includes('/api/me')) return { status: 200, body: { id: 2, name: '张三', role: 'user' } };
    if (ADMIN_ONLY.some((p) => url.startsWith(p))) {
      return { status: 403, body: { error: '需要全局管理员权限' } };
    }
    return { status: 404, body: {} };
  });
  // viewProjects 现在只认 state.projectId 指定的项目（不再静默回落到 projects[0]），
  // 这正是 route() 在真实流程里做的事，桩也要照做
  vm.runInContext('state.projectId = 1', ctx);
  let err = '';
  try { await ctx.viewProjects(mkEl()); } catch (e) { err = e.message; }
  return { err, hitAdmin: calls.filter((c) => ADMIN_ONLY.some((p) => c.url.startsWith(p))).map((c) => c.url) };
}

async function testProjectPageByRole() {
  for (const [label, role, canAdmin] of [['普通成员', 'member', false], ['项目管理员', 'project_admin', true]]) {
    const { err, hitAdmin } = await projectPageAs(role, canAdmin);
    check(`${label}打开项目页不该报错`, err, '');
    check(`${label}不该请求管理员专属接口`, hitAdmin.join(','), '');
  }
}

// ---------- 用例 5：项目管理员该能拿到候选人 ----------
// 上一条别改过头：项目管理员要「把谁加进来」，得有个候选人名单可挑
async function testProjectAdminGetsCandidates() {
  const { ctx, calls } = makeCtx((url) => {
    if (url.includes('/candidates')) return { status: 200, body: [{ id: 9, name: '候选人' }] };
    if (url.includes('/members')) return { status: 200, body: [] };
    if (url.includes('/api/projects')) {
      return { status: 200, body: [{ id: 1, name: 'P', repo_count: 0, member_count: 0,
        role: 'project_admin', role_name: '项目管理员', can_admin: true }] };
    }
    if (url.includes('/api/repos')) return { status: 200, body: [] };
    if (url.includes('/api/me')) return { status: 200, body: { id: 2, name: '张三', role: 'user' } };
    return { status: 404, body: {} };
  });
  vm.runInContext('state.projectId = 1', ctx);
  await ctx.viewProjects(mkEl());
  check('项目管理员应拉项目候选人名单',
    calls.some((c) => c.url.includes('/candidates')), true);
  check('项目管理员不该拉全局用户表',
    calls.some((c) => c.url.startsWith('/api/users')), false);
}

// ---------- 用例 11：指派选择器的搜索 ----------
// 「按中文名或英文名模糊匹配」是明确要求：指派人往往只记得住对方平时被怎么叫。
// 匹配漏掉别名的话，界面上表现是「搜不到人」——不像报错那样显眼，只会让人以为这人不在项目里。
async function testAssigneePickerSearch() {
  const members = [
    { user_id: 1, name: '邓全杰', username: 'dengquanjie', aliases: ['dengquanjie1', 'Quanjie Deng'] },
    { user_id: 2, name: '王小明', username: 'wangxm', aliases: [] },
  ];
  const { ctx } = makeCtx(() => ({ status: 404, body: {} }));
  const picked = ctx.pickAssignee(members, { title: '指派给…' }); // 等它 resolve，先不 await
  const body = ctx.document.querySelector('#drawer-body').children[0];
  const kw = body.querySelector('#as-kw');
  const list = body.querySelector('#as-list');
  const hits = () => (list._html.match(/class="pick-item/g) || []).length;

  check('默认列出全部候选', hits(), 2);
  kw.value = '王小明'; kw.oninput();
  check('筛选后只剩匹配的人', hits(), 1);
  check('中文名能搜到', list._html.includes('未'), false);
  kw.value = 'dengquanjie1'; kw.oninput();
  check('git 身份（英文名）能搜到', list._html.includes('邓全杰'), true);
  kw.value = 'WANGXM'; kw.oninput();
  check('登录名匹配不分大小写', list._html.includes('王小明'), true);
  kw.value = '查无此人'; kw.oninput();
  check('搜不到时给提示，不留一片空白', list._html.includes('没有匹配的成员'), true);

  // 点一个人 → resolve 出他的 user_id，调用方据此发指派请求
  kw.value = ''; kw.oninput();
  list.onclick({ target: { closest: () => ({ dataset: { uid: '2' } }) } });
  check('选中的人选会带回 user_id', await picked, 2);
}

// ---------- 用例 12：数字列的表头必须跟着右对齐 ----------
// 表头默认左对齐、td.num 右对齐，标题贴左数字贴右，看着就是「上下没对齐」。
// 这个坑犯过两次（报告页、统计页人员表），所以在这儿钉住：
// 手写的 <table> 必须自己写 <th class="num">；走 table() 的那些由它自动补，静态看两边都是空的，正好跳过。
function testNumHeaderAlignment() {
  const src = fs.readFileSync(path.join(here, 'app.js'), 'utf8');
  const tpl = /<thead>(.*?)<\/thead>(.*?)(?=<thead>|$)/gs;
  let checked = 0;
  let bad = [];
  for (const m of src.matchAll(tpl)) {
    const head = [...m[1].matchAll(/<th[^>]*>/g)].map((x) => x[0]);
    const firstRow = m[2].match(/<tr[^>]*>(.*?)<\/tr>/s);
    if (!firstRow) continue;
    const cells = [...firstRow[1].matchAll(/<td[^>]*>/g)].map((x) => x[0]);
    if (!cells.length) continue;
    checked++;
    const at = (arr) => arr.map((t, i) => (t.includes('num') ? i : -1)).filter((i) => i >= 0).join(',');
    if (at(head) !== at(cells)) {
      bad.push(`第 ${src.slice(0, m.index).split('\n').length} 行：表头 num 列=[${at(head)}] 但数据 num 列=[${at(cells)}]`);
    }
  }
  check('扫描到了所有表格模板', checked >= 5, true);
  check('每张表的数字列表头都右对齐', bad.join('；'), '');
}

// ---------- 用例 13：问题库筛空之后，表头里的筛选器不能跟着消失 ----------
// 筛选下拉长在表头上。如果「一条都没筛出来」时整张表被换成「暂无数据」，
// 人就被卡在空结果里了：既看不出为什么空，也改不回筛选条件。
async function testIssueFilterHeaderSurvivesEmpty() {
  const { ctx } = makeCtx((url) => {
    if (url.includes('/api/issues')) return { status: 200, body: { total: 0, items: [] } };
    if (url.includes('/members')) return { status: 200, body: [{ user_id: 1, name: '甲' }] };
    return { status: 404, body: {} };
  });
  vm.runInContext('state.projectId = 1', ctx);
  // 渲染进自己造的容器：viewIssues 只认传进去的那个 el，不碰 #view
  const el = mkEl();
  let err = '';
  try { await ctx.viewIssues(el); } catch (e) { err = e.message; }
  check('一条都没筛出来时问题库仍能渲染', err, '');

  const html = textOf(el);
  for (const key of ['severity', 'category', 'source', 'assignee_id', 'status']) {
    check(`筛空时「${key}」的筛选器还在`, html.includes(`data-ms="${key}"`), true);
  }
  check('筛空时给出「没有符合条件的问题」', html.includes('没有符合条件的问题'), true);
}

// ---------- 用例 14：多选筛选怎么变成请求参数 ----------
// 前端把多选值拼成逗号串，后端按逗号拆回切片——这是前后端之间唯一的约定，
// 拼错了不会报错，只会「筛了等于没筛」（查出比预期多的数据），所以在这侧钉死。
async function testIssueMultiSelectParams() {
  const { ctx, calls } = makeCtx((url) => {
    if (url.includes('/api/issues')) return { status: 200, body: { total: 0, items: [] } };
    if (url.includes('/members')) return { status: 200, body: [{ user_id: 2, name: '甲' }] };
    return { status: 404, body: {} };
  });
  vm.runInContext(`state.projectId = 1;
    state.issues.severity = ['高危', '建议'];
    state.issues.status = ['pending', 'fixed'];
    state.issues.assignee_id = ['0', '2'];`, ctx);
  await ctx.viewIssues(mkEl());

  const url = calls.find((c) => c.url.startsWith('/api/issues')).url;
  check('多选等级按逗号拼进查询串', url.includes('severity=' + encodeURIComponent('高危,建议')), true);
  check('多选状态按逗号拼进查询串', url.includes('status=' + encodeURIComponent('pending,fixed')), true);
  check('「未指派」作为 0 和人一起传', url.includes('assignee_id=' + encodeURIComponent('0,2')), true);

  // 没勾的维度不该出现在查询串里（空串会被 qs 丢掉，后端就按「不筛」处理）
  check('没勾的维度不带参数', url.includes('category='), false);

  // 按钮文案：没筛选（全选）时要说清是「全部」
  const list = [['高危', '高危'], ['建议', '建议'], ['风格', '风格']];
  check('没筛选时按钮写「全部等级」', ctx.msLabel(list, [], '全部等级'), '全部等级');
  check('只留一个显示那一个', ctx.msLabel(list, ['建议'], '全部等级'), '建议');
  check('留两个显示首个 +N', ctx.msLabel(list, ['高危', '建议'], '全部等级'), '高危 +1');
}

// ---------- 用例 15：「全选」是默认态，全选/清空是一个开关 ----------
// 三条都不能错：
//  1. 打开下拉时所有框默认就是勾上的（全选是常态，不是要人先点一下）
//  2. 勾满**不能**变成「带上所有值去筛」——那样请求里多一堆冗余参数，
//     而且后端将来新增一个等级，前端这个列表没跟上就会把它漏掉
//  3. 一个都不勾和全勾含义相反：全勾=不筛，全不勾=一条都不要。
//     两者在传输层上都是空串，所以要靠哨兵值区分开
function testMultiSelectDefaultAll() {
  const { ctx } = makeCtx(() => ({ status: 404, body: {} }));
  const list = [['高危', '高危'], ['建议', '建议'], ['风格', '风格']];

  const none = ctx.multiSelect('severity', '等级', list, []);
  check('默认全勾', (none.html.match(/checked/g) || []).length, 3);
  check('全勾着时按钮给的是「清空」', none.html.includes('>清空</button>'), true);

  const partial = ctx.multiSelect('severity', '等级', list, ['建议']);
  check('只留一个时只勾一个', (partial.html.match(/checked/g) || []).length, 1);
  check('没全勾时按钮给的是「全选」', partial.html.includes('>全选</button>'), true);

  const box = (v, on) => ({ value: v, checked: on });
  check('三个都勾 = 不带筛选条件',
    JSON.stringify(ctx.msVals([box('高危', true), box('建议', true), box('风格', true)])), '[]');
  check('只勾两个 = 筛这两个',
    JSON.stringify(ctx.msVals([box('高危', true), box('建议', true), box('风格', false)])), '["高危","建议"]');
  check('一个都不勾 = 筛一个不存在的值（不是「全选」，那样点了清空列表纹丝不动）',
    JSON.stringify(ctx.msVals([box('高危', false), box('建议', false)])), '["__none__"]');
  check('清空后按钮文案是「未选择」',
    ctx.msLabel(list, ['__none__'], '全部等级'), '未选择');
}

console.log('webui 冒烟检查');
await testLoginErrorVisible();
await testSessionExpiredStillRedirects();
await testPasswordFormAlwaysRendered();
await testProjectPageByRole();
await testProjectAdminGetsCandidates();
await testProjectScopeOnEveryRequest();
await testAdminSettingsWithoutProject();
await testUnknownProjectDoesNotFallBack();
await testNoStrayTextNodes();
await testAdminSettingsSaveRerenders();
await testManualTriggerDrawer();
await testAssigneePickerSearch();
testNumHeaderAlignment();
await testIssueFilterHeaderSurvivesEmpty();
await testIssueMultiSelectParams();
testMultiSelectDefaultAll();
console.log(failed === 0 ? '\n全部通过' : `\n${failed} 项失败`);
process.exit(failed === 0 ? 0 : 1);

// ---------- 用例 10：手动触发抽屉 ----------
// 两条在浏览器里都是「点了没反应」或者更糟的形状：
//  1. 抽屉打不开（渲染路径抛异常，界面上就是个空白抽屉）
//  2. 提交按钮连点两下 → 两次真实的大模型审核。这是花钱的，所以在请求这一侧钉住。
async function testManualTriggerDrawer() {
  const { ctx, calls } = makeCtx((url) => {
    if (url.includes('/commits')) {
      return { status: 200, body: { synced: true, synced_at: '2026-09-21 12:00:00',
        items: [{ sha: 'a'.repeat(40), author: 'a', date: '2026-09-21T12:00:00+08:00', subject: 'fix: 登录态丢失' }] } };
    }
    if (url.includes('/trigger')) return { status: 200, body: { task_id: 7 } };
    return { status: 404, body: {} };
  });

  let err = '';
  try { await ctx.triggerForm({ id: 3, name: 'R', can_admin: true }); } catch (e) { err = e.message; }
  check('手动触发抽屉能打开', err, '');
  check('抽屉会去拉提交候选',
    calls.some((c) => c.url.includes('/api/repos/3/commits')), true);

  // openDrawer 和这里的 #drawer-body 走的是同一个缓存桩，所以能拿到刚挂上去的表单
  const body = ctx.document.querySelector('#drawer-body').children[0];
  const go = body.querySelector('#t-go');
  body.querySelector('#t-to').value = 'a'.repeat(40);
  go.onclick();
  go.onclick(); // 第一下还没落地就再点一下
  await new Promise((r) => setTimeout(r, 10));
  check('连点提交按钮只会触发一次审核',
    calls.filter((c) => c.url.includes('/trigger')).length, 1);
}

// ---------- 用例 6：项目页的每个请求都要带 project_id ----------
// 「进入项目后数据严格按项目过滤」是靠前端把 project_id 挂到每个请求上实现的。
// 漏掉任何一个页面，表现出来不是报错，而是那个页面的数字偷偷变成全平台的——
// 从后端日志上看完全正常，只能在请求这一侧钉住。
async function testProjectScopeOnEveryRequest() {
  const { ctx, view, calls } = makeCtx((url) => {
    if (url.includes('/api/projects')) {
      return { status: 200, body: [{ id: 1, name: 'P', repo_count: 1, member_count: 1,
        role: 'member', role_name: '成员', can_admin: false }] };
    }
    if (url.includes('/api/me')) return { status: 200, body: { id: 2, name: '张三', role: 'user' } };
    if (url.includes('/api/repos')) {
      return { status: 200, body: [
        { id: 7, name: '本项目的仓库', project_id: 1 },
        { id: 8, name: '别人项目的仓库', project_id: 2 }] };
    }
    if (url.includes('/api/issues')) return { status: 200, body: { items: [], total: 0 } };
    if (url.includes('/api/tasks')) return { status: 200, body: { items: [], total: 0 } };
    if (url.includes('/api/reports')) return { status: 200, body: { items: [], total: 0 } };
    // overview 是个对象、其余是数组。一律返回 [] 的话，概览页会在
    // ov.avg_handle_hours.toFixed() 上炸掉——那是桩喂错了数据，不是页面的锅
    if (url.includes('/api/stats/overview')) {
      return { status: 200, body: { new_count: 0, high_count: 0, advice_count: 0, style_count: 0,
        pending: 0, confirmed: 0, fixed: 0, false_positive: 0, regression: 0,
        handle_rate: 0, false_rate: 0, avg_handle_hours: 0 } };
    }
    if (url.includes('/api/stats/')) return { status: 200, body: [] };
    return { status: 200, body: {} };
  });
  // state 是 const 声明，不会挂到 context 对象上，只能在上下文里赋值
  vm.runInContext('state.user = { id: 2, name: "张三", role: "user" }', ctx);

  // 项目页的每个数据接口都不该漏掉 project_id
  const DATA = ['/api/issues', '/api/reports', '/api/tasks', '/api/stats/'];
  for (const page of ['dashboard', 'issues', 'reports', 'tasks', 'stats', 'settings']) {
    calls.length = 0;
    ctx.location.hash = `#/p/1/${page}`;
    await ctx.route();
    // route() 把渲染期抛的异常兜成一个「加载失败」卡片，页面看着是好的、只是没内容。
    // 所以「请求都发对了」不等于「这页没坏」——曾有一个 cards is not defined
    // 就这么藏过去了。两样都要断言。
    check(`项目页 ${page} 渲染没有报错`,
      /加载失败/.test(view._html) ? view._html.slice(0, 120) : '', '');
    check(`项目页 ${page} 渲染出了内容`, view.children.length > 0, true);
    if (page === 'settings') continue; // 设置页不拉这些问题数据
    const scoped = calls.filter((c) => DATA.some((p) => c.url.startsWith(p)));
    check(`项目页 ${page} 确实请求了数据（用例本身没空跑）`, scoped.length > 0, true);
    const missing = scoped.filter((c) => !c.url.includes('project_id=1')).map((c) => c.url);
    check(`项目页 ${page} 的请求都带 project_id=1`, missing.join(' | '), '');
  }

  // 概览一次要打好几个统计接口，逐个确认而不是只看第一个
  calls.length = 0;
  ctx.location.hash = '#/p/1/dashboard';
  await ctx.route();
  const stats = calls.filter((c) => c.url.startsWith('/api/stats/'));
  check('概览把 project_id 带给每一个统计接口',
    stats.every((c) => c.url.includes('project_id=1')), true);
  check('概览统计接口数量符合预期', stats.length, 6);

  // 仓库下拉只列本项目仓库：列出别的项目的仓库等于把数据范围放出去了
  const opts = ctx.document.querySelector('#repo-filter').innerHTML;
  check('仓库下拉含「本项目全部仓库」', opts.includes('本项目全部仓库'), true);
  check('仓库下拉含本项目仓库', opts.includes('本项目的仓库'), true);
  check('仓库下拉不含别的项目的仓库', opts.includes('别人项目的仓库'), false);

  // 工作台是「选项目」的地方，不该带任何项目上下文
  calls.length = 0;
  ctx.location.hash = '#/workbench';
  await ctx.route();
  check('工作台不该请求项目页数据',
    calls.filter((c) => DATA.some((p) => c.url.startsWith(p))).length, 0);
  check('工作台渲染出了项目卡片',
    ctx.document.querySelector('#view').children.length > 0, true);

  // 首页是所有用户的默认落点，渲染挂了就是全站白屏——这条只守「不报错」
  let homeErr = '';
  try {
    ctx.location.hash = '#/home';
    await ctx.route();
  } catch (e) { homeErr = e.message; }
  check('首页能渲染且不报错', homeErr, '');
  check('首页渲染出了内容', ctx.document.querySelector('#view').children.length > 0, true);

  // 空 hash（首次访问）必须落到首页，而不是某个项目或空白
  const fresh = makeCtx(() => ({ status: 200, body: [] }));
  vm.runInContext('state.user = { id: 2, name: "张三", role: "user" }', fresh.ctx);
  let freshErr = '';
  try { await fresh.ctx.route(); } catch (e) { freshErr = e.message; }
  check('空 hash 落到首页且不报错', freshErr, '');
  check('空 hash 时顶栏显示首页页签而非项目工具',
    fresh.ctx.document.querySelector('#home-tabs').hidden, false);
  check('空 hash 时侧栏六个项目页签是收起的',
    fresh.ctx.document.querySelector('#nav').hidden, true);
}

// ---------- 用例 7：全局管理员不加入任何项目也能进平台设置 ----------
// 真实的坑：平台设置一度只挂在项目页的「设置」里，而项目页要先选项目才进得去。
// 一个项目都没建的全局管理员（部署完第一次登录就是这样）就永远够不着模型网关、
// 通知和用户管理——界面看着一切正常，只是那个入口不存在。
async function testAdminSettingsWithoutProject() {
  const mk = (role) => {
    const { ctx, calls } = makeCtx((url) => {
      if (url.includes('/api/projects')) return { status: 200, body: [] }; // 一个项目都没有
      if (url.includes('/api/me')) return { status: 200, body: { id: 1, name: '管理员', role } };
      if (url.includes('/api/model-config')) {
        return { status: 200, body: { gateway_url: '', models: [], default_model: '', timeout_sec: 600 } };
      }
      return { status: 200, body: {} };
    });
    vm.runInContext(`state.user = { id: 1, name: "管理员", role: "${role}" }`, ctx);
    return { ctx, calls };
  };

  // 管理员：一个项目都没有，也要能直达系统设置并真的拉到平台配置
  const a = mk('admin');
  a.ctx.location.hash = '#/admin';
  let err = '';
  try { await a.ctx.route(); } catch (e) { err = e.message; }
  check('管理员没有任何项目时进系统设置不报错', err, '');
  check('系统设置真的拉取了平台配置',
    a.calls.some((c) => c.url.startsWith('/api/model-config')), true);
  check('管理员能看到「系统设置」入口',
    a.ctx.document.querySelector('#admin-nav').hidden, false);
  check('系统设置页不显示项目页签',
    a.ctx.document.querySelector('#nav').hidden, true);
  check('系统设置页不显示项目切换按钮',
    a.ctx.document.querySelector('#proj-switch').hidden, true);

  // 非管理员：手敲地址也不该进得去（进去只会吃一屏 403）
  const u = mk('user');
  u.ctx.location.hash = '#/admin';
  try { await u.ctx.route(); } catch { /* 跳转本身不影响断言 */ }
  check('非管理员看不到「系统设置」入口',
    u.ctx.document.querySelector('#admin-nav').hidden, true);
  check('非管理员敲地址会被送回首页', u.ctx.location.hash, '#/home');

  // 首页也要能直接点到系统设置，不必先绕进某个项目
  const h = mk('admin');
  h.ctx.location.hash = '#/workbench';
  await h.ctx.route();
  check('工作台（无项目）上管理员的系统设置入口仍在',
    h.ctx.document.querySelector('#admin-nav').hidden, false);
}


// ---------- 用例 8：项目 id 不存在时不许悄悄换成另一个项目 ----------
// 以前的写法是 `projects.find(...) || projects[0]`：URL 写着 A，页面显示的却是 B 的
// 仓库和成员。「明明有仓库却显示没有」正是这么来的——静默回落比报错难查得多。
async function testUnknownProjectDoesNotFallBack() {
  const { ctx, view, calls } = makeCtx((url) => {
    if (url.includes('/api/projects')) {
      return { status: 200, body: [{ id: 1, name: '默认项目', repo_count: 0, member_count: 1,
        role: 'admin', role_name: '全局管理员', can_admin: true }] };
    }
    if (url.includes('/api/me')) return { status: 200, body: { id: 1, name: '管理员', role: 'admin', username: 'admin' } };
    if (url.includes('/api/repos')) return { status: 200, body: [] };
    if (url.includes('/members')) return { status: 200, body: [{ user_id: 1, name: '管理员', role: 'project_admin', role_name: '项目管理员' }] };
    return { status: 200, body: [] };
  });
  vm.runInContext('state.user = { id: 1, name: "管理员", role: "admin", username: "admin" }', ctx);

  // 99 不在项目列表里：页面必须说清楚，而不是拿 1 号项目来充数
  ctx.location.hash = '#/p/99/settings';
  await ctx.route();
  check('不存在的项目 id 会说明项目不存在', textOf(view).includes('项目不存在或你无权访问'), true);
  check('不存在的项目 id 不会去拉别的项目的数据',
    calls.some((c) => /\/api\/projects\/1\/members|\/api\/projects\/1\/candidates/.test(c.url)), false);

  // 概览页同理——它以前会拿不存在的 project_id 去查，安静地全返回空
  calls.length = 0;
  ctx.location.hash = '#/p/99/dashboard';
  await ctx.route();
  check('不存在的项目 id 在概览页也说明项目不存在',
    textOf(view).includes('项目不存在或你无权访问'), true);
  check('不存在的项目 id 不会发统计请求',
    calls.some((c) => c.url.startsWith('/api/stats/')), false);

  // 存在的项目照常渲染，别把守卫做成谁都不给进
  ctx.location.hash = '#/p/1/settings';
  await ctx.route();
  check('存在的项目仍能正常渲染', textOf(view).includes('项目不存在'), false);
}

// ---------- 用例 9：不许把提示语当节点 append 进去 ----------
// 真实 bug：`table(cols, rows, fn), '这个项目下还没有仓库'))` —— 那个 ) 提前关掉了
// table(，于是提示语成了 append() 的第二个参数。append 接受多个节点，浏览器就把它
// 渲染成一段裸文字，而且是**无条件**出现的：列表明明有仓库有成员，底下照样多一行
// 「这个项目下还没有仓库」。空态提示语被当成了装饰。
function strayText(n, out = []) {
  if (!n || typeof n !== 'object') return out;
  for (const c of n.children || []) {
    if (typeof c === 'string' || typeof c === 'number') out.push(String(c));
    else strayText(c, out);
  }
  return out;
}

async function testNoStrayTextNodes() {
  // 注意匹配顺序：/api/projects/1/members 也包含 '/api/projects'，
  // 先判它会把成员列表喂成项目列表——这正是本用例要防的那种「数据不对但页面不报错」
  const { ctx, view } = makeCtx((url) => {
    if (url.includes('/members')) {
      return { status: 200, body: [{ user_id: 1, name: '甲', role: 'project_admin', role_name: '项目管理员' }] };
    }
    if (url.includes('/candidates')) return { status: 200, body: [] };
    if (url.includes('/api/repos')) {
      return { status: 200, body: [
        { id: 11, name: 'server', project_id: 1, language: 'go', enabled: true, trigger_webhook: true, can_admin: true },
        { id: 12, name: 'tools', project_id: 1, language: 'go', enabled: true, trigger_webhook: true, can_admin: true }] };
    }
    if (url.includes('/api/projects')) {
      return { status: 200, body: [{ id: 1, name: 'P', repo_count: 2, member_count: 2,
        role: 'project_admin', role_name: '项目管理员', can_admin: true }] };
    }
    if (url.includes('/api/me')) return { status: 200, body: { id: 1, name: '甲', role: 'user', username: '' } };
    return { status: 200, body: [] };
  });
  vm.runInContext('state.user = { id: 1, name: "甲", role: "user", username: "" }', ctx);
  ctx.location.hash = '#/p/1/settings';
  await ctx.route();

  const stray = strayText(view);
  check('项目设置页没有裸字符串被当成节点塞进去', stray.join(' | '), '');

  // 有仓库有成员时，那两句空态提示一个字都不该出现
  const t = textOf(view);
  check('有仓库时不该出现「这个项目下还没有仓库」', t.includes('这个项目下还没有仓库'), false);
  check('有成员时不该出现「还没有成员」', t.includes('还没有成员'), false);
  // 反向确认：数据确实渲染出来了，别是页面整个空掉才「没有提示」
  check('仓库名渲染出来了', t.includes('server'), true);
  check('成员名渲染出来了', t.includes('甲'), true);

  // 真的空的时候，提示语还是要在（别把功能一起修没了）
  const empty = makeCtx((url) => {
    if (url.includes('/api/repos') || url.includes('/members') || url.includes('/candidates')) {
      return { status: 200, body: [] };
    }
    if (url.includes('/api/projects')) {
      return { status: 200, body: [{ id: 1, name: 'P', repo_count: 0, member_count: 0,
        role: 'project_admin', role_name: '项目管理员', can_admin: true }] };
    }
    if (url.includes('/api/me')) return { status: 200, body: { id: 1, name: '甲', role: 'user', username: '' } };
    return { status: 200, body: [] };
  });
  vm.runInContext('state.user = { id: 1, name: "甲", role: "user", username: "" }', empty.ctx);
  empty.ctx.location.hash = '#/p/1/settings';
  await empty.ctx.route();
  const et = textOf(empty.view);
  check('真的没仓库时才提示「这个项目下还没有仓库」', et.includes('这个项目下还没有仓库'), true);
  check('真的没成员时才提示「还没有成员」', et.includes('还没有成员'), true);
}

// ---------- 用例 10：平台设置「保存」之后要能重新渲染 ----------
// 把 viewSettings 改名成 viewAdminSettings 时漏改了 11 处调用，于是点保存后
// 一走进重渲染就 ReferenceError，被 catch 吞成一句「viewSettings is not defined」。
// 保存其实成功了，人看到的却是报错，自然会以为没存上。
async function testAdminSettingsSaveRerenders() {
  const { ctx } = makeCtx((url) => {
    if (url.includes('/api/model-config')) {
      return { status: 200, body: { gateway_url: 'http://gw', models: ['m1'], default_model: 'm1',
        timeout_sec: 600, has_gateway_key: false, gateway_source: '' } };
    }
    if (url.includes('/api/me')) return { status: 200, body: { id: 1, name: '管理员', role: 'admin', username: 'admin' } };
    return { status: 200, body: [] };
  });
  vm.runInContext('state.user = { id: 1, name: "管理员", role: "admin", username: "admin" }', ctx);
  ctx.location.hash = '#/admin';
  await ctx.route();

  const pane = ctx.document.querySelector('#view').children[1];
  pane.querySelector('#m-url').value = 'http://gw';
  await pane.querySelector('#m-save').onclick();

  const msg = ctx.document.querySelector('#toast').textContent;
  check('保存平台设置不该报「未定义」', /is not defined/.test(msg), false);
  check('保存平台设置给出成功提示', msg.includes('已保存'), true);
}
