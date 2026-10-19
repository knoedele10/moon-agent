'use strict';

const fs = require('fs');
const path = require('path');
const assert = require('assert/strict');

const { JSDOM } = require('./lib/jsdom.cjs').loadJsdom();

const root = path.resolve(__dirname, '..');
const templates = process.env.TEMPLATES_DIR || path.join(root, 'templates');
const html = fs.readFileSync(path.join(templates, 'chat/index.html'), 'utf8');
const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'outside-only', pretendToBeVisual: true });
const win = dom.window;
const doc = win.document;
win.TextDecoder = TextDecoder;
const history = [
  { id: 1, role: 'user', content: '读取文件' },
  { id: 2, role: 'tool_trace', content: JSON.stringify([{ name: 'read', ok: true, summary: '读取成功', detail: '文件内容示例' }]) },
  { id: 3, role: 'assistant', content: '读取完成' }
];

const response = body => ({ ok: true, status: 200, json: async () => body });
win.fetch = async url => {
  const request = new URL(url, 'http://localhost');
  if (request.pathname === '/api/chat') {
    const frame = 'event: memory\ndata: {"preview":"用户喜欢简洁答复"}\n\nevent: delta\ndata: {"content":"收到"}\n\nevent: done\ndata: {}\n\n';
    const bytes = new TextEncoder().encode(frame);
    let sent = false;
    return {
      ok: true,
      status: 200,
      headers: { get: () => 'text/event-stream' },
      body: { getReader: () => ({
        read: async () => {
          if (sent) return { done: true };
          sent = true;
          return { done: false, value: bytes };
        }
      }) }
    };
  }
  if (request.pathname === '/api/models') return response([{ id: 'demo', label: 'Demo', has_key: true }]);
  if (request.pathname === '/api/conversations') return response([{ id: 1, title: '测试会话', model: 'demo' }]);
  if (request.pathname.endsWith('/messages')) return response({ messages: history, has_more: false });
  if (request.pathname.endsWith('/team')) return response({ members: [], tasks: [] });
  if (request.pathname.endsWith('/subagents')) return response([]);
  if (request.pathname === '/api/assistants') return response({ assistants: [] });
  return response([]);
};

for (const name of ['runtime.js', 'math.js', 'md.js', 'chat.js']) {
  win.eval(fs.readFileSync(path.join(templates, 'static/js', name), 'utf8'));
}

const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check) {
  for (let i = 0; i < 150; i++) {
    if (check()) return;
    await pause(10);
  }
  throw new Error('Timeout waiting for agent activity UI');
}

(async () => {
  await until(() => doc.querySelector('.conv-item'));
  doc.querySelector('.conv-item').click();
  await until(() => doc.querySelector('.tool-cards'));

  const activity = doc.querySelector('.tool-cards');
  assert.equal(activity.open, false, 'historical process should start collapsed');
  assert.match(activity.querySelector('.tool-cards-summary').textContent, /1 个步骤/);
  activity.querySelector('.tool-cards-summary').click();
  assert.equal(activity.open, true, 'process should expand on click');

  const detail = activity.querySelector('.tool-card-detail');
  assert.ok(detail, 'full tool result should have its own disclosure');
  assert.equal(detail.open, false, 'full tool result should start collapsed');
  detail.querySelector('summary').click();
  assert.equal(detail.open, true, 'full tool result should expand on click');
  assert.equal(doc.querySelectorAll('.msg').length, 2, 'tool trace must not become a chat bubble');

  doc.getElementById('input').value = '你好';
  doc.getElementById('composer').dispatchEvent(new win.Event('submit', { bubbles: true, cancelable: true }));
  await until(() => doc.querySelectorAll('.tool-cards').length === 2 && doc.querySelectorAll('.activity-memory').length === 1);
  const liveActivity = doc.querySelectorAll('.tool-cards')[1];
  const memory = liveActivity.querySelector('.activity-memory');
  assert.equal(liveActivity.open, false, 'completed live process should auto-collapse');
  assert.equal(memory.open, false, 'memory preview should start collapsed');
  memory.querySelector('summary').click();
  assert.equal(memory.open, true, 'memory preview should expand on click');
  assert.match(memory.querySelector('.activity-memory-body').textContent, /喜欢简洁答复/);

  console.log('PASS: compact historical activity, expandable tool result, auto-collapsed live activity, expandable memory preview');
  dom.window.close();
})().catch(error => {
  console.error(error);
  dom.window.close();
  process.exitCode = 1;
});
