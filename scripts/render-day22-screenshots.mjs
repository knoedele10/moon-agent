#!/usr/bin/env node
// Day 22 截图渲染脚本：读验收脚本产出的 transcript.json，渲染两张证据图。
//   1) day22-1-patch-before-after.png —— PATCH 改一条数据：改之前 vs 改之后
//   2) day22-2-delete-gone.png        —— DELETE 删一条数据：GET 返回里该条已消失
//
// 用法：
//   node scripts/render-day22-screenshots.mjs --transcript <临时目录>/transcript.json \
//        --outdir docs/day22/screenshots [--htmlout <临时目录>] [--chrome <chrome.exe>]
//
// 只渲染 + 截图，不改任何业务代码。找不到 Chrome 时只写 HTML 并提示。

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

const args = {};
for (let i = 2; i < process.argv.length; i++) {
  const a = process.argv[i];
  if (a.startsWith('--')) args[a.slice(2)] = process.argv[++i];
}
const transcriptPath = args.transcript;
const outDir = args.outdir || 'docs/day22/screenshots';
const htmlDir = args.htmlout || path.join(os.tmpdir(), 'day22-render');
if (!transcriptPath) {
  console.error('缺少 --transcript');
  process.exit(1);
}

const t = JSON.parse(fs.readFileSync(transcriptPath, 'utf8'));

const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
function hl(objOrText) {
  const text = typeof objOrText === 'string' ? objOrText : JSON.stringify(objOrText, null, 2);
  return esc(text)
    .replace(/"([^"\\]*)":/g, '<span class="k">"$1"</span>:')
    .replace(/: "([^"\\]*)"/g, ': <span class="s">"$1"</span>');
}

const CSS = [
  ":root { --ink:#16202e; --muted:#5b6b7f; --line:#d8e0ea; --accent:#1f6feb; --ok:#0f8a5f; --warn:#b4341f; --bg:#f4f7fb; }",
  "* { box-sizing:border-box; }",
  "body { margin:0; background:var(--bg); color:var(--ink); font-family:\"Microsoft YaHei\",\"Segoe UI\",system-ui,-apple-system,sans-serif; font-size:15px; line-height:1.7; }",
  ".sheet { max-width:1180px; margin:0 auto; background:#fff; padding:34px 44px 46px; }",
  ".kicker { display:flex; justify-content:space-between; align-items:center; border-bottom:3px solid var(--ink); padding-bottom:10px; }",
  ".kicker .brand { font-size:13px; letter-spacing:.16em; color:var(--muted); }",
  ".kicker .stamp { font-size:12px; color:var(--muted); }",
  "h1 { font-size:25px; margin:14px 0 4px; }",
  "h2 { font-size:17px; margin:24px 0 8px; padding-left:10px; border-left:4px solid var(--accent); }",
  "p { margin:6px 0; }",
  ".mono { font-family:Consolas,\"Courier New\",monospace; font-size:12.5px; }",
  "pre { background:#0f1b2d; color:#dce6f2; padding:12px 14px; border-radius:6px; overflow:hidden; white-space:pre-wrap; word-break:break-all; margin:8px 0; font-family:Consolas,\"Courier New\",monospace; font-size:12.5px; line-height:1.6; }",
  "pre .k { color:#7fb2ff; }",
  "pre .s { color:#9ce8a8; }",
  "table { width:100%; border-collapse:collapse; margin:10px 0 16px; font-size:13.5px; }",
  "th, td { border:1px solid var(--line); padding:7px 9px; text-align:left; vertical-align:top; }",
  "th { background:#eef3fa; font-weight:600; }",
  "tbody tr:nth-child(even) { background:#fafcff; }",
  "tr.hit td { background:#fff6d8; font-weight:600; }",
  "tr.gone td { background:#fdecea; color:var(--warn); }",
  ".cmp { display:flex; gap:14px; margin:10px 0; }",
  ".cmp .box { flex:1; border:1px solid var(--line); border-radius:6px; padding:10px 12px; }",
  ".cmp .box .tag { font-size:12px; letter-spacing:.1em; color:var(--muted); }",
  ".cmp .box .val { font-family:Consolas,\"Courier New\",monospace; font-size:13px; margin-top:4px; word-break:break-all; }",
  ".cmp .box.before { background:#f7f9fc; }",
  ".cmp .box.after { background:#eefaf2; border-color:#bfe6cd; }",
  ".callout { margin:12px 0; padding:10px 14px; border-radius:6px; background:#eefaf2; border-left:4px solid var(--ok); }",
  ".callout.warn { background:#fdf3f1; border-left-color:var(--warn); }",
  ".pill { display:inline-block; padding:1px 8px; border-radius:10px; font-size:12px; font-weight:600; }",
  ".pill.ok { background:#d8f2e3; color:#0b6b4a; }",
  ".pill.no { background:#fadbd6; color:#8f2716; }",
  "footer { margin-top:26px; border-top:1px solid var(--line); padding-top:9px; font-size:12px; color:var(--muted); display:flex; justify-content:space-between; }",
].join('\n');

