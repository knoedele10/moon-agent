// 端到端验证这一轮从 Open WebUI 借鉴过来的四项能力：
//   1. 上下文压缩（context compaction）
//   2. 风险操作确认（need_input 带 confirm 标记）
//   3. ask_user 的候选项（need_input 带 display.options）
//   4. skill_write（把流程沉淀成技能）
//
// 跑法（服务端要用低阈值启动，否则压缩那条要聊几十轮才触发）：
//   $env:AGENT_COMPACT_TRIGGER='6'; $env:AGENT_COMPACT_KEEP='2'
//   $env:AGENT_MODE='confirm'; .\bin\blog-my.exe
//   node scripts/e2e-upgrade.cjs
'use strict';

const path = require('path');
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

async function chat(cookie, { conversationId, content }) {
  const res = await fetch(BASE + '/api/chat', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: cookie, Accept: 'text/event-stream' },
    body: JSON.stringify({
      conversation_id: conversationId || 0,
      model: MODEL,
      content,
      tools: true,
    }),
  });
  if (!res.ok) return { error: 'HTTP ' + res.status + ': ' + (await res.text()) };
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
  const pick = (k) => events.filter((e) => e.event === k).map((e) => e.data);
  return {
    events,
    reply: pick('delta').map((d) => (d && d.content) || '').join(''),
    needInput: pick('need_input'),
    toolResults: pick('tool_result'),
    compacts: pick('compact'),
    errors: pick('error'),
  };
}

async function main() {
  const ts = Date.now();
  const user = 'e2eupg' + ts;
  const cookie = await register(user, 'test123456');
  const ws = path.resolve(__dirname, '..', 'workspace', 'e2e-upgrade-' + ts);
  console.log('测试用户: ' + user + ' / 模型: ' + MODEL + ' / 工作区: ' + ws);

  // 工作区 + confirm 档
  let r = await api(cookie, 'PUT', '/api/agent/settings', { workspace: ws, mode: 'confirm' });

  // 全程用同一个会话：确认放行依赖“上一条 assistant 消息就是确认请求”，
  // 换会话就查不到那条历史了；而且共用会话才能把压缩聊到触发阈值。
  const convRes = await api(cookie, 'POST', '/api/conversations', { model: MODEL });
  const convID = convRes.json && convRes.json.id;
  log(!!convID, '创建会话 id=' + convID);
  log(r.status === 200, '设置工作区与 confirm 档 -> ' + r.status);
  r = await api(cookie, 'GET', '/api/agent/settings');
  log(r.json && r.json.mode === 'confirm', '档位读回 confirm（实际 ' + (r.json && r.json.mode) + '）');

  // ---- 1. 风险操作确认 ----
  let c = await chat(cookie, {
    conversationId: convID,
    content: '请在工作区里新建一个文件 hello.txt，内容写 "hi"。用 write 工具直接做。',
  });
  if (c.error) {
    log(false, '确认测试那轮失败：' + c.error);
  } else {
    const ni = c.needInput[0];
    log(!!ni, '风险操作：模型确实停下来等确认（need_input 事件）');
    if (ni) {
      log(ni.confirm === true, 'need_input 带 confirm=true（前端据此画确认/取消按钮）');
      console.log('  确认问题: ' + String(ni.question || '').split('\n')[0]);
      console.log('  确认问题全文: ' + JSON.stringify(ni.question));
    }
  }

  // ---- 2. 确认后真的执行 ----
  c = await chat(cookie, { conversationId: convID, content: '确认' });
  if (c.error) {
    log(false, '确认后那轮失败：' + c.error);
  } else {
    console.log('  确认后模型回复: ' + JSON.stringify(c.reply.slice(0, 200)));
    console.log('  确认后事件: ' + c.events.map((e) => e.event).join(','));
    // 这里断言的是「接线」而不是「模型质量」：确认后工具被真的拉起执行就算过。
    // 工具本身因模型给错参数而失败（7B 常把 path/content 写成别的字段名）属于模型问题，
    // 而且这种错误恰恰证明「存下来的参数被原样重放了」。
    const ran = c.toolResults.some((t) => t && t.name === 'write');
    log(ran, '回复「确认」后已批准的调用被真的执行（不看成败）');
    const w0 = c.toolResults.find((t) => t && t.name === 'write');
    if (w0) console.log('  执行结果: ' + w0.summary);
  }

  // ---- 3. ask_user 的候选项 ----
  c = await chat(cookie, {
    conversationId: convID,
    content: '我想给项目起个名字但拿不定主意。请用 ask_user 工具问我，给我两个候选名让我选，别自己定。',
  });
  if (c.error) {
    log(false, 'ask_user 那轮失败：' + c.error);
  } else {
    const ni = c.needInput[0];
    const opts = ni && ni.display && ni.display.options;
    if (ni && ni.tool === 'ask_user') {
      console.log('  ask_user 事件: ' + JSON.stringify(ni));
      log(Array.isArray(opts) && opts.length >= 2, 'ask_user 带了 2 个以上候选项（界面会画成按钮）');
      if (opts) console.log('  候选项: ' + JSON.stringify(opts));
    } else {
      // 软检查：调不调工具取决于模型听不听话，7B 常常不调，不该算接线失败。
      console.log('[SKIP] 模型这轮没调用 ask_user（模型行为，非接线问题）：' + JSON.stringify(ni || null));
    }
  }

  // ---- 4. skill_write ----
  await api(cookie, 'PUT', '/api/agent/settings', { mode: 'auto' });
  c = await chat(cookie, {
    conversationId: convID,
    content: '把「在工作区里新建 hello.txt 并写入 hi」这套流程用 skill_write 存成一个技能，名字叫 create-hello-file。',
  });
  if (c.error) {
    log(false, 'skill_write 那轮失败：' + c.error);
  } else {
    const sw = c.toolResults.find((t) => t && t.name === 'skill_write');
    console.log('  skill_write 轮回复: ' + JSON.stringify(c.reply.slice(0, 200)));
    console.log('  skill_write 轮事件: ' + c.events.map((e) => e.event).join(','));
    if (sw) log(sw.ok, 'skill_write 执行成功：' + sw.summary);
    else console.log('[SKIP] 模型这轮没调用 skill_write（模型行为，非接线问题）');
  }
  const skills = await api(cookie, 'GET', '/api/skills');
  const made = ((skills.json && skills.json.skills) || []).find((s) => s.name === 'create-hello-file');
  if (made) log(true, '技能库里能查到新建的技能（id=' + made.id + '）');

  // ---- 5. 上下文压缩 ----
  // 服务端 AGENT_COMPACT_TRIGGER=6 / KEEP=2 时，聊到第 4 轮左右就该触发。
  let sawCompact = false;
  for (let i = 1; i <= 5; i++) {
    c = await chat(cookie, { conversationId: convID, content: '第 ' + i + ' 句闲聊：今天天气不错，随便聊两句就行。' });
    if (c.error) {
      log(false, '压缩测试第 ' + i + ' 轮失败：' + c.error);
      break;
    }
    if (c.compacts.length) {
      sawCompact = true;
      console.log('  第 ' + i + ' 轮收到 compact 事件，摘要开头: ' + String(c.compacts[0].summary || '').slice(0, 60));
      break;
    }
  }
  log(sawCompact, '上下文压缩：收到 compact 事件（历史被压成摘要）');

  console.log('\n结果: ' + pass + ' 通过 / ' + fail + ' 失败');
  console.log('测试用户: ' + user + '（记得清理）');
}

main().catch((e) => {
  console.error('脚本异常：' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
