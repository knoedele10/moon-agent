#!/usr/bin/env node
// Day 22 验收脚本：PATCH 改一条数据 + DELETE 删一条数据，验证「增删改查」四类操作闭环。
//
// 用法：
//   node scripts/verify-day22.mjs --base http://127.0.0.1:8123 \
//        --log docs/day22/verify-log.txt --transcript <临时目录>/transcript.json \
//        --data examples/mock-web/data/favorites.json
//
// 为什么用 Node 而不是 PowerShell：Day 21 踩过坑 —— Windows PowerShell 5.1 的
// Invoke-WebRequest 会把字符串请求体按 ASCII 发送，中文标题变成 '?'。Node 的 fetch
// 默认就是 UTF-8，从根上避开这个坑。脚本只读接口、不碰业务代码。

import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

const args = {};
for (let i = 2; i < process.argv.length; i++) {
  const a = process.argv[i];
  if (a.startsWith('--')) args[a.slice(2)] = process.argv[++i];
}
const base = (args.base || 'http://127.0.0.1:8123').replace(/\/$/, '');
const logPath = args.log || '';
const transcriptPath = args.transcript || '';
const dataPath = args.data || '';

const lines = [];
const checks = [];
const steps = [];
const say = (s) => { lines.push(s); console.log(s); };
function check(name, expect, actual, pass) {
  checks.push({ name, expect: String(expect), actual: String(actual), pass: !!pass });
  say(`[${pass ? 'PASS' : 'FAIL'}] ${name}  期望=${expect} 实际=${actual}`);
}

// 用 node:http 而不是 fetch：fetch（undici）在 Windows 上退出时会触发 libuv 断言，
// 退出码变得不可靠；http + agent:false 的短连接收尾干净。
function httpReq(method, p, { body, headers } = {}) {
  return new Promise((resolve, reject) => {
    const u = new URL(base + p);
    const r = http.request(
      { hostname: u.hostname, port: u.port || 80, path: u.pathname + u.search, method, headers: headers || {}, agent: false },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => resolve({ status: res.statusCode, text: Buffer.concat(chunks).toString('utf8'), headers: res.headers }));
      }
    );
    r.on('error', reject);
    if (body !== undefined) r.write(body);
    r.end();
  });
}

async function req(method, p, body, rawBody) {
  const headers = {};
  let payload;
  if (rawBody !== undefined) {
    headers['Content-Type'] = 'application/json; charset=utf-8';
    payload = rawBody;
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json; charset=utf-8';
    payload = JSON.stringify(body);
  }
  const res = await httpReq(method, p, { body: payload, headers });
  steps.push({
    method, path: p, status: res.status, body: res.text,
    cors: res.headers['access-control-allow-origin'] || '',
  });
  return { status: res.status, text: res.text, headers: res.headers };
}

async function listFavorites() {
  const r = await req('GET', '/api/favorites');
  const data = JSON.parse(r.text).data || [];
  return { count: data.length, titles: data.map((x) => x.title), items: data };
}

const stamp = new Date().toISOString();
say(`== Day 22 验收：PATCH / DELETE 闭环 @ ${stamp} ==`);
say(`Base = ${base}`);
say('');

// 0) 探活
const health = await req('GET', '/api/health');
check('服务探活 GET /api/health', '200', health.status, health.status === 200);

// 1) 造一条测试数据（POST 是 Day 18 已上线的接口，这里当"改/删的靶子"）
say('');
say('-- 第 1 步：POST 造一条待改待删的数据 --');
const tag = Date.now().toString().slice(-6);
const uniqueTitle = `Day 22 验收：待改待删的一条 ${tag}`;
const created = await req('POST', '/api/favorites', { title: uniqueTitle, url: 'https://example.com/before' });
const createdBody = JSON.parse(created.text);
const id = createdBody.id;
check('创建测试数据 POST /api/favorites', '201', created.status, created.status === 201);
say(`  新记录 id=${id}  title="${createdBody.title}"  url=${JSON.stringify(createdBody.url)}`);

// 2) 改之前
say('');
say('-- 第 2 步：改之前（GET 读出原始值）--');
const before = await listFavorites();
const beforeItem = before.items.find((x) => x.id === id);
check('改之前：GET 能读到该条', `id=${id}`, beforeItem ? `id=${beforeItem.id}` : '未找到', !!beforeItem);
say(`  改之前：title="${beforeItem.title}"  url=${JSON.stringify(beforeItem.url)}`);

// 3) PATCH
say('');
say('-- 第 3 步：PATCH 改一条数据 --');
const newTitle = `Day 22 验收：已改成新标题 ${tag}`;
const newUrl = 'https://example.com/after';
const patchReqBody = { title: newTitle, url: newUrl };
const patched = await req('PATCH', `/api/favorites/${id}`, patchReqBody);
check('PATCH 成功返回 200', '200', patched.status, patched.status === 200);
const patchBody = JSON.parse(patched.text);
say(`  PATCH 请求体：${JSON.stringify(patchReqBody)}`);
say(`  PATCH 响应体：${patched.text}`);
check('PATCH 响应带 before 原值', beforeItem.title, patchBody.before && patchBody.before.title,
  !!(patchBody.before && patchBody.before.title === beforeItem.title));

