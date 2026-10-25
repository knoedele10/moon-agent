'use strict';
// 任务计划面板：打开会话时从 /api/agent/plan 恢复，收起/新对话时正确清理。
const fs = require('fs');
const path = require('path');
const assert = require('assert/strict');
const { JSDOM } = require('./lib/jsdom.cjs').loadJsdom();
const dir = process.env.TEMPLATES_DIR || path.join(__dirname, '../templates');
const html = fs.readFileSync(path.join(dir, 'chat/index.html'), 'utf8');
const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'outside-only', pretendToBeVisual: true });
const win = dom.window;
const doc = win.document;

const plan = {
  explanation: '拆成三步',
  steps: [
    { step: '读代码', status: 'completed' },
    { step: '改实现', status: 'in_progress' },
    { step: '跑测试', status: 'pending' }
  ]
};
const response = body => ({ ok: true, status: 200, json: async () => body });
win.fetch = async url => {
  const u = new URL(url, 'http://localhost');
  if (u.pathname === '/api/models') return response([{ id: 'demo', label: 'Demo', has_key: true }]);
  if (u.pathname === '/api/conversations') return response([{ id: 1, title: '会话 1', model: 'demo' }]);
  if (u.pathname.endsWith('/messages')) {
    return response({ messages: [{ id: 1, role: 'user', content: '开始' }], has_more: false, before: 1 });
  }
  if (u.pathname === '/api/agent/plan') {
    if (String(u.searchParams.get('conversation_id')) !== '1') return response({ steps: [] });
    return response(plan);
  }
  if (u.pathname.endsWith('/team') || u.pathname.endsWith('/subagents')) return response({ members: [], tasks: [] });
  return response([]);
};
for (const name of ['runtime.js', 'math.js', 'md.js', 'chat.js']) win.eval(fs.readFileSync(path.join(dir, 'static/js', name), 'utf8'));
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check) {
  for (let i = 0; i < 150; i++) { if (check()) return; await pause(10); }
  throw Error('Timeout waiting for UI state');
}
(async () => {
  await until(() => doc.querySelectorAll('.conv-item').length === 1);
  assert.equal(doc.getElementById('plan-panel').hidden, true, '初始不应显示计划');

  doc.querySelector('.conv-item').click();
  await until(() => doc.getElementById('plan-panel') && !doc.getElementById('plan-panel').hidden);
  assert.equal(doc.getElementById('plan-progress').textContent, '1/3');
  const steps = [...doc.querySelectorAll('.plan-step')];
  assert.equal(steps.length, 3);
  assert.equal(steps[0].classList.contains('is-completed'), true);
  assert.equal(steps[1].classList.contains('is-in_progress'), true);
  assert.ok(steps[1].textContent.includes('改实现'));
  assert.equal(steps[2].classList.contains('is-pending'), true);

  doc.getElementById('plan-toggle').click();
  assert.equal(doc.getElementById('plan-panel').classList.contains('is-collapsed'), true);
  assert.equal(doc.getElementById('plan-toggle').getAttribute('aria-expanded'), 'false');
  doc.getElementById('plan-toggle').click();
  assert.equal(doc.getElementById('plan-panel').classList.contains('is-collapsed'), false);

  doc.getElementById('new-chat').click();
  await pause(20);
  assert.equal(doc.getElementById('plan-panel').hidden, true, '新对话应清空计划');
  console.log('PASS: plan panel restores from API, renders statuses, toggles, clears on new chat');
})().catch(error => { console.error(error); process.exitCode = 1; }).finally(() => win.close());
