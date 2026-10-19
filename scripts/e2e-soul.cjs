// 端到端验证：灵魂是否真的被模型遵守 + 记忆是否自动写入/自动回忆 + 用户画像是否自动生成。
//
// 这是一条**需要活服务**的手工验证链路，不是单元测试：它要真的起后端、真的调模型。
// 跑法（先在另一个窗口把服务起好）：
//   node scripts/e2e-soul.cjs
// 依赖：
//   - 后端已在 127.0.0.1:8080 运行（E2E_BASE 可改）；
//   - Ollama 已启动，且拉过 qwen2.5:7b（E2E_MODEL 可换成别的本地模型）。
// 想连画像链路一起验，用低阈值起服务：
//   $env:AGENT_PROFILE_EVERY_TURNS='3'; .\bin\blog-my.exe
//
// 会注册一个 e2esoul<时间戳> 的测试账号，跑完记得清理（见文末 SQL 提示）。
'use strict';

const BASE = process.env.E2E_BASE || 'http://127.0.0.1:8080';
const MODEL = process.env.E2E_MODEL || 'qwen2.5:7b';

const SOUL_VOICE = '你说话必须自称「小月」，并且每一句回复的结尾都要加上「~喵」。';
const SOUL_WORK = '回答尽量简短，直接给结论，不要客套。';

const FACT_STATEMENT =
  '记住一件事：我的项目叫 blog-my，后端用 Go 写，数据库是 MySQL 5.7，前端是普通的 HTML 加 JavaScript。';

function log(ok, msg) {
  console.log((ok ? '[PASS] ' : '[FAIL] ') + msg);
}

async function register(username, password) {
  const res = await fetch(BASE + '/register', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ username, password }).toString(),
    redirect: 'manual',
  });
  const setCookies = res.headers.getSetCookie ? res.headers.getSetCookie() : [];
  const cookie = setCookies.map((c) => c.split(';')[0]).join('; ');
  if (!cookie) throw new Error('注册后没拿到 session cookie（status=' + res.status + '）');
  return cookie;
}

async function api(cookie, method, path, body) {
  const res = await fetch(BASE + path, {
    method,
    headers: {
      'Content-Type': 'application/json',
      Cookie: cookie,
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    /* 非 JSON 就保留原文 */
  }
  return { status: res.status, json, text };
}

// 发一条消息，收集 SSE 事件，返回 { events, reply, memoryEvents, error }
async function chat(cookie, { conversationId, content }) {
  const res = await fetch(BASE + '/api/chat', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Cookie: cookie,
      Accept: 'text/event-stream',
    },
    body: JSON.stringify({
      conversation_id: conversationId || 0,
      model: MODEL,
      content,
      tools: true,
    }),
  });
  if (!res.ok) {
    return { error: 'HTTP ' + res.status + ': ' + (await res.text()) };
  }
  const raw = await res.text();
  const events = [];
  for (const frame of raw.split('\n\n')) {
    const lines = frame.split('\n');
    let name = null;
    let data = null;
    for (const line of lines) {
      if (line.startsWith('event: ')) name = line.slice(7).trim();
      else if (line.startsWith('data: ')) data = line.slice(6);
    }
    if (name && data !== null) {
      let parsed = null;
      try {
        parsed = JSON.parse(data);
      } catch {
        parsed = data;
      }
      events.push({ event: name, data: parsed });
    }
  }
  const reply = events
    .filter((e) => e.event === 'delta')
    .map((e) => (e.data && e.data.content) || '')
    .join('');
  return {
    events,
    reply,
    memoryEvents: events.filter((e) => e.event === 'memory').map((e) => e.data),
    errorEvent: events.find((e) => e.event === 'error'),
  };
}

