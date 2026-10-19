#!/usr/bin/env node
// Day 23 验收（前端）：用真实 Chromium 走一遍 templates/chat/index.html 的知识库视图。
//
// 为什么单独有这个脚本：scripts/verify-rag.mjs 验的是 /api/kb/* 的 HTTP 契约，
// 它证明不了"前端真的接上了"。本脚本用 Playwright 打开真实页面、点真实按钮，
// 覆盖三条用户路径：上传入库 / 检索 / 基于知识库问答（SSE），外加图片入库与删除。
//
// 用法：
//   node docs/day23/verify-ui.mjs --base http://127.0.0.1:8300 \
//        --log docs/day23/ui-verify-log.txt --transcript docs/day23/ui-transcript.json \
//        --screenshots docs/day23/screenshots
//
// 依赖：本机已装 playwright-core 1.60.0（随 openclaw 一起安装）与 ms-playwright 的 chromium-1223。
//   脚本按固定路径 require 它，找不到就在开跑前直接报错退出（不会伪装成"通过"）。
//
// 判定口径同 verify-rag.mjs：PASS / FAIL / WARN；有 FAIL 退出码为 1。
// WARN 用于"模型输出不可预测"的软断言（例如回答里是否逐字包含暗号词）。

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';
import { createRequire } from 'node:module';

// ---------- 参数 ----------
const args = {};
const BOOL_FLAGS = new Set(['skip-image', 'help']);
for (let i = 2; i < process.argv.length; i++) {
  const a = process.argv[i];
  if (!a.startsWith('--')) continue;
  const key = a.slice(2);
  const next = process.argv[i + 1];
  if (BOOL_FLAGS.has(key) || next === undefined || next.startsWith('--')) { args[key] = true; continue; }
  args[key] = next;
  i++;
}

const base = String(args.base || 'http://127.0.0.1:8080').replace(/\/+$/, '');
const logPath = typeof args.log === 'string' ? args.log : '';
const transcriptPath = typeof args.transcript === 'string' ? args.transcript : '';
const shotDir = typeof args.screenshots === 'string' ? args.screenshots : '';
const username = typeof args.username === 'string' ? args.username : 'kbverify';
const password = typeof args.password === 'string' ? args.password : 'kbverify123456';
const preferModel = typeof args.model === 'string' ? args.model : 'qwen2.5:7b';
const skipImage = args['skip-image'] === true;
const IMAGE_TIMEOUT_MS = 300000;

// ---------- 输出与判定 ----------
const lines = [];
const checks = [];
const steps = [];
const say = (s) => { lines.push(s); console.log(s); };
function record(level, name, expect, actual) {
  checks.push({ level, name, expect: String(expect), actual: String(actual) });
  say('[' + level + '] ' + name + '  期望=' + expect + ' 实际=' + actual);
}
const pass = (n, e, a) => record('PASS', n, e, a);
const fail = (n, e, a) => record('FAIL', n, e, a);
const warn = (n, e, a) => record('WARN', n, e, a);
function check(name, expect, actual, okFlag, soft) {
  if (okFlag) pass(name, expect, actual);
  else if (soft) warn(name, expect, actual);
  else fail(name, expect, actual);
  return !!okFlag;
}
const oneLine = (v, n = 140) => {
  const s = v === undefined || v === null ? '' : String(v).replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '...' : s;
};

// ---------- 载入 playwright-core（固定路径 + 兜底） ----------
const require = createRequire(import.meta.url);
const PW_CANDIDATES = [
  'C:/Users/woshiss/AppData/Roaming/npm/node_modules/openclaw/node_modules/playwright-core',
  'playwright-core',
];
function loadPlaywright() {
  const tried = [];
  for (const c of PW_CANDIDATES) {
    try { return { mod: require(c), from: c }; } catch (e) { tried.push(c + ' -> ' + e.message); }
  }
  throw new Error('找不到 playwright-core：' + tried.join(' | '));
}
const CHROME_CANDIDATES = [
  'C:/Users/woshiss/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe',
  'C:/Users/woshiss/AppData/Local/ms-playwright/chromium_headless_shell-1223/chrome-win/headless_shell.exe',
];

