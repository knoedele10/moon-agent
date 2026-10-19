// 真 DOM 级的前端功能验证：用 jsdom 把真实的 chat/index.html + 真实 JS 跑起来，
// 然后像用户一样操作（输入、点击、按键），断言界面真的产生了预期变化。
//
// 为什么不用真浏览器：本机 Chrome 在沙箱里起不来（mojo IPC 报「拒绝访问」），
// Playwright 同样被拦。jsdom 是退而求其次的**真实 DOM 实现**——它不渲染像素，
// 但会真实执行 JS、真实跑事件、真实维护 DOM 与 localStorage，足以验证交互接线。
//
// 跑法（服务端先起好）：
//   .\bin\blog-my.exe
//   node scripts/verify-ui.cjs
//
// 依赖：npm ci 安装的 jsdom；也可通过 JSDOM_PATH 指定包目录。
'use strict';

const fs = require('fs');
const path = require('path');

const BASE = process.env.E2E_BASE || 'http://127.0.0.1:8080';
const ROOT = path.resolve(__dirname, '..');
// 默认用仓库里的模板；设了 TEMPLATES_DIR 就改用别的目录。
// 这样可以直接把打包产物（dist/win-unpacked/resources/templates）拿进来验，
// 而不是只验仓库——毕竟用户跑的是打包版。
const TEMPLATES_DIR = process.env.TEMPLATES_DIR || path.join(ROOT, 'templates');

