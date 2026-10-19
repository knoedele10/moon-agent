'use strict';

const fs = require('fs');
const path = require('path');
const assert = require('assert/strict');
const { JSDOM } = require('./lib/jsdom.cjs').loadJsdom();

const root = path.resolve(__dirname, '..');
const templates = process.env.TEMPLATES_DIR || path.join(root, 'templates');
const dom = new JSDOM(fs.readFileSync(path.join(templates, 'chat/index.html'), 'utf8'), {
  url: 'http://localhost/', runScripts: 'outside-only', pretendToBeVisual: true
});
const win = dom.window;
const doc = win.document;
win.TextDecoder = TextDecoder;
const requests = [];
const history = [{ id: 1, role: 'user', content: '检查目录' }, { id: 2, role: 'assistant', content: '已有内容' }];
const response = body => ({ ok: true, status: 200, json: async () => body });
win.fetch = async (url, options = {}) => {
  const pathname = new URL(url, 'http://localhost').pathname;
  if (pathname === '/api/chat' || pathname.endsWith('/regenerate')) {
    requests.push({ pathname, body: JSON.parse(options.body) });
    const incomplete = requests.length === 1;
    const frames = 'event: tool_call\ndata: {"id":"r1","name":"read","arguments":{}}\n\n' +
      'event: tool_result\ndata: {"id":"r1","name":"read","ok":true,"summary":"已读取","detail":"中文结果"}\n\n' +
      'event: delta\ndata: {"content":"' + (incomplete ? '已读取，剩余工作尚未完成。' : '已完成核对。') + '"}\n\n' +
      'event: done\ndata: ' + JSON.stringify(incomplete ? { incomplete: true, reason: 'execution_limit' } : {}) + '\n\n';
    const bytes = new TextEncoder().encode(frames);
    let offset = 0;
    return { ok: true, status: 200, headers: { get: () => 'text/event-stream' }, body: { getReader: () => ({
      read: async () => {
        if (offset >= bytes.length) return { done: true };
        const value = bytes.slice(offset, offset += 7);
        return { done: false, value };
      }
    }) } };
  }
  if (pathname === '/api/models') return response([{ id: 'demo', label: 'Demo', has_key: true }]);
  if (pathname === '/api/conversations') return response([{ id: 1, title: '自主执行检查', model: 'demo' }]);
  if (pathname.endsWith('/messages')) return response({ messages: history, has_more: false });
  if (pathname.endsWith('/team')) return response({ members: [], tasks: [] });
  if (pathname.endsWith('/subagents')) return response([]);
  if (pathname === '/api/assistants') return response({ assistants: [{ id: 5, name: '确认模式助手', mode: 'confirm', enabled: true }] });
  if (pathname === '/api/agent/settings') return response({ mode: 'confirm', workspace: 'E:/workspace' });
  return response([]);
};

for (const name of ['runtime.js', 'math.js', 'md.js', 'chat.js']) {
  win.eval(fs.readFileSync(path.join(templates, 'static/js', name), 'utf8'));
}
async function until(check) {
  for (let i = 0; i < 200; i++) { if (check()) return; await new Promise(resolve => setTimeout(resolve, 10)); }
  throw new Error('Timeout waiting for autonomy UI');
}

(async () => {
  await until(() => doc.querySelector('.conv-item') && doc.querySelector('#assistant-select option[value="5"]'));
  doc.querySelector('.conv-item').click();
  await until(() => doc.querySelectorAll('.msg').length === 2);
  doc.getElementById('assistant-select').value = '5';
  doc.getElementById('mode-select').value = 'auto';
  doc.getElementById('input').value = '自行处理';
  doc.getElementById('composer').dispatchEvent(new win.Event('submit', { bubbles: true, cancelable: true }));
  await until(() => requests.length === 1 && doc.querySelectorAll('.msg-actions button').length &&
    !doc.querySelectorAll('.msg-actions button')[0].disabled);
  assert.equal(requests[0].body.execution_mode, 'auto');
  assert.equal(requests[0].body.assistant_id, 5);
  const activity = doc.querySelectorAll('.tool-cards')[0];
  assert.equal(activity.getAttribute('data-incomplete'), 'true', 'incomplete metadata must reach the UI');
  assert.match(activity.querySelector('.tool-cards-summary').textContent, /已暂停/);
  assert.match(doc.getElementById('thread').textContent, /剩余工作尚未完成/);
  assert.doesNotMatch(doc.getElementById('thread').textContent, /\[出错\]/);

  doc.getElementById('mode-select').value = 'confirm';
  const regenerate = [...doc.querySelectorAll('button')].filter(button => button.title === '重新生成这条回复').pop();
  regenerate.click();
  await until(() => requests.length === 2 && !regenerate.disabled);
  assert.equal(requests[1].body.execution_mode, 'confirm', 'regeneration must use the current composer mode');
  const updated = doc.querySelector('.tool-cards');
  assert.equal(updated.getAttribute('data-incomplete'), null, 'a later successful run must not retain paused status');
  console.log('PASS: composer mode overrides preset; split UTF-8 SSE retains incomplete status; regeneration uses current mode and clears paused status');
  dom.window.close();
})().catch(error => { console.error(error); dom.window.close(); process.exitCode = 1; });