// ---------- 素材 ----------
const d = new Date();
const pad = (n) => String(n).padStart(2, '0');
const runTag = '' + d.getFullYear() + pad(d.getMonth() + 1) + pad(d.getDate()) + pad(d.getHours()) + pad(d.getMinutes()) + pad(d.getSeconds());
const secret = 'UIVERIFY-' + runTag;
const mdName = 'ui-verify-' + runTag + '.md';
const pngName = 'ui-verify-' + runTag + '.png';

// 入库用的长文本：确保能切出多个块，暗号词出现在首尾
const filler = [
  '这段文字由前端验收脚本生成，用来验证「粘贴/上传文本 -> 切块 -> 嵌入 -> 检索」这条链路在前端点击后确实跑通。',
  '知识库把长文切成交叠的小块，每块单独算向量，检索时按余弦相似度取前几个。',
  '前端只负责发请求与渲染，切块与向量化都发生在后端，前端拿到的是一串命中块与相似度。',
  '如果前端没有真正接上接口，页面上不会出现文档行，也不会出现命中块，本脚本就会失败。',
];
const paragraphs = [];
for (let i = 1; i <= 14; i++) paragraphs.push('【前端验收段落 ' + pad(i) + '】' + filler[(i - 1) % filler.length] + '（序号 ' + i + '）');
const mdBody = ['前端验收专用文本，暗号词是 ' + secret + '。', ...paragraphs, '结尾再次确认暗号词：' + secret + '。'].join('\n\n');

// 每次运行都不同的 PNG（加一个 tEXt 块改字节，避免命中 sha256 去重）
function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) {
    c ^= buf[i];
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  }
  return (c ^ 0xffffffff) >>> 0;
}
function textChunk(keyword, text) {
  const data = Buffer.concat([Buffer.from(keyword, 'latin1'), Buffer.from([0]), Buffer.from(text, 'latin1')]);
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length, 0);
  const type = Buffer.from('tEXt', 'latin1');
  const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(Buffer.concat([type, data])), 0);
  return Buffer.concat([len, type, data, crc]);
}
const PNG_B64 = 'iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAbUlEQVR42g3NQQEAMAgDsUqpFKQgBSmVwvNkIGWLgUjCokSLERErTkjGpkybMTFrzkiFiyq6mCLFFldIjZtqupkmzTbXSIOHGnqYIcMON/8IDhU6TEjYcPnH4qWWXmbJssvtPw4fdfQxR4497ni85GoBf4s30gAAAABJRU5ErkJggg==';
function buildPng(tag) {
  const b = Buffer.from(PNG_B64, 'base64');
  return Buffer.concat([b.subarray(0, b.length - 12), textChunk('Comment', 'day23-ui-' + tag), b.subarray(b.length - 12)]);
}

// ============================ 主流程 ============================
const out = { console: [], failedRequests: [] };
let browser = null;
let fatal = '';

