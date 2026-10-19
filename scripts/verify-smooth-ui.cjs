'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert/strict');
const { JSDOM } = require('./lib/jsdom.cjs').loadJsdom();
const dir = process.env.TEMPLATES_DIR || path.join(__dirname, '../templates');
const html = fs.readFileSync(path.join(dir, 'chat/index.html'), 'utf8');
const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'outside-only', pretendToBeVisual: true });
const win = dom.window;
const doc = win.document;
let hidden = false;
Object.defineProperty(doc, 'hidden', { get: () => hidden });
const rows = Array.from({ length: 185 }, (_, n) => ({ id: n + 1, role: n % 2 ? 'assistant' : 'user', content: '消息 ' + (n + 1) }));
const seen = [];
let releaseSlow;
const response = body => ({ ok: true, status: 200, json: async () => body });
win.fetch = async url => {
  const u = new URL(url, 'http://localhost');
  seen.push(u.pathname + u.search);
  if (u.pathname === '/api/models') return response([{ id: 'demo', label: 'Demo', has_key: true }]);
  if (u.pathname === '/api/conversations') return response([1, 2, 3].map(id => ({ id, title: '会话 ' + id, model: 'demo' })));
  if (u.pathname.endsWith('/messages')) {
    if (u.pathname.includes('/2/')) await new Promise(resolve => { releaseSlow = resolve; });
    const before = Number(u.searchParams.get('before')) || Infinity;
    const eligible = rows.filter(row => row.id < before);
    const limit = Number(u.searchParams.get('limit')) || eligible.length;
    const messages = eligible.slice(-limit);
    return response({ messages, has_more: eligible.length > limit, before: messages[0]?.id || 0 });
  }
  if (u.pathname.endsWith('/team')) return response({ members: [], tasks: [] });
  if (u.pathname.endsWith('/subagents')) return response([]);
  if (u.pathname === '/api/assistants') return response({ assistants: [] });
  return response([]);
};
for (const name of ['runtime.js', 'math.js', 'md.js', 'chat.js']) win.eval(fs.readFileSync(path.join(dir, 'static/js', name), 'utf8'));
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check) {
  for (let i = 0; i < 150; i++) { if (check()) return; await pause(10); }
  throw Error('Timeout waiting for UI state');
}
(async () => {
  await until(() => doc.querySelectorAll('.conv-item').length === 3);
  doc.querySelector('.conv-item').click();
  await until(() => doc.querySelectorAll('.msg').length === 60);
  assert.equal(doc.querySelector('.msg').getAttribute('data-message-id'), '126');
  assert.ok(seen.includes('/api/conversations/1/messages?limit=60'));
  for (const count of [120, 180, 185]) {
    doc.querySelector('.history-older').click();
    await until(() => doc.querySelectorAll('.msg').length === count);
  }
  assert.equal(doc.querySelector('.history-older'), null);
  assert.equal(doc.querySelector('.msg').getAttribute('data-message-id'), '1');
  assert.equal(new Set([...doc.querySelectorAll('.msg')].map(node => node.dataset.messageId)).size, 185);
  doc.querySelectorAll('.conv-item')[1].click();
  await until(() => releaseSlow);
  doc.querySelectorAll('.conv-item')[2].click();
  await until(() => doc.querySelectorAll('.msg').length === 60);
  releaseSlow();
  await pause(40);
  assert.equal(doc.querySelector('.conv-item.active').textContent.includes('会话 3'), true);
  assert.equal(doc.querySelectorAll('.msg').length, 60);
  doc.querySelectorAll('.conv-item')[0].click();
  await until(() => doc.querySelectorAll('.msg').length === 185);
  assert.equal(doc.querySelector('.history-older'), null);

  let runs = 0, inFlight = 0, maxInFlight = 0, releaseJob;
  const job = win.APP_RUNTIME.every(async () => {
    runs++; inFlight++; maxInFlight = Math.max(maxInFlight, inFlight);
    await new Promise(resolve => { releaseJob = resolve; });
    inFlight--;
  }, 1);
  win.APP_RUNTIME.wake();
  await until(() => runs === 1);
  await pause(550);
  assert.equal(runs, 1, 'slow jobs must not overlap');
  hidden = true;
  doc.dispatchEvent(new win.Event('visibilitychange'));
  releaseJob();
  await pause(550);
  assert.equal(runs, 1, 'hidden windows must not poll');
  hidden = false;
  doc.dispatchEvent(new win.Event('visibilitychange'));
  await until(() => runs === 2);
  releaseJob();
  win.APP_RUNTIME.stop(job);
  assert.equal(maxInFlight, 1);
  assert.ok(win.APP_PERF.snapshot().samples.some(sample => sample.kind === 'request'));
  console.log('PASS: cursor pages, no duplicate rows, late response isolation, cached history, hidden pause, resume, no overlapping polls, local metrics');
})().catch(error => { console.error(error); process.exitCode = 1; }).finally(() => win.close());