// 4) 改之后
say('');
say('-- 第 4 步：改之后（GET 回读，确认新值已生效）--');
const after = await listFavorites();
const afterItem = after.items.find((x) => x.id === id);
check('改之后：title 变成新值', newTitle, afterItem ? afterItem.title : '(缺失)', !!afterItem && afterItem.title === newTitle);
check('改之后：url 变成新值', newUrl, afterItem ? afterItem.url : '(缺失)', !!afterItem && afterItem.url === newUrl);
say(`  改之后：title="${afterItem.title}"  url=${JSON.stringify(afterItem.url)}`);

// 5) PATCH 的边界
say('');
say('-- 第 5 步：PATCH 的坏输入都要被挡住 --');
const emptyPatch = await req('PATCH', `/api/favorites/${id}`, {});
check('PATCH 空请求体被拒', '400', emptyPatch.status, emptyPatch.status === 400);
const otherTitle = before.titles.find((t) => t !== beforeItem.title);
if (otherTitle) {
  const dupPatch = await req('PATCH', `/api/favorites/${id}`, { title: otherTitle });
  check('PATCH 改成与别的记录重名被拒', '409', dupPatch.status, dupPatch.status === 409);
}
const missingPatch = await req('PATCH', '/api/favorites/999999', { title: 'x' });
check('PATCH 不存在的 id 返回 404', '404', missingPatch.status, missingPatch.status === 404);
const badJson = await req('PATCH', `/api/favorites/${id}`, undefined, '{not json');
check('PATCH 非法 JSON 被拒', '400', badJson.status, badJson.status === 400);

// 6) DELETE
say('');
say('-- 第 6 步：DELETE 删一条数据 --');
const beforeDel = await listFavorites();
say(`  删除前：GET 返回 ${beforeDel.count} 条，含 id=${id}`);
const del = await req('DELETE', `/api/favorites/${id}`);
check('DELETE 成功返回 200', '200', del.status, del.status === 200);
const delBody = JSON.parse(del.text);
say(`  DELETE 响应体：${del.text}`);
const afterDel = await listFavorites();
check('删除后：该条不再返回', '该条消失', afterDel.items.find((x) => x.id === id) ? '仍能读到' : '已消失',
  !afterDel.items.find((x) => x.id === id));
check('删除后：条数比删除前少 1', beforeDel.count - 1, afterDel.count, afterDel.count === beforeDel.count - 1);
say(`  删除后：GET 返回 ${afterDel.count} 条，id=${id} 已不在列表中`);
const delAgain = await req('DELETE', `/api/favorites/${id}`);
check('重复 DELETE 返回 404', '404', delAgain.status, delAgain.status === 404);

// 7) 软删除：数据还在文件里，删错能找回（余力加练）
let softRecord = null;
if (dataPath && fs.existsSync(dataPath)) {
  say('');
  say('-- 第 7 步：软删除证据（数据没真删，还在文件里）--');
  const rows = JSON.parse(fs.readFileSync(dataPath, 'utf8'));
  softRecord = rows.find((x) => x.id === id) || null;
  check('软删除：记录仍在数据文件中', 'is_deleted=true', softRecord ? `is_deleted=${softRecord.is_deleted}` : '未找到',
    !!(softRecord && softRecord.is_deleted === true));
  say(`  文件里该条：${JSON.stringify(softRecord)}`);
}

// 8) 跨域预检要放行 PATCH / DELETE
say('');
say('-- 第 8 步：跨域预检放行 PATCH / DELETE --');
const opt = await httpReq('OPTIONS', `/api/favorites/${id}`, {
  headers: { Origin: 'http://localhost:8091', 'Access-Control-Request-Method': 'PATCH' },
});
const allow = opt.headers['access-control-allow-methods'] || '';
check('OPTIONS 预检返回 204', '204', opt.status, opt.status === 204);
check('预检响应允许 PATCH 与 DELETE', '含 PATCH,DELETE', allow, /PATCH/.test(allow) && /DELETE/.test(allow));

const pass = checks.filter((c) => c.pass).length;
const fail = checks.length - pass;
say('');
say(`== 汇总：PASS ${pass} / FAIL ${fail} ==`);

if (logPath) {
  fs.mkdirSync(path.dirname(logPath), { recursive: true });
  fs.writeFileSync(logPath, lines.join('\n') + '\n', 'utf8');
  say(`log written: ${logPath}`);
}
if (transcriptPath) {
  const transcript = {
    base, stamp, tag,
    patch: {
      id,
      before: { title: beforeItem.title, url: beforeItem.url },
      request: patchReqBody,
      response: patchBody,
      after: { title: afterItem.title, url: afterItem.url },
    },
    del: {
      id,
      beforeCount: beforeDel.count,
      beforeItems: beforeDel.items.map((x) => ({ id: x.id, title: x.title })),
      response: delBody,
      afterCount: afterDel.count,
      afterItems: afterDel.items.map((x) => ({ id: x.id, title: x.title })),
      softRecord,
    },
    steps, checks, pass, fail,
  };
  fs.writeFileSync(transcriptPath, JSON.stringify(transcript, null, 2), 'utf8');
  console.log(`transcript written: ${transcriptPath}`);
}
process.exit(fail > 0 ? 1 : 0);