async function runAll() {
  say('== Day 23 验收（前端页面）：知识库视图端到端 ==');
  say('Base = ' + base);
  say('时间 = ' + d.toISOString());
  say('');

  const pw = loadPlaywright();
  say('playwright-core 来自：' + pw.from);
  const exe = CHROME_CANDIDATES.find((p) => fs.existsSync(p));
  say('Chromium 可执行文件：' + (exe || '(未找到，交给 playwright 自己找)'));

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'day23-ui-'));
  const mdPath = path.join(tmp, mdName);
  const pngPath = path.join(tmp, pngName);
  fs.writeFileSync(mdPath, mdBody, 'utf8');
  fs.writeFileSync(pngPath, buildPng(runTag));
  say('临时素材：' + mdPath + '（' + Buffer.byteLength(mdBody, 'utf8') + ' 字节）、' + pngPath + '（' + fs.statSync(pngPath).size + ' 字节）');

  if (shotDir) fs.mkdirSync(shotDir, { recursive: true });
  const shots = [];
  async function shot(page, name) {
    if (!shotDir) return;
    const p = path.join(shotDir, name);
    await page.screenshot({ path: p });
    shots.push(p);
    say('  截图：' + p);
  }

  browser = await pw.mod.chromium.launch({ headless: true, executablePath: exe, args: ['--no-sandbox'] });
  const context = await browser.newContext({ viewport: { width: 1440, height: 1180 } });
  const page = await context.newPage();
  page.on('console', (m) => {
    if (m.type() === 'error') out.console.push(oneLine(m.text(), 240));
  });
  page.on('requestfailed', (r) => {
    out.failedRequests.push({ url: r.url(), error: (r.failure() && r.failure().errorText) || '' });
  });
  // 记录所有 4xx/5xx 资源响应：kb.js / style.css 这类静态资源如果 404，页面会"看起来正常但没接上"，
  // 这是必须硬断言的东西（浏览器默认请求的 /favicon.ico 404 不算，后端本就没有该路由）。
  out.badResponses = [];
  page.on('response', (r) => {
    if (r.status() >= 400) out.badResponses.push({ status: r.status(), url: r.url() });
  });

  // ---------- 第 0 步：登录 ----------
  say('');
  say('-- 第 0 步：登录 --');
  await page.goto(base + '/login', { waitUntil: 'domcontentloaded' });
  await page.fill('#username', username);
  await page.fill('#password', password);
  // 用 Enter 而不是点按钮：Enter 走表单默认提交，行为与用户一致
  await Promise.all([
    page.waitForNavigation({ waitUntil: 'domcontentloaded', timeout: 30000 }).catch(() => null),
    page.press('#password', 'Enter'),
  ]);
  const afterLoginUrl = page.url();
  const loggedIn = !/\/login$/.test(afterLoginUrl);
  check('登录后离开 /login', '不在 /login', afterLoginUrl, loggedIn);
  out.login = { url: afterLoginUrl, ok: loggedIn };

  // ---------- 第 1 步：知识库入口 ----------
  say('');
  say('-- 第 1 步：知识库入口（导航 + 视图）--');
  const tab = page.locator('.view-tab[data-view="kb"]');
  check('侧栏存在「知识库」导航项', '存在', (await tab.count()) + ' 个', (await tab.count()) === 1);
  check('导航项文案为「知识库」', '知识库', oneLine(await tab.first().innerText()), (await tab.first().innerText()).trim() === '知识库');

  await page.click('.view-tab[data-view="kb"]');
  await page.waitForTimeout(400);
  check('hash 变为 #/kb', '#/kb', page.url().split('#')[1] || '', (page.url().split('#')[1] || '') === '/kb');
  check('知识库视图可见', '可见', String(await page.locator('#view-kb').isVisible()), await page.locator('#view-kb').isVisible());
  check('对话区在知识库视图下隐藏', '隐藏', String(await page.locator('#messages').isVisible()), !(await page.locator('#messages').isVisible()));

  // 等统计与列表首次加载完成（空库会显示提示文案）
  await page.waitForTimeout(600);
  const statDocs0 = await page.locator('#kb-stat-docs').innerText();
  check('统计数字已加载（不是占位 …）', '数字', statDocs0, /^\d+$/.test(statDocs0.trim()));
  await shot(page, 'kb-1-view.png');

  // ---------- 第 2 步：上传文本文件（路径一：上传） ----------
  say('');
  say('-- 第 2 步：上传 .md 文件（前端上传路径）--');
  await page.setInputFiles('#kb-file', mdPath);
  const chosen = await page.locator('#kb-file').evaluate((el) => (el.files && el.files[0] ? el.files[0].name : ''));
  check('文件选择器已选中素材', mdName, chosen, chosen === mdName);
  await page.click('#kb-upload-btn');
  await page.waitForFunction(() => {
    const n = document.getElementById('kb-upload-note');
    return n && /已入库|失败|已存在/.test(n.textContent || '');
  }, null, { timeout: 120000 });
  const uploadNote = (await page.locator('#kb-upload-note').innerText()).trim();
  say('  上传提示：' + oneLine(uploadNote));
  check('上传后提示「已入库」', '含 已入库', oneLine(uploadNote), uploadNote.includes('已入库'));
  const uploadedId = (uploadNote.match(/id=(\d+)/) || [])[1] || '';
  out.upload = { filename: mdName, note: uploadNote, id: uploadedId };
  await page.waitForFunction((name) => {
    const box = document.getElementById('kb-doc-list');
    return box && box.textContent.indexOf(name) !== -1;
  }, mdName, { timeout: 30000 });
  check('文档列表出现刚上传的文件', mdName, oneLine(await page.locator('#kb-doc-list').innerText(), 200), (await page.locator('#kb-doc-list').innerText()).includes(mdName));
  await shot(page, 'kb-2-upload.png');

  // ---------- 第 3 步：检索（路径二） ----------
  say('');
  say('-- 第 3 步：检索（前端检索路径）--');
  await page.fill('#kb-search-input', secret);
  await page.click('#kb-search-btn');
  await page.waitForSelector('.kb-hit', { timeout: 60000 }).catch(() => null);
  const cards = page.locator('#kb-search-result .kb-hit');
  const hitCount = await cards.count();
  check('检索结果渲染出命中块', '>= 1', hitCount + ' 个', hitCount >= 1);
  const hits = [];
  for (let i = 0; i < hitCount; i++) {
    const c = cards.nth(i);
    hits.push({
      text: await c.locator('.kb-hit-text').innerText(),
      meta: await c.locator('.kb-hit-meta').innerText(),
      score: await c.locator('.kb-hit-score').innerText(),
    });
  }
  hits.forEach((h, i) => say('  命中 ' + (i + 1) + '：' + oneLine(h.score, 30) + ' | ' + oneLine(h.meta, 80)));
  const anyContains = hits.some((h) => h.text.includes(secret));
  check('命中的块里至少有一条包含检索词（召回证据）', '含 ' + secret, anyContains ? '有' : '无', anyContains);
  const hitDocIds = hits.map((h) => (h.meta.match(/document_id=(\d+)/) || [])[1]).filter(Boolean);
  const containsUploaded = uploadedId ? hitDocIds.indexOf(String(uploadedId)) !== -1 : false;
  check('命中里包含刚上传文档的块（document_id=' + uploadedId + '）', '包含', hitDocIds.join(', ') || '(无)', containsUploaded);
  // 纯向量检索没有词法保证：命中集合里有该词是硬断言，排第一位是不是它只记 WARN。
  check('排在第一位的命中包含检索词（软断言：向量检索不保证词法排序）', '含 ' + secret,
    oneLine(hits.length ? hits[0].text : '', 90), hits.length ? hits[0].text.includes(secret) : false, true);
  out.search = {
    query: secret,
    hitCount,
    anyContainsSecret: anyContains,
    hitDocumentIds: hitDocIds,
    containsUploadedDoc: containsUploaded,
    topContainsSecret: hits.length ? hits[0].text.includes(secret) : false,
    topPreview: oneLine(hits.length ? hits[0].text : '', 200),
    hitScores: hits.map((h) => h.score),
  };
  await shot(page, 'kb-3-search.png');

  // ---------- 第 4 步：图片上传（多模态，路径一的多模态分支） ----------
  if (!skipImage) {
    say('');
    say('-- 第 4 步：上传 PNG（多模态：视觉模型生成描述后入库，较慢）--');
    await page.setInputFiles('#kb-file', pngPath);
    await page.click('#kb-upload-btn');
    const t0 = Date.now();
    await page.waitForFunction(() => {
      const n = document.getElementById('kb-upload-note');
      return n && /已入库|失败|已存在/.test(n.textContent || '') && !/上传中/.test(n.textContent || '');
    }, null, { timeout: IMAGE_TIMEOUT_MS });
    const imgNote = (await page.locator('#kb-upload-note').innerText()).trim();
    say('  图片上传耗时 ' + ((Date.now() - t0) / 1000).toFixed(1) + 's，提示：' + oneLine(imgNote));
    check('图片上传后提示「已入库」', '含 已入库', oneLine(imgNote), imgNote.includes('已入库'));
    check('图片上传提示 kind=image', '含 kind=image', oneLine(imgNote), imgNote.includes('kind=image'));
    await page.waitForFunction((name) => {
      const box = document.getElementById('kb-doc-list');
      return box && box.textContent.indexOf(name) !== -1;
    }, pngName, { timeout: 30000 });
    check('文档列表出现图片文档', pngName, oneLine(await page.locator('#kb-doc-list').innerText(), 200), (await page.locator('#kb-doc-list').innerText()).includes(pngName));
    out.imageUpload = { filename: pngName, note: imgNote, seconds: Number(((Date.now() - t0) / 1000).toFixed(1)) };
    await shot(page, 'kb-4-image-upload.png');
  } else {
    say('（--skip-image：跳过图片上传）');
  }

  // ---------- 第 5 步：知识库问答（路径三，SSE） ----------
  say('');
  say('-- 第 5 步：基于知识库问答（前端 SSE 路径）--');
  const models = await page.locator('#model-select option').evaluateAll((opts) => opts.map((o) => ({ value: o.value, text: o.textContent })));
  say('  可选模型：' + models.map((m) => m.value).join(', '));
  const target = models.find((m) => m.value.indexOf(preferModel) !== -1);
  if (!target) {
    fail('模型下拉里存在 ' + preferModel, '存在', models.map((m) => m.value).join(', ') || '(空)');
  } else {
    await page.selectOption('#model-select', target.value);
    say('  选中模型：' + target.value);
  }
  const question = '本次前端验收文本里的暗号词是什么？请只依据知识库资料回答。';
  await page.fill('#kb-ask-input', question);
  await page.click('#kb-ask-btn');
  const askT0 = Date.now();
  await page.waitForSelector('#kb-ask-result .kb-answer-text', { timeout: 180000 }).catch(() => null);
  await page.waitForFunction(() => {
    const el = document.querySelector('#kb-ask-result .kb-answer-text');
    return el && (el.textContent || '').trim().length > 0;
  }, null, { timeout: 180000 }).catch(() => null);
  // 只等"有内容"是不够的：那是第一个 delta 就满足的条件。必须等流真正结束（提问按钮重新可用），
  // 否则读到的是半截回答，而且后续删除会被页面的 busy 守卫挡掉（这正是第一次跑出 FAIL 的原因）。
  const settled = await page.waitForFunction(() => {
    const b = document.getElementById('kb-ask-btn');
    return b && !b.disabled;
  }, null, { timeout: 180000 }).then(() => true).catch(() => false);
  check('问答流式结束后提问按钮恢复可用', '恢复可用', settled ? '已恢复' : '一直禁用（超时）', settled);
  const answer = (await page.locator('#kb-ask-result .kb-answer-text').innerText().catch(() => '')).trim();
  const sources = await page.locator('#kb-ask-result .kb-source').count();
  const wantSources = await page.locator('#kb-ask-result .kb-sources').count();
  say('  问答耗时 ' + ((Date.now() - askT0) / 1000).toFixed(1) + 's；出处 ' + sources + ' 条');
  say('  回答（前 180 字）：' + oneLine(answer, 180));
  check('问答渲染出 sources 出处区', '>= 1', wantSources + ' 个', wantSources >= 1);
  check('出处条目数 >= 1', '>= 1', sources + ' 个', sources >= 1);
  check('回答正文非空', '非空', answer.length + ' 字', answer.length > 0);
  check('回答包含暗号词（软断言：取决于模型）', '含 ' + secret, oneLine(answer, 90), answer.includes(secret), true);
  const errNote = await page.locator('#kb-ask-result .kb-note-error').count();
  check('问答没有渲染错误提示', '0', errNote + ' 个', errNote === 0);
  out.ask = { model: target ? target.value : '', question, answer, sources, seconds: Number(((Date.now() - askT0) / 1000).toFixed(1)) };
  await shot(page, 'kb-5-ask.png');

  // ---------- 第 6 步：删除（前端删除路径） ----------
  say('');
  say('-- 第 6 步：删除文本文档 --');
  page.on('dialog', (dlg) => dlg.accept());
  const rowTextBefore = await page.locator('#kb-doc-list').innerText();
  const row = page.locator('.kb-doc-row', { hasText: mdName }).first();
  const rowsBefore = await page.locator('.kb-doc-row').count();
  check('删除前列表里能找到该行', '存在', rowTextBefore.includes(mdName) ? '存在' : '不存在', rowTextBefore.includes(mdName));
  await row.locator('.kb-doc-del').click();
  await page.waitForFunction((name) => {
    const box = document.getElementById('kb-doc-list');
    return box && box.textContent.indexOf(name) === -1;
  }, mdName, { timeout: 30000 }).catch(() => null);
  const rowTextAfter = await page.locator('#kb-doc-list').innerText();
  const rowsAfter = await page.locator('.kb-doc-row').count();
  check('删除后列表里不再有该文档', '不包含', rowTextAfter.includes(mdName) ? '仍包含' : '不包含', !rowTextAfter.includes(mdName));
  say('  行数：' + rowsBefore + ' -> ' + rowsAfter);
  out.delete = { filename: mdName, rowsBefore, rowsAfter, gone: !rowTextAfter.includes(mdName) };
  await shot(page, 'kb-6-delete.png');

  // 本脚本创建的图片文档也删掉，尽量把知识库还原成运行前的样子（别人的文档一律不动）
  if (!skipImage && (await page.locator('.kb-doc-row', { hasText: pngName }).count()) > 0) {
    const imgRow = page.locator('.kb-doc-row', { hasText: pngName }).first();
    await imgRow.locator('.kb-doc-del').click();
    await page.waitForFunction((name) => {
      const box = document.getElementById('kb-doc-list');
      return box && box.textContent.indexOf(name) === -1;
    }, pngName, { timeout: 30000 }).catch(() => null);
    const listTextLast = await page.locator('#kb-doc-list').innerText();
    check('清理：本次创建的图片文档已删除', '不包含', listTextLast.includes(pngName) ? '仍包含' : '不包含', !listTextLast.includes(pngName));
    out.cleanupImage = { filename: pngName, gone: !listTextLast.includes(pngName) };
    await shot(page, 'kb-7-cleanup.png');
  }

  // 页面自身的 JS 报错与失败请求也是证据
  const badResources = out.badResponses.filter((r) => !/favicon/i.test(r.url));
  const faviconOnly = out.badResponses.filter((r) => /favicon/i.test(r.url));
  check('页面加载的资源没有 4xx/5xx（/favicon.ico 除外）', '0', badResources.length + ' 条', badResources.length === 0);
  if (badResources.length) badResources.forEach((r) => say('  资源错误：' + r.status + ' ' + r.url));
  if (faviconOnly.length) say('  说明：/favicon.ico 返回 ' + faviconOnly[0].status + '，后端本就没有该路由，不计入判定。');
  const realConsoleErrors = out.console.filter((c) => !/favicon/i.test(c) && !/404 \(Not Found\)/.test(c));
  check('页面没有 JS console 报错（favicon/静态资源 404 已在上面单独判定）', '0', realConsoleErrors.length + ' 条', realConsoleErrors.length === 0, true);
  out.console.forEach((c) => say('  console.error: ' + c));
  if (out.failedRequests.length) out.failedRequests.forEach((r) => say('  requestfailed: ' + r.url + ' ' + r.error));
  out.screenshots = shots;
}

