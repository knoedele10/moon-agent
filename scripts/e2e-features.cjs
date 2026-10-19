// 端到端验证这一轮新增的能力（多助手 / 提示词库 / 会话与消息编辑）。
//
// 跑法（服务端要先起好，且 Ollama 在跑）：
//   .\bin\blog-my.exe       # 另开一个窗口
//   node scripts/e2e-features.cjs
'use strict';

const BASE = process.env.E2E_BASE || 'http://127.0.0.1:8080';
const MODEL = process.env.E2E_MODEL || 'qwen2.5:7b';

let pass = 0;
let fail = 0;
function log(ok, msg) {
  if (ok) pass++;
  else fail++;
  console.log((ok ? '[PASS] ' : '[FAIL] ') + msg);
}

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

async function api(cookie, method, path, body) {
  const res = await fetch(BASE + path, {
    method,
    headers: { 'Content-Type': 'application/json', Cookie: cookie },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    /* 非 JSON 保留原文 */
  }
  return { status: res.status, json, text };
}

// SSE 聊天：返回解析后的事件与拼接出的回复文本。
async function chat(cookie, payload) {
  const res = await fetch(BASE + '/api/chat', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: cookie, Accept: 'text/event-stream' },
    body: JSON.stringify(Object.assign({ tools: true, model: MODEL }, payload)),
  });
  if (!res.ok) return { error: 'HTTP ' + res.status + ': ' + (await res.text()) };
  const raw = await res.text();
  const events = [];
  for (const frame of raw.split('\n\n')) {
    let name = null;
    let data = null;
    for (const line of frame.split('\n')) {
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
  return {
    events,
    reply: events.filter((e) => e.event === 'delta').map((e) => (e.data && e.data.content) || '').join(''),
    errors: events.filter((e) => e.event === 'error').map((e) => e.data),
  };
}

async function main() {
  const ts = Date.now();
  const user = 'e2efeat' + ts;
  const cookie = await register(user, 'test123456');
  console.log('测试用户: ' + user + ' / 模型: ' + MODEL + '\n');

  // ---------- 1. 提示词库 ----------
  let r = await api(cookie, 'GET', '/api/prompts');
  log(r.status === 200 && Array.isArray(r.json.prompts), '提示词：列表可读（初始 ' + (r.json && r.json.prompts ? r.json.prompts.length : '?') + ' 条）');

  r = await api(cookie, 'POST', '/api/prompts', { title: '总结要点', content: '把下面的内容总结成三条要点，每条不超过 20 字。' });
  const promptID = r.json && r.json.id;
  log(r.status === 201 && !!promptID, '提示词：新建成功 id=' + promptID);

  r = await api(cookie, 'PATCH', '/api/prompts/' + promptID, { title: '总结要点（改）' });
  log(r.status === 200, '提示词：改名成功 -> ' + r.status);

  r = await api(cookie, 'GET', '/api/prompts');
  const renamed = ((r.json && r.json.prompts) || []).find((p) => p.id === promptID);
  log(!!renamed && renamed.title === '总结要点（改）', '提示词：改名落库正确（' + (renamed && renamed.title) + '）');

  // ---------- 2. 多助手 ----------
  const SOUL = '你说话必须自称「小月」，并且每句结尾都加上「~喵」。';
  r = await api(cookie, 'POST', '/api/assistants', {
    name: '小月',
    description: '测试用助手',
    model: MODEL,
    soul_voice: SOUL,
    soul_work: '只给结论，别客套。',
  });
  const assistantID = r.json && r.json.id;
  log(r.status === 201 && !!assistantID, '助手：新建成功 id=' + assistantID);

  r = await api(cookie, 'POST', '/api/assistants', { name: '小月', description: '重名' });
  log(r.status === 400, '助手：重名被拒绝（' + r.status + '）');

  r = await api(cookie, 'GET', '/api/assistants');
  const got = ((r.json && r.json.assistants) || []).find((a) => a.id === assistantID);
  log(!!got && got.soul_voice === SOUL, '助手：列表里灵魂字段正确');

  // 用助手的灵魂聊一轮，验证覆盖真的生效
  const conv = await api(cookie, 'POST', '/api/conversations', { model: MODEL });
  const convID = conv.json && conv.json.id;
  log(!!convID, '助手：创建会话 id=' + convID);

  let c = await chat(cookie, { conversation_id: convID, content: '你好，简单介绍一下你自己。', assistant_id: assistantID });
  if (c.error) {
    log(false, '助手对话失败：' + c.error);
  } else {
    console.log('  助手回复: ' + JSON.stringify(c.reply.slice(0, 140)));
    // 注意：这里只能验证「一轮带 assistant_id 的对话能正常跑完」。灵魂是否被模型遵守取决于模型本身：
    // 实测本地 qwen2.5:7b 在**21 个工具定义**全量下发时会无视系统提示词（去掉工具或只留 3 个就遵守），
    // 这是小模型的注意力被工具模板挤掉，不是接线问题——系统提示词的内容已逐字节确认过。
    // 所以这里做成软检查；要硬验证灵魂注入，用把工具数压下去的模型（云端模型），或看 scripts/e2e-soul.cjs。
    if (c.reply.includes('小月') || c.reply.includes('喵')) {
      log(true, '助手：灵魂覆盖生效（回复带「小月」或「喵」）');
    } else {
      console.log('[SKIP] 助手：本轮模型没遵守灵魂（本地 7B 在 21 工具下会无视系统提示词，非接线问题）');
    }
  }

  // 停用助手后，列表里应当还能看到但 enabled=false
  r = await api(cookie, 'PATCH', '/api/assistants/' + assistantID, { enabled: false });
  log(r.status === 200, '助手：停用成功 -> ' + r.status);

  // ---------- 3. 会话重命名 ----------
  r = await api(cookie, 'PATCH', '/api/conversations/' + convID, { title: '被改过的标题' });
  log(r.status === 200 && r.json.title === '被改过的标题', '会话：重命名成功');

  r = await api(cookie, 'GET', '/api/conversations');
  const convNow = (r.json || []).find((x) => x.id === convID);
  log(!!convNow && convNow.title === '被改过的标题', '会话：列表里标题已更新');

  // ---------- 4. 导出 Markdown ----------
  const exp = await fetch(BASE + '/api/conversations/' + convID + '/export', { headers: { Cookie: cookie } });
  const md = await exp.text();
  log(exp.status === 200 && /text\/markdown/.test(exp.headers.get('content-type') || ''), '导出：Content-Type 正确');
  log(/attachment/i.test(exp.headers.get('content-disposition') || ''), '导出：带下载响应头');
  log(md.startsWith('# ') && md.includes('## 用户') && md.includes('## 助手'), '导出：Markdown 结构正确');
  console.log('  --- 导出片段 ---\n' + md.split('\n').slice(0, 6).join('\n') + '\n  ----------------');

  // ---------- 5. 编辑消息（改 user 消息 + 截断其后） ----------
  const msgs = await api(cookie, 'GET', '/api/conversations/' + convID + '/messages');
  const list = (msgs.json || []);
  const userMsg = list.find((m) => m.role === 'user');
  log(!!userMsg, '消息：拿得到 user 消息 id=' + (userMsg && userMsg.id));

  if (userMsg) {
    r = await api(cookie, 'PATCH', '/api/messages/' + userMsg.id, { content: '改成：你好。' });
    log(r.status === 200 && typeof r.json.removed === 'number', '消息：编辑成功并报告截断条数 removed=' + (r.json && r.json.removed));

    const after = await api(cookie, 'GET', '/api/conversations/' + convID + '/messages');
    const afterList = after.json || [];
    log(afterList.length === 1 && afterList[0].content === '改成：你好。', '消息：其后消息确实被截断（现在只剩 ' + afterList.length + ' 条）');
  }

  // ---------- 6. 重新生成 ----------
  c = await chat(cookie, { conversation_id: convID, content: '再问一句：你叫什么？', assistant_id: assistantID });
  if (c.error) {
    log(false, '重新生成前置对话失败：' + c.error);
  } else {
    const beforeCount = ((await api(cookie, 'GET', '/api/conversations/' + convID + '/messages')).json || []).length;
    const regen = await fetch(BASE + '/api/conversations/' + convID + '/regenerate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Cookie: cookie, Accept: 'text/event-stream' },
      body: JSON.stringify({ model: MODEL, assistant_id: assistantID }),
    });
    const raw = await regen.text();
    const kinds = [];
    for (const frame of raw.split('\n\n')) {
      for (const line of frame.split('\n')) if (line.startsWith('event: ')) kinds.push(line.slice(7).trim());
    }
    log(regen.status === 200 && kinds.includes('done'), '重新生成：返回 SSE 且正常收尾（事件：' + [...new Set(kinds)].join(',') + '）');

    const afterCount = ((await api(cookie, 'GET', '/api/conversations/' + convID + '/messages')).json || []).length;
    log(afterCount === beforeCount, '重新生成：消息条数没有翻倍（前 ' + beforeCount + ' / 后 ' + afterCount + '）');
  }

  // ---------- 7. 删除消息 ----------
  const finalList = (await api(cookie, 'GET', '/api/conversations/' + convID + '/messages')).json || [];
  const lastAssistant = finalList.filter((m) => m.role === 'assistant').pop();
  if (lastAssistant) {
    r = await api(cookie, 'DELETE', '/api/messages/' + lastAssistant.id);
    log(r.status === 204, '消息：单条删除返回 204');
    const nowList = (await api(cookie, 'GET', '/api/conversations/' + convID + '/messages')).json || [];
    log(nowList.length === finalList.length - 1, '消息：确实少了一条');
  }

  // ---------- 8. 清理 ----------
  await api(cookie, 'DELETE', '/api/prompts/' + promptID);
  await api(cookie, 'DELETE', '/api/assistants/' + assistantID);

  console.log('\n结果: ' + pass + ' 通过 / ' + fail + ' 失败');
  console.log('测试用户: ' + user + '（记得清理）');
}

main().catch((e) => {
  console.error('脚本异常：' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
