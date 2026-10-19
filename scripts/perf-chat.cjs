// Local CPU benchmark for the real chat page handlers. No server or account needed.
// Run with: node scripts/perf-chat.cjs
'use strict';

const fs = require('fs');
const path = require('path');
const { performance } = require('perf_hooks');
const { loadJsdom } = require('./lib/jsdom.cjs');
const { JSDOM } = loadJsdom();

const root = path.resolve(__dirname, '..');
const templatesDir = process.env.TEMPLATES_DIR || path.join(root, 'templates');
const html = fs.readFileSync(path.join(templatesDir, 'chat/index.html'), 'utf8');
const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'outside-only', pretendToBeVisual: true });
const win = dom.window;
const conversations = Array.from({ length: 160 }, (_, n) => ({ id: n + 1, title: '会话 ' + (n + 1), model: 'demo' }));
const answer = ('一段较长的回复，包含 **加粗**、列表和代码。\n\n- 第一项\n- 第二项\n\n').repeat(18);
const history = Array.from({ length: 180 }, (_, n) => ({ id: n + 1, role: n % 2 ? 'assistant' : 'user', content: answer }));
const chunks = Array.from({ length: 300 }, (_, n) => '第' + n + '段 ' + answer.slice(0, 30));
let messageRequests = 0;
let markdownCalls = 0;
let messageDelay = 0;

function response(data) {
  return { ok: true, status: 200, json: async () => data };
}
win.fetch = async (url) => {
  const target = String(url);
  if (target === '/api/models') return response([{ id: 'demo', label: 'Demo', has_key: true }]);
  if (target === '/api/conversations') return response(conversations);
  if (/^\/api\/conversations\/\d+\/messages/.test(target)) {
    messageRequests++;
    if (messageDelay) await new Promise(resolve => setTimeout(resolve, messageDelay));
    return response(history);
  }
  if (/^\/api\/conversations\/\d+\/team/.test(target)) return response({ members: [], tasks: [] });
  if (/^\/api\/conversations\/\d+\/subagents/.test(target)) return response([]);
  if (target === '/api/assistants') return response({ assistants: [] });
  if (target === '/api/chat') {
    const encoder = new TextEncoder();
    let index = 0;
    return {
      ok: true,
      status: 200,
      headers: { get: () => 'text/event-stream' },
      body: { getReader: () => ({ read: async () => {
        if (index >= chunks.length) return { done: true };
        return { done: false, value: encoder.encode('event: delta\ndata: ' + JSON.stringify({ content: chunks[index++] }) + '\n\n') };
      } }) }
    };
  }
  return response([]);
};

for (const file of ['math.js', 'md.js', 'chat.js']) {
  win.eval(fs.readFileSync(path.join(templatesDir, 'static/js', file), 'utf8'));
}
const originalRender = win.MD.render;
win.MD.render = function (value) { markdownCalls++; return originalRender(value); };

async function until(check) {
  for (let i = 0; i < 1000; i++) {
    if (check()) return;
    await new Promise(resolve => setTimeout(resolve, 2));
  }
  throw new Error('UI benchmark timed out');
}

(async () => {
  await until(() => win.document.querySelectorAll('.conv-item').length === conversations.length);
  const startHistory = performance.now();
  win.document.querySelector('.conv-item').click();
  await until(() => win.document.querySelectorAll('.msg').length === history.length);
  const historyMs = performance.now() - startHistory;

  // Switch away and back; this catches repeated history fetches and rebuilding.
  win.location.hash = '#/models';
  await until(() => win.document.querySelector('.app').getAttribute('data-view') === 'models');
  win.location.hash = '#/chat';
  await until(() => win.document.querySelector('.app').getAttribute('data-view') === 'chat');
  win.document.querySelectorAll('.conv-item')[1].click();
  await until(() => win.document.querySelectorAll('.msg').length === history.length);
  messageDelay = 80;
  const startReopen = performance.now();
  win.document.querySelectorAll('.conv-item')[0].click();
  await until(() => win.document.querySelectorAll('.msg').length === history.length);
  const reopenMs = performance.now() - startReopen;
  if (win.document.querySelectorAll('.conv-item.active').length !== 1 ||
      win.document.querySelector('.conv-item.active').textContent.indexOf('会话 1') === -1) {
    throw new Error('切换会话后侧栏选中状态错误');
  }
  await new Promise(resolve => setTimeout(resolve, 90));
  messageDelay = 0;

  const beforeStream = markdownCalls;
  const input = win.document.getElementById('input');
  input.value = '测试流式回复';
  const startStream = performance.now();
  win.document.getElementById('send-btn').click();
  await until(() => !win.document.getElementById('send-btn').classList.contains('is-stop'));
  const streamMs = performance.now() - startStream;
  const assistantRows = win.document.querySelectorAll('.msg.assistant');
  if (!assistantRows[assistantRows.length - 1].textContent.includes('第299段')) throw new Error('流式回复缺少最后一段');
  process.stdout.write(JSON.stringify({ historyMs: Math.round(historyMs), reopenMs: Math.round(reopenMs), streamMs: Math.round(streamMs), markdownCallsDuringStream: markdownCalls - beforeStream, messageRequests }) + '\n');
  win.close();
})().catch(err => { console.error(err); win.close(); process.exitCode = 1; });
