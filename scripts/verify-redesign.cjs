// 改版验收：对话页新排版（顶部四模式分段控件 / 主题切换 / 右上角「当前任务」浮层卡片 / 空状态类）
// 的真 DOM 级验证。用 jsdom 把真实的 chat/index.html + 真实 JS 跑起来，
// 然后像用户一样点击、改值、发消息，断言界面真的产生了预期变化。
//
// 为什么不用真浏览器：本机 Chrome 在沙箱里起不来（mojo IPC 报「拒绝访问」），
// Playwright 同样被拦。jsdom 是退而求其次的**真实 DOM 实现**——它不渲染像素，
// 但会真实执行 JS、真实跑事件、真实维护 DOM 与 localStorage，足以验证交互接线。
//
// 跑法（服务端先起好）：
//   .\bin\blog-my.exe
//   node scripts/verify-redesign.cjs
//
// 依赖：npm ci 安装的 jsdom；也可通过 JSDOM_PATH 指定包目录。
//
// 断言口径以 AGENT_TASK_verify_redesign.md 的契约为准：只认契约里写明的
// id / 全局对象 / localStorage 键，不猜实现细节——这样实现换写法时脚本不用跟着改。
'use strict';

const fs = require('fs');
const path = require('path');

const BASE = process.env.E2E_BASE || 'http://127.0.0.1:8080';
const ROOT = path.resolve(__dirname, '..');
// 默认用仓库里的模板；设了 TEMPLATES_DIR 就改用别的目录（和 verify-ui.cjs 一致），
// 这样同一份脚本也能验打包产物里的前端，而不是只验仓库。
const TEMPLATES_DIR = process.env.TEMPLATES_DIR || path.join(ROOT, 'templates');

let pass = 0;
let fail = 0;
let skipped = 0;
// 页面脚本自己抛的错（unhandled rejection / window.onerror）。不拦的话 Node 会直接
// 用未处理的 rejection 杀掉整个进程，A~G 一条都拿不到；拦住后它们变成可读的
// 「页面错误」清单，断言该失败还是失败，只是不再连累整次验证。
const pageErrors = [];
process.on('unhandledRejection', (err) => {
  pageErrors.push(String(err && err.stack ? err.stack : err).split('\n')[0]);
});
function log(ok, msg) {
  if (ok) pass++;
  else fail++;
  console.log((ok ? '[PASS] ' : '[FAIL] ') + msg);
}
function skip(msg) {
  skipped++;
  console.log('[SKIP] ' + msg);
}

const { loadJsdom } = require('./lib/jsdom.cjs');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function register(username, password) {
  const res = await fetch(BASE + '/register', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ username, password }).toString(),
    redirect: 'manual',
  });
  const cookie = (res.headers.getSetCookie() || []).map((c) => c.split(';')[0]).join('; ');
  if (!cookie) throw new Error('注册后没拿到 cookie（status=' + res.status + '）');
  return cookie;
}

// jsdom 的 dispatchEvent 不会把监听器里的异常抛给调用方，这里再兜一层，
// 保证「实现里有 bug」表现为某条断言 FAIL，而不是脚本直接崩掉、后面的断言全部拿不到结果。
function click(window, el) {
  try {
    el.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
  } catch (e) {
    /* 交给断言用实际 DOM 状态判定 */
  }
}

