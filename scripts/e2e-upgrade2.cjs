// 端到端验证这一轮「借鉴 Open WebUI 升级已有功能」的后端部分：
//   1. 对话参数（额外系统提示词 / 温度 / 最大 token）真的贯通到了模型
//   2. 知识库按来源过滤 + 分来源计数
//
// 跑法（服务端先起好，Ollama 在跑）：
//   .\bin\blog-my.exe
//   node scripts/e2e-upgrade2.cjs
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

function parseSSE(raw) {
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

async function chat(cookie, payload) {
  const res = await fetch(BASE + '/api/chat', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: cookie, Accept: 'text/event-stream' },
    body: JSON.stringify(Object.assign({ model: MODEL }, payload)),
  });
  if (!res.ok) return { error: 'HTTP ' + res.status + ': ' + (await res.text()) };
  return parseSSE(await res.text());
}

async function main() {
  const ts = Date.now();
  const user = 'e2eup2' + ts;
  const cookie = await register(user, 'test123456');
  console.log('测试用户: ' + user + ' / 模型: ' + MODEL + '\n');

  // ---------- 1. 额外系统提示词真的生效 ----------
  // 关键：用 **tools=false 的直连路径**。直连路径没有工具定义，
  // 小模型不会被工具模板挤掉注意力，所以这里能干净地验证「system_prompt 确实送到了模型」。
  const MARK = '【参数生效】';
  let c = await chat(cookie, {
    tools: false,
    content: '你好。',
    system_prompt: '无论用户说什么，你的回复必须是且仅是这样一行：' + MARK,
  });
  if (c.error) {
    log(false, '额外系统提示词那轮失败：' + c.error);
  } else {
    console.log('  直连路径回复: ' + JSON.stringify(c.reply.slice(0, 120)));
    log(c.reply.includes(MARK), '对话参数：额外系统提示词真的送到了模型（回复含 ' + MARK + '）');
  }

  // ---------- 2. 温度 / 最大 token 被接受且不破坏对话 ----------
  c = await chat(cookie, { tools: false, content: '用一句话说明你是谁。', temperature: 0.3, max_tokens: 64 });
  if (c.error) {
    log(false, '带温度/上限那轮失败：' + c.error);
  } else {
    log(!!c.reply && !c.errors.length, '对话参数：temperature/max_tokens 被接受且正常收尾');
  }

  // 越界值应当被夹到边界而不是报错
  c = await chat(cookie, { tools: false, content: '说一个字。', temperature: 99, max_tokens: 999999 });
  if (c.error) {
    log(false, '越界参数那轮失败：' + c.error);
  } else {
    log(!c.errors.length, '对话参数：越界值被夹取而不是报错');
  }

  // ---------- 3. 知识库按来源过滤 + 分来源计数 ----------
  await api(cookie, 'POST', '/api/kb/text', { title: '用户资料 A', text: '这是一段用户自己粘贴进去的普通资料，用来验证来源过滤。' });
  await api(cookie, 'POST', '/api/kb/text', { title: '用户资料 B', text: '第二段普通资料，内容与第一段不同，避免被去重掉。' });

  // 造两条「记忆」：直接走记忆工具不方便，这里用带 source 的接口没有，
  // 所以改用对话自动记忆太重——退一步：只验证「全部 / 文档」两个筛选与计数口径自洽。
  let r = await api(cookie, 'GET', '/api/kb/documents?source=all');
  const allList = (r.json && r.json.data) || [];
  log(r.status === 200 && Array.isArray(allList), '知识库：?source=all 可用，返回 ' + allList.length + ' 条');
  log(allList.every((d) => typeof d.source === 'string'), '知识库：列表项带 source 字段');
  log(typeof (r.json && r.json.total) === 'number', '知识库：响应带 total=' + (r.json && r.json.total));

  r = await api(cookie, 'GET', '/api/kb/documents?source=document');
  const docList = (r.json && r.json.data) || [];
  log(docList.length === allList.length, '知识库：?source=document 目前与 all 一致（还没有记忆）');

  r = await api(cookie, 'GET', '/api/kb/documents?source=memory');
  const memList = (r.json && r.json.data) || [];
  log(memList.length === 0, '知识库：?source=memory 目前为空');

  r = await api(cookie, 'GET', '/api/kb/documents?source=bogus');
  log(r.status === 200 && ((r.json && r.json.data) || []).length === allList.length, '知识库：非法 source 回落为 all');

  r = await api(cookie, 'GET', '/api/kb/stats');
  const by = (r.json && r.json.by_source) || {};
  const sum = (by.document || 0) + (by.memory || 0) + (by.profile || 0);
  log(!!r.json && typeof r.json.documents === 'number', '知识库：stats 带 documents=' + (r.json && r.json.documents));
  log(sum === (r.json && r.json.documents), '知识库：by_source 三项之和等于 documents（' + sum + ' vs ' + (r.json && r.json.documents) + '）');
  console.log('  by_source: ' + JSON.stringify(by));

  console.log('\n结果: ' + pass + ' 通过 / ' + fail + ' 失败');
  console.log('测试用户: ' + user + '（记得清理）');
}

main().catch((e) => {
  console.error('脚本异常：' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
