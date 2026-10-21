'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert/strict');
const { JSDOM } = require('./lib/jsdom.cjs').loadJsdom();
const dir = process.env.TEMPLATES_DIR || path.join(__dirname, '../templates');
const dom = new JSDOM(fs.readFileSync(path.join(dir, 'chat/index.html'), 'utf8'), {
  url: 'http://localhost/', runScripts: 'outside-only', pretendToBeVisual: true
});
const win = dom.window;
const doc = win.document;
const calls = [];
let enabled = true;
let reloads = 0;
let failure = false;
win.SKILLS = { onShow: async () => { reloads++; } };
win.fetch = async (url, options) => {
  calls.push({ url, body: options.body && JSON.parse(options.body) });
  if (failure) return { ok: false, json: async () => ({ error: '记录服务暂不可用' }) };
  let body = {};
  if (url.endsWith('/evolution')) body = {
    enabled, can_apply: enabled, mode: enabled ? 'auto' : 'off',
    report: { runs: 3, skills: 1, candidates: [{ description: '<img src=x onerror=alert(1)>', auto_applicable: true }] }
  };
  if (url.endsWith('/tool-catalog')) body = { disclosure: 'search', spill: 32768 };
  return { ok: true, json: async () => body };
};
win.eval(fs.readFileSync(path.join(dir, 'static/js/evolution.js'), 'utf8'));
const byId = id => doc.getElementById(id);
async function settled(check) {
  for (let i = 0; i < 80; i++) {
    if (check()) return;
    await new Promise(resolve => setTimeout(resolve, 5));
  }
  throw new Error('学习面板操作没有完成');
}

(async () => {
  try {
    byId('evolution-panel').open = true;
    await settled(() => byId('evolution-report').textContent.includes('已回看 3') && !byId('evolution-refresh').disabled);
    assert.equal(calls.filter(c => c.body !== undefined).length, 0, '打开面板不应修改技能');
    assert.equal(byId('evolution-report').querySelector('img'), null, '候选内容必须作为文本显示');
    byId('evolution-review').click();
    byId('evolution-review').click();
    await settled(() => !byId('evolution-refresh').disabled);
    assert.equal(calls.filter(c => c.url.endsWith('/curate')).length, 1, '重复点击不能提交两次维护请求');
    assert.equal(calls.find(c => c.url.endsWith('/curate')).body.apply, false);
    byId('evolution-apply').click();
    await settled(() => reloads === 1 && !byId('evolution-refresh').disabled);
    assert.equal(calls.filter(c => c.url.endsWith('/curate')).at(-1).body.apply, true);
    byId('evolution-rollback').click();
    await settled(() => reloads === 2 && byId('evolution-status').textContent.includes('已撤销'));
    enabled = false;
    byId('evolution-refresh').click();
    await settled(() => byId('evolution-status').textContent.includes('未启用') && !byId('evolution-refresh').disabled);
    assert.equal(byId('evolution-apply').hidden, true);
    assert.equal(byId('evolution-rollback').hidden, true);
    assert.equal(byId('evolution-review').disabled, true);
    failure = true;
    byId('evolution-refresh').click();
    await settled(() => byId('evolution-status').textContent.includes('暂不可用'));
    console.log('PASS: learning report is read-only, safe text, serialized mutations, refreshed skills, disabled mode, visible errors');
  } finally { win.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
