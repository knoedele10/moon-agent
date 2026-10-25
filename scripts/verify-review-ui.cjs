'use strict';
// 改动审阅面板：拉 unified diff、按文件列出、撤销调用 /api/agent/restore。
const fs = require('fs');
const path = require('path');
const assert = require('assert/strict');
const { JSDOM } = require('./lib/jsdom.cjs').loadJsdom();
const dir = process.env.TEMPLATES_DIR || path.join(__dirname, '../templates');
const html = fs.readFileSync(path.join(dir, 'chat/index.html'), 'utf8');
const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'outside-only', pretendToBeVisual: true });
const win = dom.window;
const doc = win.document;

const diff = [
  'diff --git a/src/a.go b/src/a.go',
  '--- a/src/a.go',
  '+++ b/src/a.go',
  '@@ -1 +1 @@',
  '-old',
  '+new',
  'diff --git a/docs/b.md b/docs/b.md',
  '--- a/docs/b.md',
  '+++ b/docs/b.md'
].join('\n');

let restoreBody = null;
const response = (body, ok = true) => ({ ok, status: ok ? 200 : 400, json: async () => body });
win.fetch = async (url, options) => {
  const u = new URL(url, 'http://localhost');
  if (u.pathname === '/api/models') return response([{ id: 'demo', label: 'Demo', has_key: true }]);
  if (u.pathname === '/api/conversations') return response([{ id: 1, title: '会话 1', model: 'demo' }]);
  if (u.pathname.endsWith('/messages')) return response({ messages: [{ id: 1, role: 'user', content: '开始' }], has_more: false, before: 1 });
  if (u.pathname === '/api/agent/diff') {
    return response({ root: 'E:/work', dirty: true, note: 'M src/a.go', diff });
  }
  if (u.pathname === '/api/agent/restore' && options && options.method === 'POST') {
    restoreBody = JSON.parse(options.body);
    return response({ content: '已恢复', is_error: false });
  }
  if (u.pathname === '/api/agent/plan') return response({ steps: [] });
  if (u.pathname.endsWith('/team') || u.pathname.endsWith('/subagents')) return response({ members: [], tasks: [] });
  return response([]);
};
for (const name of ['runtime.js', 'math.js', 'md.js', 'chat.js']) win.eval(fs.readFileSync(path.join(dir, 'static/js', name), 'utf8'));

const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check, label) {
  for (let i = 0; i < 200; i++) { if (check()) return; await pause(10); }
  throw Error('Timeout waiting for ' + (label || 'UI state'));
}

(async () => {
  await until(() => doc.querySelectorAll('.conv-item').length === 1, 'conversation list');
  assert.equal(doc.getElementById('review-panel').hidden, true, '改动面板初始应隐藏');
  assert.equal(doc.getElementById('usage-chip').hidden, true, '用量在收到事件前应隐藏');

  doc.querySelector('.conv-item').click();
  await pause(30);

  doc.getElementById('review-toggle').click();
  await until(() => doc.getElementById('review-panel').hidden === false, 'review panel open');
  await until(() => doc.querySelectorAll('.review-file').length === 2, 'two changed files');

  const paths = [...doc.querySelectorAll('.review-file-path')].map(el => el.textContent);
  assert.deepEqual(paths, ['src/a.go', 'docs/b.md']);
  assert.match(doc.getElementById('review-summary').textContent, /M src\/a\.go/);
  assert.equal(doc.getElementById('review-diff').hidden, false);
  assert.ok(doc.getElementById('review-diff').textContent.includes('+new'));

  doc.querySelectorAll('.review-undo')[0].click();
  await until(() => restoreBody !== null, 'restore request');
  assert.equal(restoreBody.path, 'src/a.go');
  assert.equal(restoreBody.session_id, '1');

  doc.getElementById('review-close').click();
  assert.equal(doc.getElementById('review-panel').hidden, true, '关闭后应隐藏');

  doc.getElementById('new-chat').click();
  await pause(20);
  assert.equal(doc.getElementById('review-panel').hidden, true);
  assert.equal(doc.getElementById('usage-chip').hidden, true);

  console.log('PASS: review panel lists changed files from diff, restores one file, closes and clears');
})().catch(error => { console.error(error); process.exitCode = 1; }).finally(() => win.close());