async function main() {
  const ts = Date.now();
  const user = 'e2esoul' + ts;
  const pass = 'test123456';
  console.log('测试用户: ' + user + ' / 模型: ' + MODEL);

  const cookie = await register(user, pass);
  console.log('已注册并拿到会话。');

  // --- 1. 写灵魂 ---
  // 这里刻意只发 soul_voice/soul_work：设置页的「保存灵魂」就是这么发的。
  // 后端曾要求必须带 workspace/mode，导致这一步直接 400、灵魂根本存不进去。
  let r = await api(cookie, 'PUT', '/api/agent/settings', {
    soul_voice: SOUL_VOICE,
    soul_work: SOUL_WORK,
  });
  log(r.status === 200, 'PUT /api/agent/settings -> ' + r.status);

  r = await api(cookie, 'GET', '/api/agent/settings');
  const gotVoice = r.json && r.json.soul_voice;
  const gotWork = r.json && r.json.soul_work;
  log(gotVoice === SOUL_VOICE && gotWork === SOUL_WORK, '读回灵魂与写入一致（voice="' + gotVoice + '"）');

  // --- 2. 灵魂是否被遵守 ---
  let c = await chat(cookie, { content: '你好，简单介绍一下你自己。' });
  if (c.error) {
    log(false, '首轮对话失败：' + c.error);
    return;
  }
  console.log('\n--- 首轮回复 ---\n' + c.reply + '\n----------------');
  const obeysName = c.reply.includes('小月');
  const obeysTic = c.reply.includes('喵');
  log(obeysName, '灵魂-自称：回复里出现「小月」 -> ' + obeysName);
  log(obeysTic, '灵魂-口头禅：回复里出现「喵」 -> ' + obeysTic);

  // --- 3. 记忆自动写入 ---
  c = await chat(cookie, { content: FACT_STATEMENT });
  if (c.error) {
    log(false, '写记忆那轮对话失败：' + c.error);
    return;
  }
  const saveWarn = c.memoryEvents.find((m) => m && m.saved === false);
  log(!saveWarn, '写记忆轮：无「未保存」告警' + (saveWarn ? '（' + JSON.stringify(saveWarn) + '）' : ''));
  console.log('写记忆轮的 memory 事件: ' + JSON.stringify(c.memoryEvents));

  // --- 4. 记忆自动回忆 ---
  c = await chat(cookie, { content: '我的项目叫什么名字？后端用什么语言写的？' });
  if (c.error) {
    log(false, '回忆那轮对话失败：' + c.error);
    return;
  }
  console.log('\n--- 回忆轮回复 ---\n' + c.reply + '\n----------------');
  console.log('回忆轮的 memory 事件: ' + JSON.stringify(c.memoryEvents));
  const recalled = c.memoryEvents.some((m) => m && m.preview && m.preview.includes('blog-my'));
  log(recalled, '记忆注入：本轮 memory 事件里带回了 blog-my');
  const answerHasFact = c.reply.includes('blog-my') && /Go/i.test(c.reply);
  log(answerHasFact, '记忆生效：模型答出项目名 blog-my 与语言 Go');
  const noDegrade = !c.memoryEvents.some((m) => m && m.degraded);
  log(noDegrade, '记忆未降级（检索链路正常）');

  // --- 5. 用户画像：攒够轮数后后台自动生成 ---
  // 服务端若开了 AGENT_PROFILE_EVERY_TURNS=3，到第 3 轮就会触发。多聊两句让画像有料。
  await chat(cookie, { content: '我平时喜欢用中文交流，讨厌啰嗦的解释，给我结论就行。' });
  await chat(cookie, { content: '我现在最想做的事，是把这个桌面助手打磨成真正好用的工具。' });

  let profile = null;
  for (let i = 0; i < 20; i++) {
    const docs = await api(cookie, 'GET', '/api/kb/documents');
    const items = (docs.json && docs.json.data) || [];
    profile = items.find((d) => d.title === '用户画像');
    if (profile) break;
    await new Promise((r) => setTimeout(r, 2000));
  }
  log(!!profile, '用户画像：已自动生成（title=用户画像）' + (profile ? ' id=' + profile.id : ' 未生成'));
  if (profile) {
    const full = await api(cookie, 'GET', '/api/kb/documents/' + profile.id);
    const d = (full.json && full.json.data) || {};
    const chunks = d.chunks || [];
    const body = d.content || chunks.map((c) => c.text).join('\n');
    console.log('\n--- 画像正文 ---\n' + body.slice(0, 600) + '\n----------------');
    log(body.includes('Go') || body.includes('blog-my'), '画像内容：覆盖到用户的关键信息');
  }

  console.log('\n测试用户ID前缀: ' + user);
  console.log('清理提示：按前缀删除该账号的 user / agent_setting / kb_document / kb_chunk /');
  console.log('          kb_memory_event / conversation / message / user_key 各行即可。');
}

main().catch((e) => {
  console.error('脚本异常：' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