const head = (title, kicker) =>
  '<!DOCTYPE html>\n<html lang="zh-CN">\n<head>\n<meta charset="UTF-8">\n<title>' + esc(title) + '</title>\n<style>' + CSS + '</style>\n</head>\n<body>\n<div class="sheet">\n' +
  '<div class="kicker"><span class="brand">BLOG-MY · ' + esc(kicker) + '</span><span class="stamp">' + esc(t.stamp) + '</span></div>\n';
const foot = (label) =>
  '\n<footer><span>blog-my · Day 22 交付</span><span>' + esc(label) + '</span></footer>\n</div>\n' +
  '<script>addEventListener("load",function(){document.title="H:"+document.documentElement.scrollHeight;});</script>\n</body>\n</html>\n';

// ---------- 图 1：PATCH 改之前 → 改之后 ----------
const p = t.patch;
const page1 = head('Day 22 · PATCH 证据', 'DAY 22 · PATCH 改一条数据') +
  '<h1>PATCH /api/favorites/' + p.id + ' —— 改一条数据，值真的变了</h1>' +
  '<p>靶子记录：POST 造一条（id=' + p.id + '），先 GET 读原值，再 PATCH，最后 GET 回读对比。</p>' +

  '<h2>改之前（GET /api/favorites 读到的原值）</h2>' +
  '<div class="cmp">' +
  '<div class="box before"><div class="tag">BEFORE · title</div><div class="val">' + esc(p.before.title) + '</div></div>' +
  '<div class="box before"><div class="tag">BEFORE · url</div><div class="val">' + esc(p.before.url === null ? 'null' : p.before.url) + '</div></div>' +
  '</div>' +

  '<h2>PATCH 请求体</h2><pre>' + hl(p.request) + '</pre>' +
  '<h2>PATCH 响应（HTTP 200，响应里带回 before 原值）</h2><pre>' + hl(p.response) + '</pre>' +

  '<h2>改之后（GET /api/favorites 回读）</h2>' +
  '<div class="cmp">' +
  '<div class="box after"><div class="tag">AFTER · title</div><div class="val">' + esc(p.after.title) + '</div></div>' +
  '<div class="box after"><div class="tag">AFTER · url</div><div class="val">' + esc(p.after.url === null ? 'null' : p.after.url) + '</div></div>' +
  '</div>' +

  '<h2>改之前 vs 改之后</h2>' +
  '<table><thead><tr><th>字段</th><th>改之前</th><th>改之后</th><th>结论</th></tr></thead><tbody>' +
  '<tr><td>title</td><td class="mono">' + esc(p.before.title) + '</td><td class="mono">' + esc(p.after.title) + '</td><td><span class="pill ok">已变更</span></td></tr>' +
  '<tr><td>url</td><td class="mono">' + esc(p.before.url) + '</td><td class="mono">' + esc(p.after.url) + '</td><td><span class="pill ok">已变更</span></td></tr>' +
  '</tbody></table>' +
  '<div class="callout">PATCH 只改请求体里出现的字段，没传的保持原值；改成与别的记录重名会返回 409，空请求体 / 非法 JSON 返回 400。</div>' +
  foot(t.base);