async function main() {
  const { JSDOM } = loadJsdom();

  const ts = Date.now();
  const user = 'e2eredesign' + ts;
  const cookie = await register(user, 'test123456');
  console.log('测试用户: ' + user + '\n');

  const html = await (await fetch(BASE + '/', { headers: { Cookie: cookie } })).text();
  const dom = new JSDOM(html, { url: BASE + '/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const doc = window.document;

  // 页面里同步抛出的脚本错误（事件监听器、内联脚本）也收集起来，方便把
  // 「某条断言为什么失败」和「控制台里的异常」对上号。
  window.addEventListener('error', (ev) => {
    if (ev && ev.message) pageErrors.push(String(ev.message));
  });

  // fetch 桩：jsdom 的 window 上没有 fetch，而 chat.js 依赖它。这里**不**拦 /api/chat——
  // G 段要打真实后端与真实模型，只在请求上补 session cookie。
  const realFetch = globalThis.fetch;
  window.fetch = (input, init = {}) => {
    const url = new URL(typeof input === 'string' ? input : input.url, BASE).toString();
    const opts = Object.assign({}, init, {
      headers: Object.assign({}, init.headers || {}, { Cookie: cookie }),
    });
    // chat.js 传的是 jsdom 自己的 AbortSignal，Node 的 fetch（undici）只认自己的实例，
    // 直接透传会抛「Expected signal ... to be an instance of AbortSignal」，请求根本没发出去。
    // 这里把中止意图桥接到一个 Node 侧 controller 上，停止生成的语义照样能验。
    if (opts.signal) {
      const pageSignal = opts.signal;
      const ctrl = new AbortController();
      if (pageSignal.aborted) ctrl.abort();
      else if (typeof pageSignal.addEventListener === 'function') pageSignal.addEventListener('abort', () => ctrl.abort());
      opts.signal = ctrl.signal;
    }
    return realFetch(url, opts);
  };

  // 按 index.html 里真实的顺序注入脚本（jsdom 不会自己拉外部脚本）。
  const srcs = [...html.matchAll(/<script src="([^"]+)"/g)].map((m) => m[1]);
  log(srcs.length > 0, '准备：从页面里解析出 ' + srcs.length + ' 个脚本，按真实顺序注入');
  for (const src of srcs) {
    const file = path.join(TEMPLATES_DIR, src.replace(/^\//, ''));
    if (!fs.existsSync(file)) {
      skip('脚本文件不存在，跳过注入：' + src);
      continue;
    }
    const s = doc.createElement('script');
    s.textContent = fs.readFileSync(file, 'utf8');
    doc.body.appendChild(s);
  }

  // 等 init() 里的几个异步加载跑完
  await sleep(1500);

  // 首屏快照：卡片在 boot 完成后就该是可见的「常驻」状态（参考图里那块卡片一直都在）。
  // 必须在这里抓：后面的 A~D 会改模式和主题，而改模式会让 chat.js 顺手刷新卡片。
  const bootCard = doc.getElementById('task-card');
  const bootCardState = bootCard ? {
    hidden: bootCard.hidden === true,
    count: (bootCard.querySelector('.task-card-count') || {}).textContent || '(缺)',
    current: (bootCard.querySelector('.task-card-current') || {}).textContent || '(缺)'
  } : null;

  // ---------- A. 顶部四模式分段控件 ----------
  const kindSwitch = doc.getElementById('kind-switch');
  const kindBtns = kindSwitch ? [...kindSwitch.querySelectorAll('.kind-switch-btn')] : [];
  const kindSelect = doc.getElementById('kind-select');
  const kindValues = kindBtns.map((b) => b.getAttribute('data-kind'));
  const kindsOk = ['chat', 'work', 'code', 'learn'].every((k) => kindValues.indexOf(k) !== -1);
  log(kindBtns.length === 4 && kindsOk && !!kindSelect && kindSelect.value === 'work',
    'A. 顶部四模式分段控件：4 个按钮（data-kind=[' + kindValues.join(',') + ']），#kind-select 默认值=' +
    JSON.stringify(kindSelect ? kindSelect.value : null));

  // ---------- B. 点按钮 → 下拉 / 选中态 / 草稿键 同步 ----------
  let bOk = false;
  let bDetail = '';
  const codeBtn = kindSwitch ? kindSwitch.querySelector('.kind-switch-btn[data-kind="code"]') : null;
  if (!codeBtn) {
    bDetail = '找不到 #kind-switch 里的「代码」按钮';
  } else {
    click(window, codeBtn);
    await sleep(50);
    const othersActive = kindBtns.filter((b) => b !== codeBtn).some((b) => b.classList.contains('is-active'));
    const othersPressed = kindBtns.filter((b) => b !== codeBtn).some((b) => b.getAttribute('aria-pressed') === 'true');
    const draft = window.localStorage.getItem('blogmy:chatkind:__draft');
    bOk = !!kindSelect && kindSelect.value === 'code'
      && codeBtn.classList.contains('is-active')
      && codeBtn.getAttribute('aria-pressed') === 'true'
      && !othersActive && !othersPressed
      && draft === 'code';
    bDetail = '#kind-select=' + JSON.stringify(kindSelect ? kindSelect.value : null)
      + '，代码按钮 is-active=' + codeBtn.classList.contains('is-active')
      + '/aria-pressed=' + JSON.stringify(codeBtn.getAttribute('aria-pressed'))
      + '，其它按钮仍选中=' + othersActive + '/' + othersPressed
      + '，draft=' + JSON.stringify(draft);
  }
  log(bOk, 'B. 点「代码」→ 下拉同步为 code、选中态唯一、草稿键写入（' + bDetail + '）');

  // ---------- C. 反向同步：代码改 #kind-select 后调 sync() ----------
  let cOk = false;
  let cDetail = '';
  const syncFn = window.KIND_SWITCH && window.KIND_SWITCH.sync;
  if (!kindSelect || typeof syncFn !== 'function') {
    cDetail = '#kind-select 或 window.KIND_SWITCH.sync 不存在';
  } else {
    kindSelect.value = 'learn';
    try {
      syncFn();
    } catch (e) {
      cDetail = 'sync() 抛异常：' + (e && e.message ? e.message : e) + '；';
    }
    await sleep(50);
    const learnBtn = kindSwitch ? kindSwitch.querySelector('.kind-switch-btn[data-kind="learn"]') : null;
    const othersActive = kindBtns.filter((b) => b !== learnBtn).some((b) => b.classList.contains('is-active'));
    cOk = !!learnBtn && learnBtn.classList.contains('is-active') && !othersActive;
    cDetail += '学习按钮 is-active=' + (learnBtn ? learnBtn.classList.contains('is-active') : '按钮不存在')
      + '，其它按钮仍选中=' + othersActive;
  }
  log(cOk, 'C. 反向同步：#kind-select 改成 learn 后 sync() → 「学习」按钮跟随（' + cDetail + '）');

  // ---------- D. 主题切换 ----------
  const themeBtn = doc.getElementById('theme-toggle');
  const rootEl = doc.documentElement;
  const initialTheme = rootEl.dataset.theme || '';
  const validTheme = (t) => t === 'azure' || t === 'graphite';
  let dOk = false;
  let dDetail = '';
  if (!themeBtn) {
    dDetail = '#theme-toggle 不存在（初始 data-theme=' + JSON.stringify(initialTheme) + '）';
  } else {
    click(window, themeBtn);
    await sleep(50);
    const t1 = rootEl.dataset.theme || '';
    const stored1 = window.localStorage.getItem('blogmy:theme');
    click(window, themeBtn);
    await sleep(50);
    const t2 = rootEl.dataset.theme || '';
    const stored2 = window.localStorage.getItem('blogmy:theme');
    // 「两次点击恰好往返」等价于只有两个取值：t1 必须是另一个合法值，t2 必须回到初始值。
    dOk = validTheme(initialTheme) && validTheme(t1) && t1 !== initialTheme
      && t2 === initialTheme && stored1 === t1 && stored2 === t2;
    dDetail = '初始=' + JSON.stringify(initialTheme)
      + ' → 第一次=' + JSON.stringify(t1) + '(存储=' + JSON.stringify(stored1) + ')'
      + ' → 第二次=' + JSON.stringify(t2) + '(存储=' + JSON.stringify(stored2) + ')';
  }
  log(dOk, 'D. 主题切换：两次点击在 azure/graphite 间往返并写入 localStorage（' + dDetail + '）');

  // ---------- E. 「当前任务」卡片 ----------
  const card = doc.getElementById('task-card');
  let eOk = false;
  let eDetail = '';
  // E1：boot 后卡片常驻（「暂无任务 / 0/0 已完成」），不是等第一次对话才出现。
  const e1Ok = !!bootCardState && bootCardState.hidden === false
    && bootCardState.count.indexOf('0/0') !== -1
    && bootCardState.current.indexOf('暂无任务') !== -1;
  if (!card || !bootCardState) {
    eDetail = '#task-card 不存在';
  } else {
    if (!window.TASKCARD || typeof window.TASKCARD.update !== 'function') {
      eDetail = '首屏 hidden=' + bootCardState.hidden + '，但 window.TASKCARD.update 不存在';
    } else {
      try {
        window.TASKCARD.update({ kind: 'code', done: 2, total: 4, current: '整理今天的笔记', workspace: 'E:\\blog-my', busy: true, recalled: 3 });
      } catch (e) {
        eDetail = 'update() 抛异常：' + (e && e.message ? e.message : e);
      }
      await sleep(50);
      const countEl = card.querySelector('.task-card-count');
      const barEl = card.querySelector('.task-card-bar > i');
      const kindEl = card.querySelector('.task-card-kind');
      const curEl = card.querySelector('.task-card-current');
      const liveEl = card.querySelector('.task-card-live');
      const memEl = card.querySelector('.task-card-memory');
      const countText = countEl ? countEl.textContent : '(缺)';
      const barWidth = barEl ? barEl.style.width : '(缺)';
      const kindText = kindEl ? kindEl.textContent.trim() : '(缺)';
      const curText = curEl ? curEl.textContent : '(缺)';
      const memText = memEl ? memEl.textContent : '(缺)';
      eOk = e1Ok && card.hidden === false
        && countText.indexOf('2/4') !== -1
        && barWidth === '50%'
        && kindText === '代码'
        && curText.indexOf('整理今天的笔记') !== -1
        && !!liveEl && liveEl.hidden === false
        && !!memEl && memEl.hidden === false && memText.indexOf('3') !== -1;
      eDetail += '首屏 hidden=' + bootCardState.hidden + '/count=' + JSON.stringify(bootCardState.count)
        + '/current=' + JSON.stringify(bootCardState.current)
        + '，update 后 hidden=' + card.hidden
        + '，count=' + JSON.stringify(countText)
        + '，进度条=' + JSON.stringify(barWidth)
        + '，类型=' + JSON.stringify(kindText)
        + '，当前任务=' + JSON.stringify(curText)
        + '，执行中可见=' + (liveEl ? !liveEl.hidden : '(缺)')
        + '，回忆=' + JSON.stringify(memText) + '/hidden=' + (memEl ? memEl.hidden : '(缺)');
    }
  }
  log(eOk, 'E. 「当前任务」卡片：首屏常驻「暂无任务 / 0/0」，update() 后各字段按契约渲染（' + eDetail + '）');

  // ---------- F. 卡片收起 / 展开 ----------
  const cardToggle = card ? card.querySelector('.task-card-toggle') : null;
  let fOk = false;
  let fDetail = '';
  if (!cardToggle) {
    fDetail = '.task-card-toggle 不存在';
  } else {
    click(window, cardToggle);
    await sleep(50);
    const collapsed1 = card.classList.contains('is-collapsed');
    const expanded1 = cardToggle.getAttribute('aria-expanded');
    click(window, cardToggle);
    await sleep(50);
    const collapsed2 = card.classList.contains('is-collapsed');
    const expanded2 = cardToggle.getAttribute('aria-expanded');
    fOk = collapsed1 && expanded1 === 'false' && !collapsed2 && expanded2 === 'true';
    fDetail = '第一次点击后 is-collapsed=' + collapsed1 + '/aria-expanded=' + JSON.stringify(expanded1)
      + '，第二次点击后 is-collapsed=' + collapsed2 + '/aria-expanded=' + JSON.stringify(expanded2);
  }
  log(fOk, 'F. 卡片收起/展开：点击往返（' + fDetail + '）');

  // ---------- G. 空状态 ----------
  // 前半段（.app 上的 is-empty）是纯前端接线；后半段要真的把消息发出去，
  // 因此依赖真实后端与模型——模型不可用时明确 SKIP，不假装通过。
  const app = doc.querySelector('.app');
  const emptyState = doc.getElementById('empty-state');
  const initialEmpty = !!(app && app.classList.contains('is-empty'));
  const input = doc.getElementById('input');
  const composer = doc.getElementById('composer');
  const sendBtn = doc.getElementById('send-btn');
  const modelSelect = doc.getElementById('model-select');

  // 真实用户会避开标着「未配置 Key」的模型；优先选第一个有 Key 的，选不到就沿用默认。
  if (modelSelect && modelSelect.options.length) {
    const usable = [...modelSelect.options].find((o) => o.value && o.textContent.indexOf('未配置 Key') === -1);
    if (usable) {
      modelSelect.value = usable.value;
      modelSelect.dispatchEvent(new window.Event('change', { bubbles: true }));
    }
  }

  if (!app || !input || !composer) {
    skip('G. 空状态：#input / #composer / .app 不存在，无法模拟发送（初始 is-empty=' + initialEmpty + '）');
  } else {
    input.value = '你好';
    input.dispatchEvent(new window.Event('input', { bubbles: true }));
    composer.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));

    // 等真实 SSE 流收尾：AI 气泡有内容且发送键不再是停止态，即视为一轮结束。
    // 模型可能很慢，给 120 秒；超时 / 报错都按「后端模型不可用」SKIP。
    const deadline = Date.now() + 120000;
    let outcome = 'timeout（120 秒内没等到流结束）';
    const sentAt = Date.now();
    while (Date.now() < deadline) {
      await sleep(500);
      const toastEl = doc.getElementById('toast');
      const toastText = toastEl ? toastEl.textContent.trim() : '';
      if (toastText.indexOf('没有可用模型') !== -1) { outcome = 'no-model: ' + toastText; break; }
      const msgs = [...doc.querySelectorAll('#thread .msg')];
      const assistants = msgs.filter((m) => m.getAttribute('data-role') === 'assistant');
      const streaming = !!(sendBtn && sendBtn.classList.contains('is-stop'));
      // is-error 挂在消息里层的 .bubble 上（appendMessage 返回的是内层气泡，不是 .msg），
      // 而且 .msg 的 textContent 还带着头像「AI」与操作按钮，所以判定必须看内层气泡。
      const innerBubbles = assistants.map((m) => m.querySelector('.bubble')).filter(Boolean);
      const errBubble = innerBubbles.find((b) => b.classList.contains('is-error'));
      if (errBubble && !streaming) { outcome = 'error: ' + (errBubble.textContent || '').slice(0, 160); break; }
      const okBubble = innerBubbles.find((b) => !b.classList.contains('is-error') && (b.textContent || '').trim() !== '');
      if (okBubble && !streaming) { outcome = 'ok'; break; }
      // 正常实现里用户/AI 气泡是在发请求前就同步插进 DOM 的；10 秒还没有任何气泡，
      // 说明发送流程在渲染前就断了（多半是页面脚本抛错），没必要再干等两分钟。
      if (Date.now() - sentAt > 10000 && msgs.length === 0) {
        outcome = 'no-bubbles: 发送后 10 秒仍没有任何气泡（前端发送流程中断）';
        break;
      }
    }

    if (outcome !== 'ok') {
      skip('G. 空状态：后端/模型不可用，发送流程没跑完（' + outcome + '；初始 is-empty=' + initialEmpty + '），不判定');
    } else {
      const afterEmpty = app.classList.contains('is-empty');
      // 把模型回复的片段一起写进日志：确认这轮真的是后端模型跑完的，而不是空流转。
      const reply = (doc.querySelector('#thread .msg.assistant .bubble:not(.is-error)') || {}).textContent || '';
      const replyText = reply.trim();
      log(initialEmpty && !afterEmpty && !!emptyState,
        'G. 空状态：加载时 .app 带 is-empty，发出第一条消息后去掉（初始=' + initialEmpty + '，发送后=' + afterEmpty +
        '，#empty-state 存在=' + !!emptyState + '）');
      console.log('  [debug] 模型回复 ' + replyText.length + ' 字：' + JSON.stringify(replyText.slice(0, 40)));
    }
  }

  // ---------- H. 参数按钮要落在输入框那一排胶囊里 ----------
  // 这颗按钮是 chat.js 动态注入的（buildParamsButton）。改版后 form 变成竖向 flex：
  // 如果它被 append 到 form 上，就会单独占一行，视觉上像颗掉出来的孤儿胶囊。
  const paramsBtn = doc.getElementById('chat-params-btn');
  const composerToolbar = doc.querySelector('#composer .composer-toolbar');
  const hOk = !!paramsBtn && !!composerToolbar && paramsBtn.parentNode === composerToolbar;
  log(hOk, 'H. 参数按钮落在输入框工具条内（按钮存在=' + !!paramsBtn + '，实际父节点='
    + (paramsBtn && paramsBtn.parentNode ? (paramsBtn.parentNode.className || paramsBtn.parentNode.tagName) : '(无)')
    + '，工具条存在=' + !!composerToolbar + '）');

  // ---------- I. 胶囊下拉：原生 select 换成按钮 + 页内浮层 ----------
  let iOk = false;
  let iDetail = '';
  const modeSelect = doc.getElementById('mode-select');
  const chipBtn = modeSelect && modeSelect.parentNode ? modeSelect.parentNode.querySelector('.chipselect-btn') : null;
  if (!modeSelect || !chipBtn) {
    iDetail = '找不到 #mode-select 或它旁边的 .chipselect-btn';
  } else {
    click(window, chipBtn);
    await sleep(60);
    const pop = doc.querySelector('.chipselect-pop');
    const popOpts = pop ? [...pop.querySelectorAll('.chipselect-opt')] : [];
    const autoOpt = popOpts.filter((o) => (o.textContent || '').indexOf('自主执行') !== -1)[0];
    iDetail = '按钮存在=true，浮层=' + !!pop + '，选项数=' + popOpts.length;
    if (!autoOpt) {
      iDetail += '，但浮层里没有「自主执行」';
    } else {
      click(window, autoOpt);
      await sleep(80);
      iOk = modeSelect.value === 'auto' && !!pop && pop.hidden === true
        && chipBtn.getAttribute('aria-expanded') === 'false';
      iDetail += '，点「自主执行」后 #mode-select=' + JSON.stringify(modeSelect.value)
        + '，浮层收起=' + (pop ? pop.hidden : '(无)')
        + '，按钮 aria-expanded=' + JSON.stringify(chipBtn.getAttribute('aria-expanded'));
    }
  }
  log(iOk, 'I. 胶囊下拉：点按钮开浮层、点选项写回 select 并收起（' + iDetail + '）');

  // ---------- J. 页内对话框：prompt 不再用系统弹窗 ----------
  let jOk = false;
  let jDetail = '';
  const uiask = window.UIASK;
  if (!uiask || typeof uiask.prompt !== 'function') {
    jDetail = 'window.UIASK.prompt 不存在';
  } else {
    const pending = uiask.prompt({ title: '测试路径', label: '绝对路径', value: 'D:\\old' });
    await sleep(40);
    const dlg = doc.querySelector('.uiask-dialog');
    const dlgInput = dlg ? dlg.querySelector('.uiask-input') : null;
    const okBtn = dlg ? dlg.querySelector('.uiask-ok') : null;
    if (dlgInput) dlgInput.value = 'D:\\new-workspace';
    if (okBtn) click(window, okBtn);
    const typed = await pending;
    const pending2 = uiask.prompt({ title: '再来一次' });
    await sleep(40);
    const cancelBtn = doc.querySelector('.uiask-dialog .uiask-cancel');
    if (cancelBtn) click(window, cancelBtn);
    const cancelled = await pending2;
    jOk = typed === 'D:\\new-workspace' && cancelled === null;
    jDetail = '确定返回=' + JSON.stringify(typed) + '，取消返回=' + JSON.stringify(cancelled);
  }
  log(jOk, 'J. 页内对话框：prompt 能拿到输入，取消返回 null（' + jDetail + '）');

  // ---------- K. chat.js 里不该再留 window.prompt ----------
  // Electron 从未实现过它：调用只会返回 null，用户看到的就是「点了没反应」。
  const chatSrc = fs.readFileSync(path.join(TEMPLATES_DIR, 'static', 'js', 'chat.js'), 'utf8');
  const nativePromptHits = (chatSrc.match(/window\.prompt\s*\(/g) || []).length;
  log(nativePromptHits === 0, 'K. chat.js 里没有 window.prompt（Electron 不支持它）（命中=' + nativePromptHits + ' 处）');

  console.log('\n结果: ' + pass + ' 通过 / ' + fail + ' 失败' + (skipped ? ' / ' + skipped + ' 跳过' : ''));
  console.log('测试用户: ' + user + '（记得清理）');
  dom.window.close();
  if (fail > 0) process.exitCode = 1;

  // 页面脚本自身报的错单独列出来：它们往往就是上面某条 FAIL 的根因。
  // 这类错误一律让退出码非 0，避免「断言恰好都过了但页面其实在抛异常」。
  if (pageErrors.length) {
    console.log('\n页面脚本错误（' + pageErrors.length + ' 条）：');
    pageErrors.forEach((m) => console.log('  - ' + m));
    process.exitCode = 1;
  }
}

main().catch((e) => {
  console.error('脚本异常：' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
