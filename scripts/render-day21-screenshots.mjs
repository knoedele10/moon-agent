// Day 21 截图渲染脚本：把三份交付文档（md）渲染成 HTML，供无头 Chrome 截图。
// 用法：node scripts/render-day21-screenshots.mjs
// 输出：系统临时目录 day21-render/*.html（中间产物不入库；截图结果入 docs/day21/screenshots/）
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const outDir = path.join(os.tmpdir(), 'day21-render');
fs.mkdirSync(outDir, { recursive: true });

const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const inline = (s) =>
  esc(s)
    .replace(/\x60([^\x60]+)\x60/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2">$1</a>')
    .replace(/(?<!\]\()(?<!")(https?:\/\/[^\s<>()\uFF08\uFF09\uFF0C\u3002\uFF1B\u3001"]+)/g, '<a href="$1">$1</a>');

const isSep = (s) => /^\s*\|[\s:|-]+\|\s*$/.test(s);
const cells = (l) => l.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map((c) => c.trim());

function convert(md) {
  const lines = md.replace(/\r\n/g, '\n').split('\n');
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }
    let m;
    if ((m = line.match(/^###\s+(.*)$/))) { out.push('<h3>' + inline(m[1]) + '</h3>'); i++; continue; }
    if ((m = line.match(/^##\s+(.*)$/))) { out.push('<h2>' + inline(m[1]) + '</h2>'); i++; continue; }
    if ((m = line.match(/^#\s+(.*)$/))) { out.push('<h1>' + inline(m[1]) + '</h1>'); i++; continue; }
    if (/^-{3,}\s*$/.test(line)) { out.push('<hr>'); i++; continue; }
    if (line.startsWith('|') && i + 1 < lines.length && isSep(lines[i + 1])) {
      const head = cells(line);
      out.push('<table><thead><tr>' + head.map((h) => '<th>' + inline(h) + '</th>').join('') + '</tr></thead><tbody>');
      i += 2;
      while (i < lines.length && lines[i].startsWith('|')) {
        out.push('<tr>' + cells(lines[i]).map((c) => '<td>' + inline(c) + '</td>').join('') + '</tr>');
        i++;
      }
      out.push('</tbody></table>');
      continue;
    }
    if (/^\s*[-*]\s+/.test(line)) {
      out.push('<ul>');
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) { out.push('<li>' + inline(lines[i].replace(/^\s*[-*]\s+/, '')) + '</li>'); i++; }
      out.push('</ul>');
      continue;
    }
    if (/^\s*\d+\.\s+/.test(line)) {
      out.push('<ol>');
      while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) { out.push('<li>' + inline(lines[i].replace(/^\s*\d+\.\s+/, '')) + '</li>'); i++; }
      out.push('</ol>');
      continue;
    }
    if (/^>\s?/.test(line)) {
      const buf = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) { buf.push(lines[i].replace(/^>\s?/, '')); i++; }
      out.push('<blockquote>' + inline(buf.join(' ')) + '</blockquote>');
      continue;
    }
    const buf = [];
    while (i < lines.length && lines[i].trim() && !/^(\s*[-*]\s+|\s*\d+\.\s+|#{1,3}\s+|>|\||-{3,}\s*$)/.test(lines[i])) { buf.push(lines[i]); i++; }
    out.push('<p>' + inline(buf.join('<br>')) + '</p>');
  }
  return out.join('\n');
}

const CSS = [
  ":root { --ink:#16202e; --muted:#5b6b7f; --line:#d8e0ea; --accent:#1f6feb; --ok:#0f8a5f; --bg:#f4f7fb; }",
  "* { box-sizing: border-box; }",
  "body { margin:0; background:var(--bg); color:var(--ink); font-family:\"Microsoft YaHei\",\"Segoe UI\",system-ui,-apple-system,sans-serif; font-size:15px; line-height:1.75; }",
  ".sheet { max-width:1180px; margin:0 auto; background:#fff; padding:38px 46px 52px; }",
  ".kicker { display:flex; justify-content:space-between; align-items:center; border-bottom:3px solid var(--ink); padding-bottom:10px; margin-bottom:8px; }",
  ".kicker .brand { font-size:13px; letter-spacing:.18em; color:var(--muted); }",
  ".kicker .stamp { font-size:12px; color:var(--muted); }",
  "h1 { font-size:26px; margin:14px 0 6px; }",
  "h2 { font-size:19px; margin:30px 0 10px; padding-left:10px; border-left:4px solid var(--accent); }",
  "h3 { font-size:16px; margin:22px 0 8px; }",
  "p { margin:8px 0; }",
  "ul, ol { margin:8px 0 8px 4px; padding-left:22px; }",
  "li { margin:4px 0; }",
  "hr { border:0; border-top:1px dashed var(--line); margin:22px 0; }",
  "table { width:100%; border-collapse:collapse; margin:12px 0 18px; font-size:13.5px; }",
  "th, td { border:1px solid var(--line); padding:7px 9px; text-align:left; vertical-align:top; }",
  "th { background:#eef3fa; font-weight:600; }",
  "tbody tr:nth-child(even) { background:#fafcff; }",
  "code { background:#eef1f6; padding:1px 5px; border-radius:4px; font-family:Consolas,\"Courier New\",monospace; font-size:12.5px; }",
  "a { color:var(--accent); text-decoration:none; word-break:break-all; font-family:Consolas,\"Courier New\",monospace; font-size:12.5px; }",
  "blockquote { margin:12px 0; padding:8px 14px; background:#f7f9fc; border-left:4px solid var(--line); color:var(--muted); }",
  "footer { margin-top:34px; border-top:1px solid var(--line); padding-top:10px; font-size:12px; color:var(--muted); display:flex; justify-content:space-between; }"
].join('\n');

const DOCS = [
  { file: 'acceptance.md', out: 'day21-1-acceptance.html', kicker: 'DAY 21 · 周验收表 · 第 3 周（Day 15–21）' },
  { file: 'crosscheck.md', out: 'day21-2-crosscheck.html', kicker: 'DAY 21 · 同伴交叉验证 · 三行结论' },
  { file: 'demo-outline.md', out: 'day21-3-demo-outline.html', kicker: 'DAY 21 · 演示提纲 · 四段结构' },
];

const stamp = new Date().toISOString().replace('T', ' ').slice(0, 16) + ' UTC';
for (const d of DOCS) {
  const md = fs.readFileSync(path.join(root, 'docs', 'day21', d.file), 'utf8');
  const html = '<!DOCTYPE html>\n<html lang="zh-CN">\n<head>\n<meta charset="UTF-8">\n<title>' + d.kicker + '</title>\n<style>' + CSS + '</style>\n</head>\n<body>\n<div class="sheet">\n' +
    '<div class="kicker"><span class="brand">BLOG-MY · ' + d.kicker + '</span><span class="stamp">生成时间 ' + stamp + '</span></div>\n' +
    convert(md) +
    '\n<footer><span>blog-my · Day 21 交付</span><span>https://blog-my-check-10974.app.workbuddy.host</span></footer>\n</div>\n' +
    '<script>addEventListener("load",function(){document.title="H:"+document.documentElement.scrollHeight;});</script>\n</body>\n</html>\n';
  const dest = path.join(outDir, d.out);
  fs.writeFileSync(dest, html, 'utf8');
  console.log(dest);
}