// ---------- 图 2：DELETE 后 GET 不再返回 ----------
const d = t.del;
const rows = (items, goneId) => items.map((it) =>
  '<tr class="' + (it.id === goneId ? 'hit' : '') + '"><td class="mono">' + it.id + '</td><td>' + esc(it.title) + '</td></tr>').join('');
const page2 = head('Day 22 · DELETE 证据', 'DAY 22 · DELETE 删一条数据') +
  '<h1>DELETE /api/favorites/' + d.id + ' —— 删完 GET 就不再返回它</h1>' +
  '<p>同一条靶子记录：删除前 GET 里有它，DELETE 之后再 GET，它已经不在返回列表里。</p>' +

  '<h2>删除前：GET /api/favorites 返回 ' + d.beforeCount + ' 条（含 id=' + d.id + '）</h2>' +
  '<table><thead><tr><th>id</th><th>title</th></tr></thead><tbody>' + rows(d.beforeItems, d.id) + '</tbody></table>' +

  '<h2>DELETE 响应（HTTP 200）</h2><pre>' + hl(d.response) + '</pre>' +

  '<h2>删除后：GET /api/favorites 返回 ' + d.afterCount + ' 条（id=' + d.id + ' 已消失）</h2>' +
  '<table><thead><tr><th>id</th><th>title</th></tr></thead><tbody>' + rows(d.afterItems, -1) + '</tbody></table>' +
  '<div class="callout">删除前 ' + d.beforeCount + ' 条 → 删除后 ' + d.afterCount + ' 条；<b>id=' + d.id + ' 不在返回里</b>。再删一次同一个 id 会返回 404。</div>' +

  '<h2>余力加练：软删除（数据没真删，删错能找回）</h2>' +
  '<p>DELETE 只在记录上打 <span class="mono">is_deleted</span> 标记，查询时跳过；数据本身仍留在 favorites.json 里：</p>' +
  '<pre>' + hl(d.softRecord) + '</pre>' +
  foot(t.base);

fs.mkdirSync(htmlDir, { recursive: true });
const p1 = path.join(htmlDir, 'day22-1-patch.html');
const p2 = path.join(htmlDir, 'day22-2-delete.html');
fs.writeFileSync(p1, page1, 'utf8');
fs.writeFileSync(p2, page2, 'utf8');
console.log('html: ' + p1);
console.log('html: ' + p2);

// ---------- 找 Chrome ----------
function findChrome() {
  if (args.chrome && fs.existsSync(args.chrome)) return args.chrome;
  const home = os.homedir();
  const pw = path.join(home, 'AppData', 'Local', 'ms-playwright');
  if (fs.existsSync(pw)) {
    const hit = fs.readdirSync(pw).filter((n) => /^chromium-\d+$/.test(n)).sort().reverse();
    for (const dir of hit) {
      const exe = path.join(pw, dir, 'chrome-win64', 'chrome.exe');
      if (fs.existsSync(exe)) return exe;
    }
  }
  const candidates = [
    'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  ];
  return candidates.find((c) => fs.existsSync(c)) || '';
}

const chrome = findChrome();
if (!chrome) {
  console.log('未找到 Chrome/Edge，只写了 HTML。截图请手动完成。');
  process.exit(0);
}
console.log('chrome: ' + chrome);

fs.mkdirSync(outDir, { recursive: true });
const shots = [
  { html: p1, png: path.join(outDir, 'day22-1-patch-before-after.png') },
  { html: p2, png: path.join(outDir, 'day22-2-delete-gone.png') },
];
for (const s of shots) {
  const url = 'file:///' + s.html.replace(/\\/g, '/');
  const dom = execFileSync(chrome, ['--headless=new', '--disable-gpu', '--hide-scrollbars', '--virtual-time-budget=3000', '--dump-dom', url], { encoding: 'utf8', maxBuffer: 1 << 26 });
  const m = dom.match(/<title>H:(\d+)<\/title>/);
  const height = m ? Math.max(600, parseInt(m[1], 10) + 8) : 1400;
  execFileSync(chrome, ['--headless=new', '--disable-gpu', '--hide-scrollbars', '--force-device-scale-factor=1', '--window-size=1280,' + height, '--screenshot=' + s.png, url], { stdio: 'ignore' });
  console.log('png: ' + s.png + '  (' + 1280 + 'x' + height + ')');
}