let exitCode = 1;
try {
  await runAll();
} catch (e) {
  fatal = e && (e.stack || e.message) ? String(e.stack || e.message) : String(e);
  fail('脚本执行中断', '无异常地跑完流程', oneLine(fatal, 220));
  say(fatal);
} finally {
  if (browser) { try { await browser.close(); } catch (e) {} }
}

const nPass = checks.filter((c) => c.level === 'PASS').length;
const nWarn = checks.filter((c) => c.level === 'WARN').length;
const nFail = checks.filter((c) => c.level === 'FAIL').length;
say('');
if (nWarn) {
  say('WARN 明细（不影响退出码）：');
  for (const c of checks.filter((c) => c.level === 'WARN')) say('  - ' + c.name + '（期望=' + c.expect + ' 实际=' + c.actual + '）');
}
if (nFail) {
  say('FAIL 明细：');
  for (const c of checks.filter((c) => c.level === 'FAIL')) say('  - ' + c.name + '（期望=' + c.expect + ' 实际=' + c.actual + '）');
}
exitCode = nFail > 0 ? 1 : 0;
say('== 汇总：PASS ' + nPass + ' / WARN ' + nWarn + ' / FAIL ' + nFail + ' ==');
say('退出码 = ' + exitCode + '（有 FAIL 即 1，否则 0）');

if (logPath) {
  fs.mkdirSync(path.dirname(path.resolve(logPath)), { recursive: true });
  fs.writeFileSync(logPath, lines.join('\n') + '\n', 'utf8');
  say('log written: ' + logPath);
}
if (transcriptPath) {
  fs.mkdirSync(path.dirname(path.resolve(transcriptPath)), { recursive: true });
  fs.writeFileSync(transcriptPath, JSON.stringify({
    base, runTag, secret, mdName, pngName, fatal: fatal || undefined,
    evidence: out, checks, summary: { pass: nPass, warn: nWarn, fail: nFail, exitCode },
  }, null, 2), 'utf8');
  console.log('transcript written: ' + transcriptPath);
}
process.exit(exitCode);