let pass = 0;
let fail = 0;
function log(ok, msg) {
  if (ok) pass++;
  else fail++;
  console.log((ok ? '[PASS] ' : '[FAIL] ') + msg);
}
function skip(msg) {
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

async function api(cookie, method, p, body) {
  const res = await fetch(BASE + p, {
    method,
    headers: { 'Content-Type': 'application/json', Cookie: cookie },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    /* 保留原文 */
  }
  return { status: res.status, json, text };
}

async function main() {
  const { JSDOM } = loadJsdom();

  // 无需启动后端即可验证任务卡片拖动本身；完整页面验收仍走下方 HTTP 流程。
  if (process.env.TASKCARD_ONLY === '1') {
    const src = fs.readFileSync(path.join(TEMPLATES_DIR, 'static/js/taskcard.js'), 'utf8');
    const dom = new JSDOM('<!doctype html><aside id="task-card" class="task-card" hidden></aside>', {
      url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true,
    });
    const { window } = dom;
    window.eval(src);
    window.TASKCARD.update({ current: 'drag verification' });
    const card = window.document.getElementById('task-card');
    const head = card.querySelector('.task-card-head');
    const dragEvents = typeof window.PointerEvent === 'function'
      ? ['pointerdown', 'pointermove', 'pointerup']
      : ['mousedown', 'mousemove', 'mouseup'];
    const pointerLike = (type, x, y) => new window.MouseEvent(type, {
      bubbles: true, cancelable: true, clientX: x, clientY: y, button: 0,
    });
    head.dispatchEvent(pointerLike(dragEvents[0], 100, 100));
    window.document.dispatchEvent(pointerLike(dragEvents[1], 180, 150));
    window.document.dispatchEvent(pointerLike(dragEvents[2], 180, 150));
    let saved = null;
    try { saved = JSON.parse(window.localStorage.getItem('blogmy:taskcard:pos') || 'null'); } catch (_) {}
    const moved = !!saved && card.style.left === saved.x + 'px' && card.style.top === saved.y + 'px';
    log(moved, '任务卡片独立验证：鼠标拖动更新位置并写入 localStorage（' + JSON.stringify(saved) + '）');

    const fresh = window.document.createElement('aside');
    fresh.id = 'task-card';
    fresh.className = 'task-card';
    fresh.hidden = true;
    card.parentNode.replaceChild(fresh, card);
    window.TASKCARD.init();
    const restored = !!saved && fresh.style.left === saved.x + 'px' && fresh.style.top === saved.y + 'px';
    log(restored, '任务卡片独立验证：重新 init 后恢复 localStorage 位置（left=' + fresh.style.left + '，top=' + fresh.style.top + '）');
    let opened = null;
    window.CHAT = { openConversation: (id, parent, title) => { opened = { id, parent, title }; } };
    window.TASKCARD.setTeam({ members: [
      { name: 'lead', role: 'lead', status: 'running', depth: 0 },
      { name: 'architect', role: 'teammate', status: 'running', depth: 1, child_id: '11', parent_id: 1 },
      { name: '整理目录', role: 'subagent', status: 'running', depth: 2, child_id: '12', parent_id: 11, activity: '正在读文件' },
    ], tasks: [], parent_id: 1 });
    const nestedRows = fresh.querySelectorAll('.team-member-row');
    if (nestedRows[2]) nestedRows[2].click();
    log(nestedRows.length === 3 && nestedRows[2].classList.contains('is-nested') &&
      nestedRows[2].textContent.includes('正在读文件') && opened && opened.id === '12' && opened.parent === 11,
      '团队面板独立验证：子子 agent 分层显示进度，并打开自己的会话');
    dom.window.close();

    // jsdom 默认没有 PointerEvent；用 MouseEvent 构造器模拟同名事件，走桌面 Chromium 的指针监听分支。
    const pointerDom = new JSDOM('<!doctype html><aside id="task-card" class="task-card" hidden></aside>', {
      url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true,
    });
    const pointerWindow = pointerDom.window;
    pointerWindow.PointerEvent = function (type, init) { return new pointerWindow.MouseEvent(type, init); };
    pointerWindow.eval(src);
    pointerWindow.TASKCARD.update({ current: 'pointer drag verification' });
    const pointerCard = pointerWindow.document.getElementById('task-card');
    const pointerHead = pointerCard.querySelector('.task-card-head');
    const pointerEvent = (type, x, y) => new pointerWindow.MouseEvent(type, {
      bubbles: true, cancelable: true, clientX: x, clientY: y, button: 0,
    });
    pointerHead.dispatchEvent(pointerEvent('pointerdown', 100, 100));
    pointerWindow.document.dispatchEvent(pointerEvent('pointermove', 180, 150));
    pointerWindow.document.dispatchEvent(pointerEvent('pointerup', 180, 150));
    const pointerSaved = pointerWindow.localStorage.getItem('blogmy:taskcard:pos');
    log(!!pointerSaved && pointerCard.classList.contains('is-positioned'),
      '任务卡片独立验证：PointerEvent 分支可拖动并保存位置（' + pointerSaved + '）');
    pointerDom.window.close();

    console.log('\n结果: ' + pass + ' 通过 / ' + fail + ' 失败');
    if (fail > 0) process.exitCode = 1;
    return;
  }

  const ts = Date.now();
  const user = 'e2eui2' + ts;
  const cookie = await register(user, 'test123456');
  console.log('测试用户: ' + user + '\n');

  // 造一条提示词，供「/ 快选」用
  const PROMPT_TITLE = '总结要点';
  const PROMPT_BODY = '把下面的内容总结成三条要点。';
  const pr = await api(cookie, 'POST', '/api/prompts', { title: PROMPT_TITLE, content: PROMPT_BODY });
  log(pr.status === 201, '准备：新建一条提示词（id=' + (pr.json && pr.json.id) + '）');

  const html = await (await fetch(BASE + '/', { headers: { Cookie: cookie } })).text();
  const dom = new JSDOM(html, { url: BASE + '/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const doc = window.document;

  // 剪贴板桩：chat.js 的 copyText 会优先用 navigator.clipboard，其次 execCommand。
  let clipboard = '';
  Object.defineProperty(window.navigator, 'clipboard', {
    configurable: true,
    value: { writeText: (t) => { clipboard = t; return Promise.resolve(); } },
  });
  doc.execCommand = (cmd) => { if (cmd === 'copy') { clipboard = doc.__lastSelected || clipboard; return true; } return false; };

  // fetch 桩：补上 session cookie；/api/chat 走「永不结束的流」，用来验证停止键。
  const realFetch = globalThis.fetch;
  let chatAbortCtrl = null;
  // stop：永不结束的流，用来验证停止键；subagent：先 running 再 done，用来验证卡片原地更新。
  // stop：永不结束的流，用来验证停止键；subagent：先 running 再 done，用来验证卡片原地更新；
  // ask：直接发一条带 options 的 need_input，用来验证选项按钮与「其他…」。
  let chatStub = 'stop';
  // 团队接口的 fetch 桩：null = 走真接口；设成对象后 /api/conversations/:id/team 直接返回它。
  // 后端团队接口可能还没上，团队面板的断言用这个桩喂数据，不依赖真接口。
  let teamStub = null;
  window.fetch = (input, init = {}) => {
    const url = new URL(typeof input === 'string' ? input : input.url, BASE).toString();
    const opts = Object.assign({}, init, {
      headers: Object.assign({}, init.headers || {}, { Cookie: cookie }),
    });
    if (teamStub !== null && /\/api\/conversations\/[^/]+\/team$/.test(url)) {
      return Promise.resolve(new globalThis.Response(JSON.stringify(teamStub), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }));
    }
    if (url.includes('/api/chat')) {
      const enc = new TextEncoder();
      let ctrl = null;
      // 用 Node 的全局 ReadableStream/Response：jsdom 的 window 上不一定有这两个构造函数，
      // 用 window.* 会直接抛错，fetch 桩立刻失败——那就测不到「生成中」这个状态了。
      const stream = new globalThis.ReadableStream({
        start(c) {
          ctrl = c;
          if (chatStub === 'subagent') {
            c.enqueue(enc.encode('event: subagent\ndata: {"child_id":42,"label":"总结文档","status":"running","stop_reason":"","reply":"","error":"","depth":1,"started_at":"2026-10-14T10:00:00Z","finished_at":null}\n\n'));
            globalThis.setTimeout(() => {
              try {
                c.enqueue(enc.encode('event: subagent\ndata: {"child_id":42,"label":"总结文档","status":"done","stop_reason":"","reply":"三份文档都总结完了。","error":"","depth":1,"started_at":"2026-10-14T10:00:00Z","finished_at":"2026-10-14T10:00:01Z"}\n\nevent: done\ndata: {}\n\n'));
                c.close();
              } catch (e) {
                /* 流已经被停止按钮关掉了 */
              }
            }, 250);
          } else if (chatStub === 'ask') {
            // 选项式提问：一次把 need_input（带 options）与 done 发完，流随即关闭。
            // options 形状与后端冻结契约一致：string[]。
            c.enqueue(enc.encode('event: need_input\ndata: {"question":"用哪个名字？","tool":"ask_user","confirm":false,"display":{"question":"用哪个名字？","options":["小月","小羊"]}}\n\nevent: done\ndata: {}\n\n'));
            c.close();
          } else {
            c.enqueue(enc.encode('event: delta\ndata: {"content":"生成中"}\n\n'));
            // stop 模式刻意不 close：模拟「还在生成」，等测试点停止。
          }
        },
      });
      chatAbortCtrl = ctrl;
      if (opts.signal) {
        opts.signal.addEventListener('abort', () => {
          try {
            ctrl.error(Object.assign(new Error('aborted'), { name: 'AbortError' }));
          } catch (e) {
            /* 已经关了就忽略 */
          }
        });
      }
      return Promise.resolve(new globalThis.Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }));
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

  // ---------- 1. Markdown 代码块复制按钮 ----------
  log(!!window.MD && typeof window.MD.render === 'function', 'MD 渲染器已加载');
  const mdHtml = window.MD ? window.MD.render('```js\nconst a = 1;\n```') : '';
  log(mdHtml.includes('md-code-copy'), '代码块：渲染结果里有复制按钮');
  log(mdHtml.includes('md-code'), '代码块：结构未破坏');

  // ---------- 2. 参数面板 ----------
  const paramsBtn = doc.getElementById('chat-params-btn');
  log(!!paramsBtn, '参数面板：输入行里有「参数」按钮');
  if (paramsBtn) {
    paramsBtn.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
    await sleep(50);
    const panel = doc.querySelector('.chat-params');
    log(!!panel && panel.hidden === false, '参数面板：点击后展开');
    const ta = doc.getElementById('chat-params-system');
    // 温度用的是 class 而不是 id（见 chat.js 的 buildParamsPanel），所以按类选。
    const range = doc.querySelector('.chat-params-range');
    const num = doc.getElementById('chat-params-maxtokens');
    log(!!ta && !!range && !!num, '参数面板：三个字段都在（额外系统提示词 / 温度 / 最大 token）');
    if (ta && range && num) {
      ta.value = '回答尽量简短。';
      ta.dispatchEvent(new window.Event('input', { bubbles: true }));
      num.value = '256';
      num.dispatchEvent(new window.Event('input', { bubbles: true }));
      await sleep(50);
      const saved = window.localStorage.getItem('blogmy:chatparams:__draft');
      const parsed = saved ? JSON.parse(saved) : {};
      log(parsed.system_prompt === '回答尽量简短。' && Number(parsed.max_tokens) === 256,
        '参数面板：改动按会话写进了 localStorage（' + saved + '）');
    }
    paramsBtn.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
    await sleep(30);
  }

  // ---------- 3. `/` 提示词快选 ----------
  const input = doc.getElementById('input');
  log(!!input, '输入框存在');
  if (input) {
    input.value = '/';
    input.dispatchEvent(new window.Event('input', { bubbles: true }));
    await sleep(120);
    const picker = doc.querySelector('.prompt-picker');
    log(!!picker, '提示词快选：输入 / 后弹出候选列表');
    const items = doc.querySelectorAll('.prompt-picker-item');
    log(items.length >= 1, '提示词快选：候选里有 ' + items.length + ' 项');
    const first = doc.querySelector('.prompt-picker-title');
    log(!!first && first.textContent.includes(PROMPT_TITLE), '提示词快选：候选里能看到刚建的提示词（' + (first && first.textContent) + '）');

    if (picker) {
      input.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      await sleep(80);
      log(input.value === PROMPT_BODY, '提示词快选：按 Enter 后把正文填进输入框（' + JSON.stringify(input.value) + '）');
      // closePromptPicker() 是把盒子 hidden 并清空，不是从 DOM 里删掉——按真实行为断言。
      const box = doc.querySelector('.prompt-picker');
      log(!!box && box.hidden === true && box.textContent === '', '提示词快选：选中后候选列表收起（hidden 且已清空）');
    }
  }

  // ---------- 4. 停止生成 ----------
  const sendBtn = doc.getElementById('send-btn');
  const composer = doc.getElementById('composer');
  log(!!sendBtn && !!composer, '发送按钮与表单存在');
  if (sendBtn && composer && input) {
    input.value = '你好';
    input.dispatchEvent(new window.Event('input', { bubbles: true }));
    composer.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
    await sleep(300);
    console.log('  [debug] 提交后 sendBtn.className = ' + JSON.stringify(sendBtn.className) + ' / aria-label=' + JSON.stringify(sendBtn.getAttribute('aria-label')) + ' / 气泡数=' + doc.querySelectorAll('.msg').length);
    log(sendBtn.classList.contains('is-stop'), '停止生成：生成中发送键变成停止态（is-stop）');
    log((sendBtn.getAttribute('aria-label') || '').includes('停止') || (sendBtn.title || '').includes('停止'),
      '停止生成：无障碍标签变成「停止」（aria-label=' + sendBtn.getAttribute('aria-label') + '）');

    sendBtn.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
    await sleep(400);
    log(!sendBtn.classList.contains('is-stop'), '停止生成：点击后恢复成发送态');
    const bubbles = doc.querySelectorAll('.msg');
    log(bubbles.length >= 1, '停止生成：已生成的内容留在界面上（气泡数 ' + bubbles.length + '）');
  }

  // ---------- 5. 代码块复制点击 ----------
  clipboard = '';
  // 必须挂进消息容器：chat.js 的复制是**事件委托在 #messages 上**的，挂到 body 上不会触发。
  const host = doc.createElement('div');
  host.className = 'msg';
  host.innerHTML = window.MD.render('```js\nconst answer = 42;\n```');
  const thread = doc.getElementById('thread') || doc.getElementById('messages') || doc.body;
  thread.appendChild(host);
  const copyBtn = host.querySelector('.md-code-copy');
  log(!!copyBtn, '代码块复制：按钮已渲染到 DOM 里');
  if (copyBtn) {
    copyBtn.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
    await sleep(120);
    log(clipboard.includes('const answer = 42;'), '代码块复制：点击后代码进了剪贴板（' + JSON.stringify(clipboard.slice(0, 40)) + '）');
  }

  // ---------- 6. 知识库的来源分段筛选 ----------
  await api(cookie, 'POST', '/api/kb/text', { title: 'UI 验证用资料', text: '这段文字用来验证知识库列表的来源徒标与分段筛选。' });
  log(!!window.KB && typeof window.KB.onShow === 'function', '知识库：window.KB.onShow 可用');
  if (window.KB && window.KB.onShow) {
    window.KB.onShow();
    await sleep(600);
    const btns = doc.querySelectorAll('.kb-filter-btn');
    log(btns.length === 3, '知识库：分段控件有 3 个按钮（全部 / 我的文档 / AI 记忆），实际 ' + btns.length);
    const labels = [...btns].map((b) => b.textContent.trim()).join('/');
    log(labels === '全部/我的文档/AI 记忆', '知识库：按钮文案正确（' + labels + '）');
    const badge = doc.querySelector('.kb-src-badge');
    log(!!badge, '知识库：列表项带来源徒标');
    log(!!badge && badge.textContent.includes('资料'), '知识库：普通文档的徒标是「资料」（' + (badge && badge.textContent) + '）');

    // 切到「AI 记忆」：这个用户还没有记忆，应该看到记忆专属的空态。
    const memBtn = [...btns].find((b) => b.textContent.trim() === 'AI 记忆');
    if (memBtn) {
      memBtn.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
      await sleep(500);
      const listText = (doc.getElementById('kb-doc-list') || {}).textContent || '';
      log(listText.includes('AI 记忆'), '知识库：切到 AI 记忆后显示记忆专属空态（' + JSON.stringify(listText.slice(0, 40)) + '）');
      log(memBtn.getAttribute('aria-pressed') === 'true', '知识库：选中态用 aria-pressed 标出');
      const allBtn = [...btns].find((b) => b.textContent.trim() === '全部');
      if (allBtn) {
        allBtn.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
        await sleep(500);
        const back = (doc.getElementById('kb-doc-list') || {}).textContent || '';
        log(back.includes('UI 验证用资料'), '知识库：切回全部后资料又出现了');
      }
    }
  }

  // ---------- 7. 子任务卡片（SSE running → done） ----------
  chatStub = 'subagent';
  if (input && composer) {
    input.value = '派个子任务';
    input.dispatchEvent(new window.Event('input', { bubbles: true }));
    composer.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
    await sleep(100);
    const runningCards = doc.querySelectorAll('.subagent-card');
    const runningStatus = runningCards[0] && runningCards[0].querySelector('.subagent-card-status');
    log(runningCards.length === 1, '子任务卡片：SSE subagent 事件后出现一张卡片（实际 ' + runningCards.length + '）');
    log(!!runningStatus && runningStatus.textContent === '运行中', '子任务卡片：running 状态显示「运行中」（' + (runningStatus && runningStatus.textContent) + '）');
    await sleep(350);
    const doneCards = doc.querySelectorAll('.subagent-card');
    const doneStatus = doneCards[0] && doneCards[0].querySelector('.subagent-card-status');
    log(doneCards.length === 1 && !!doneStatus && doneStatus.textContent === '完成', '子任务卡片：同 child_id 的 running→done 原地更新（卡片数 ' + doneCards.length + '，状态 ' + (doneStatus && doneStatus.textContent) + '）');
  }

  // ---------- 8. 选项式提问（need_input 带 options） ----------
  // 真实链路：ask_user 停轮 → 后端 need_input.display.options → 前端画按钮。
  // 这里用 SSE 桩直接喂一条带 options 的 need_input，不依赖模型这轮真的调 ask_user。
  chatStub = 'ask';
  if (input && composer) {
    input.value = '帮我给项目起个名字';
    input.dispatchEvent(new window.Event('input', { bubbles: true }));
    composer.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }));
    await sleep(300);
    const askOptionBtns = doc.querySelectorAll('.ask-option');
    const otherBtn = doc.querySelector('.ask-option-other');
    log(
      askOptionBtns.length === 2 && !!otherBtn && otherBtn.textContent.indexOf('其他') === 0,
      '选项式提问：need_input 带 2 个选项 → 渲染 2 个选项按钮 + 1 个「其他…」按钮（实际 ' + askOptionBtns.length + ' 个选项 / ' + (otherBtn && otherBtn.textContent) + '）'
    );

    // 记录输入框被写入的值：点选项 = 选项文本先进输入框，再走正常的发送链路。
    // 输入框实际是 textarea，从元素自己的原型上取 value 描述符，input/textarea 都适用。
    const valueDesc = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(input), 'value');
    const inputWrites = [];
    if (valueDesc && valueDesc.set) {
      Object.defineProperty(input, 'value', {
        configurable: true,
        get: function () { return valueDesc.get.call(this); },
        set: function (v) { inputWrites.push(String(v)); valueDesc.set.call(this, v); },
      });
    }
    if (askOptionBtns.length) {
      askOptionBtns[0].dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
      await sleep(300);
    }
    const sentAsUser = [...doc.querySelectorAll('.msg.user')].some(function (m) {
      return m.textContent.includes('小月');
    });
    log(
      inputWrites.indexOf('小月') !== -1 && sentAsUser,
      '选项式提问：点第一个选项后文本进入输入框并作为回答发出（输入写入 ' + JSON.stringify(inputWrites) + '，用户气泡 ' + sentAsUser + '）'
    );
  } else {
    log(false, '选项式提问：找不到输入框或表单，无法驱动');
    log(false, '选项式提问：点击选项断言未执行');
  }

  // ---------- 9. 团队面板（fetch 桩喂数据，不依赖真接口） ----------
  // 真实链路：切走再切回会话会触发 chat.js 的团队拉取；这里把 /team 的响应换成桩，
  // 验证「拉取 → TASKCARD.setTeam → DOM 出现成员行 / 任务行」整条线，以及空数据整段隐藏。
  teamStub = {
    members: [
      { name: 'lead', description: '主理人', role: 'lead', status: 'running', depth: 0, child_id: '', model: '' },
      { name: 'writer', description: '写手', role: 'teammate', status: 'inactive', depth: 1, child_id: '42', model: 'stub-model' },
    ],
    tasks: [
      { id: 'task-1', revision: 3, subject: '整理发布说明', status: 'in_progress', owner: 'writer', blocked_by: [], write_scopes: [], ready: true },
    ],
  };
  const newChatBtn = doc.getElementById('new-chat');
  const firstConvItem = () => doc.querySelector('#conv-list .conv-item');
  const switchBackToConversation = async () => {
    newChatBtn.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
    await sleep(60);
    const item = firstConvItem();
    if (item) item.dispatchEvent(new window.MouseEvent('click', { bubbles: true }));
    await sleep(500);
  };
  if (newChatBtn && firstConvItem()) {
    await switchBackToConversation();
    const memberRows = doc.querySelectorAll('.task-card-team .team-member-row');
    const taskRows = doc.querySelectorAll('.task-card-team .team-task-row');
    const leadRole = memberRows[0] && memberRows[0].querySelector('.team-member-role.is-lead');
    log(
      memberRows.length === 2 && taskRows.length === 1 &&
        !!leadRole &&
        memberRows[0].textContent.includes('lead') &&
        memberRows[1].textContent.includes('writer') &&
        taskRows[0].textContent.includes('task-1') &&
        taskRows[0].textContent.includes('整理发布说明') &&
        taskRows[0].textContent.includes('writer'),
      '团队面板：喂 roster + task 数据后卡片里出现成员行与任务行（成员 ' + memberRows.length + ' / 任务 ' + taskRows.length + '）'
    );

    teamStub = { members: [], tasks: [] };
    await switchBackToConversation();
    const teamBox = doc.querySelector('.task-card-team');
    const leftover = doc.querySelectorAll('.task-card-team .team-member-row, .task-card-team .team-task-row').length;
    log(
      !!teamBox && teamBox.hidden === true && leftover === 0,
      '团队面板：喂空数据后团队区域整段隐藏（hidden=' + (teamBox && teamBox.hidden) + '，残留行 ' + leftover + '）'
    );
  } else {
    log(false, '团队面板：找不到 #new-chat 或会话列表项，无法驱动会话切换');
    log(false, '团队面板：空数据断言未执行');
  }

  // ---------- 10. 任务卡片拖动 + 位置持久化 ----------
  // 真实链路：按住 .task-card-head 拖 → 坐标写进 localStorage['blogmy:taskcard:pos']；
  // 重新 init（相当于刷新后重建卡片）→ 坐标从 localStorage 还原。
  const dragCard = doc.getElementById('task-card');
  const dragHead = dragCard && dragCard.querySelector('.task-card-head');
  if (dragCard && dragHead) {
    const pointerLike = (type, x, y) => {
      const Ctor = typeof window.PointerEvent === 'function' ? window.PointerEvent : window.MouseEvent;
      return new Ctor(type, { bubbles: true, cancelable: true, clientX: x, clientY: y, button: 0, pointerId: 1 });
    };
    // 清掉上一条运行留下的位置，从默认位开始拖。
    window.localStorage.removeItem('blogmy:taskcard:pos');
    dragCard.style.left = '';
    dragCard.style.top = '';
    dragCard.classList.remove('is-positioned');

    const dragEvents = typeof window.PointerEvent === 'function'
      ? ['pointerdown', 'pointermove', 'pointerup']
      : ['mousedown', 'mousemove', 'mouseup'];
    dragHead.dispatchEvent(pointerLike(dragEvents[0], 400, 100));
    doc.dispatchEvent(pointerLike(dragEvents[1], 520, 180));
    doc.dispatchEvent(pointerLike(dragEvents[2], 520, 180));
    await sleep(30);

    let savedPos = null;
    try { savedPos = JSON.parse(window.localStorage.getItem('blogmy:taskcard:pos') || 'null'); } catch (e) { savedPos = null; }
    const movedOk = !!savedPos && Number.isFinite(savedPos.x) && Number.isFinite(savedPos.y) &&
      (savedPos.x !== 0 || savedPos.y !== 0) &&
      dragCard.classList.contains('is-positioned') &&
      dragCard.style.left === savedPos.x + 'px' && dragCard.style.top === savedPos.y + 'px';
    log(movedOk, '任务卡片拖动：按住头部拖动后位置改变并写进 localStorage（pos=' + JSON.stringify(savedPos) +
      '，left=' + JSON.stringify(dragCard.style.left) + '，top=' + JSON.stringify(dragCard.style.top) + '）');

    // 模拟刷新：换一个干净的新容器重新 init，位置应从 localStorage 还原。
    const freshCard = doc.createElement('aside');
    freshCard.className = 'task-card';
    freshCard.id = 'task-card';
    freshCard.hidden = true;
    dragCard.parentNode.replaceChild(freshCard, dragCard);
    window.TASKCARD.init();
    const restoredOk = freshCard.classList.contains('is-positioned') &&
      freshCard.style.left === savedPos.x + 'px' && freshCard.style.top === savedPos.y + 'px';
    log(restoredOk, '任务卡片拖动：重新 init 后位置从 localStorage 还原（left=' + JSON.stringify(freshCard.style.left) +
      '，top=' + JSON.stringify(freshCard.style.top) + '）');
  } else {
    log(false, '任务卡片拖动：找不到 #task-card 或 .task-card-head，无法驱动');
    log(false, '任务卡片拖动：重新 init 还原位置断言未执行');
  }

  console.log('\n结果: ' + pass + ' 通过 / ' + fail + ' 失败');
  console.log('测试用户: ' + user + '（记得清理）');
  dom.window.close();
  if (fail > 0) process.exitCode = 1;
}

main().catch((e) => {
  console.error('脚本异常：' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
