#!/usr/bin/env node
// Day 23 验收脚本：本地多模态 RAG 知识库端到端验收
//   登录/注册 -> 文本入库 -> 列表可见 -> 检索命中 -> 详情可查 -> 图片入库(多模态) -> 图片去重契约 -> 图片检索(尽力) -> 删除闭环 -> 契约边界(错误路径)
//
// 用法：
//   node scripts/verify-rag.mjs --base http://127.0.0.1:8080 \
//        --log docs/day23/verify-log.txt --transcript <临时目录>/rag-transcript.json [--ask]
//
// 参数：
//   --base        后端地址，默认 http://127.0.0.1:8080
//   --log         日志文件路径（可空：不传或给空值就不写）
//   --transcript  证据 JSON 路径（可空：不传或给空值就不写）
//   --username    登录/注册用的账号，默认 kbverify
//   --password    登录/注册用的密码，默认 kbverify123456
//   --ask         额外跑一次 /api/kb/ask 的 SSE 契约冒烟（默认关闭；该项最多记 WARN，不会 FAIL）
//   --model       /api/kb/ask 使用的模型名，默认 qwen2.5:7b
//   --upload-timeout  图片上传（会现场调用本地视觉模型）的 HTTP 超时毫秒数，默认 300000
//
// 关于 --upload-timeout：图片入库要先让 qwen2.5vl:7b 看图生成描述，这一步是整条链路里最慢的。
// 本机实测：冷启动（模型首次加载进显存）一次约 56 秒，远超普通 JSON 接口的 20 秒窗口。
// 后端自己的视觉超时是 5 分钟（internal/rag/embed.go 的 OllamaConfig.withDefaults），所以这里默认也给 5 分钟，
// 两边对齐；调小它只会让脚本先于后端放弃，把一个其实正常的慢请求记成 FAIL。
//
// 登录：知识库接口在 Go 后端下和其它 /api 接口一样是需要登录的。
//   先 POST /login（application/x-www-form-urlencoded，字段 username/password）；
//   若登录没有发回 session（例如账号还不存在），再 POST /register 用同样账密注册；
//   注册若只建账号没发 session，则回头再 POST /login 一次。
//   从 Set-Cookie 里取出 session=...，之后所有 /api/kb/* 请求都带 Cookie: session=...。
//
// 为什么用 node:http 而不是全局 fetch：Windows 上 fetch（undici）在进程退出时会触发 libuv 断言
// （!(handle->flags & UV_HANDLE_CLOSING)），退出码变得不可靠。这里照 scripts/verify-day22.mjs 的做法，
// 用 node:http + agent:false 的短连接，socket 收尾干净、退出码可信。
//
// 中文编码：所有请求体都在 Node 侧用 Buffer.from(..., 'utf8') 组装，不经过 PowerShell，避免
// PowerShell 5.1 把中文按 ASCII 发出去（历史上会把标题写成 '?'）。控制台上若看到中文乱码那是终端
// 代码页的问题，先 chcp 65001；写进日志/transcript 的始终是 UTF-8。
//
// 判定口径：PASS / FAIL / WARN 三种。核心流程不满足记 FAIL；"图片能否被检索"这类依赖视觉模型输出、
// 无法预先断言的项目记 WARN。WARN 不影响退出码：只要有 FAIL 退出码为 1，否则 0。

import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';

// ---------- 参数解析 ----------
const args = {};
const BOOL_FLAGS = new Set(['ask', 'help']);
for (let i = 2; i < process.argv.length; i++) {
  const a = process.argv[i];
  if (!a.startsWith('--')) continue;
  const key = a.slice(2);
  const next = process.argv[i + 1];
  // 布尔开关，或后面没有值（或下一个又是 -- 参数）：当作 true
  if (BOOL_FLAGS.has(key) || next === undefined || next.startsWith('--')) {
    args[key] = true;
    continue;
  }
  args[key] = next;
  i++;
}

const base = String(args.base || 'http://127.0.0.1:8080').replace(/\/+$/, '');
const logPath = typeof args.log === 'string' ? args.log : '';
const transcriptPath = typeof args.transcript === 'string' ? args.transcript : '';
const username = typeof args.username === 'string' ? args.username : 'kbverify';
const password = typeof args.password === 'string' ? args.password : 'kbverify123456';
const askEnabled = args.ask === true;
const askModel = typeof args.model === 'string' ? args.model : 'qwen2.5:7b';
const TIMEOUT_MS = 20000;
const SSE_TIMEOUT_MS = 120000;
// 上传会触发视觉模型推理，用单独的（更长的）超时；默认与后端视觉超时 5 分钟对齐
const uploadTimeoutMs = Number.isFinite(Number(args['upload-timeout'])) && Number(args['upload-timeout']) > 0
  ? Number(args['upload-timeout'])
  : 300000;

if (!/^http:\/\//i.test(base)) {
  console.error(`[FAIL] --base 只支持 http:// 地址（脚本用 node:http，不用 fetch/https），当前：${base}`);
  process.exit(1);
}

// ---------- 输出与判定 ----------
const lines = [];
const checks = [];
const steps = [];
const say = (s) => { lines.push(s); console.log(s); };

function record(level, name, expect, actual) {
  checks.push({ level, name, expect: String(expect), actual: String(actual) });
  say(`[${level}] ${name}  期望=${expect} 实际=${actual}`);
}
const pass = (name, expect, actual) => record('PASS', name, expect, actual);
const fail = (name, expect, actual) => record('FAIL', name, expect, actual);
const warn = (name, expect, actual) => record('WARN', name, expect, actual);
// soft=true 时，不满足只记 WARN（用于"尽力而为"的检查）
function check(name, expect, actual, okFlag, soft) {
  if (okFlag) pass(name, expect, actual);
  else if (soft) warn(name, expect, actual);
  else fail(name, expect, actual);
  return !!okFlag;
}

const clip = (v, n = 1200) => {
  const s = v === undefined || v === null ? '' : String(v);
  return s.length > n ? `${s.slice(0, n)}...(+${s.length - n} chars)` : s;
};
const oneLine = (v, n = 100) => clip(String(v == null ? '' : v).replace(/\s+/g, ' ').trim(), n);
const parseJson = (text) => { try { return JSON.parse(text); } catch { return null; } };
const num = (v) => (typeof v === 'number' ? v : v === undefined || v === null ? '(缺失)' : JSON.stringify(v));
const maskSession = (cookie) => {
  const v = String(cookie).replace(/^session=/, '');
  return `session=${v.slice(0, 8)}...(共 ${v.length} 字符)`;
};

// ---------- HTTP：node:http + agent:false，自动收尾，超时可控 ----------
let sessionCookie = ''; // 登录后从 Set-Cookie 取出的 "session=..."，非空时所有请求自动带上

function httpReq(method, p, { body, headers, timeoutMs = TIMEOUT_MS } = {}) {
  return new Promise((resolve) => {
    const u = new URL(base + p);
    const req = http.request(
      {
        hostname: u.hostname,
        port: u.port || 80,
        path: u.pathname + u.search,
        method,
        headers: headers || {},
        agent: false, // 一次性短连接：不留 keep-alive socket，进程才能干净退出
        timeout: timeoutMs,
      },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => resolve({ status: res.statusCode, text: Buffer.concat(chunks).toString('utf8'), headers: res.headers }));
      }
    );
    // 网络错误（连不上、被拒、超时）不当异常抛出，而是回一个 status=0，让每条 check 都能如实报 FAIL
    req.on('error', (e) => resolve({ status: 0, text: '', headers: {}, error: e.message }));
    req.on('timeout', () => req.destroy(new Error(`请求超时（${timeoutMs}ms）`)));
    if (body !== undefined) req.write(body);
    req.end();
  });
}

// JSON 请求：请求体是 UTF-8 Buffer，确保中文不经任何 ASCII 转换；带上 session cookie
async function reqJson(method, p, body) {
  const headers = {};
  if (sessionCookie) headers.Cookie = sessionCookie;
  let payload;
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json; charset=utf-8';
    payload = Buffer.from(JSON.stringify(body), 'utf8');
  }
  const res = await httpReq(method, p, { body: payload, headers });
  steps.push({
    method,
    path: p,
    status: res.status,
    error: res.error || undefined,
    requestBody: payload ? clip(payload.toString('utf8'), 600) : undefined,
    responseBody: clip(res.text),
  });
  return res;
}

// 表单请求（登录/注册用）：application/x-www-form-urlencoded
async function reqForm(p, fields) {
  const body = Buffer.from(
    Object.entries(fields).map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`).join('&'),
    'utf8'
  );
  const res = await httpReq('POST', p, {
    body,
    headers: { 'Content-Type': 'application/x-www-form-urlencoded; charset=utf-8' },
  });
  steps.push({
    method: 'POST',
    path: p,
    status: res.status,
    error: res.error || undefined,
    location: res.headers.location || undefined,
    requestBody: clip(body.toString('utf8'), 300),
    setCookie: (Array.isArray(res.headers['set-cookie']) ? res.headers['set-cookie'] : res.headers['set-cookie'] ? [res.headers['set-cookie']] : []).map((c) => clip(c, 120)),
    responseBody: clip(res.text, 600),
  });
  return res;
}

// 从 Set-Cookie 里挑出 session=...，避免误匹配 xsession= 之类的名字
function pickSession(res) {
  const raw = res.headers['set-cookie'];
  const list = Array.isArray(raw) ? raw : raw ? [raw] : [];
  for (const c of list) {
    const m = /(?:^|;\s*)session=([^;]+)/i.exec(c);
    if (m) return `session=${m[1]}`;
  }
  return '';
}

// ---------- multipart/form-data：node:http 没有 FormData，手写 ----------
function buildMultipart({ name, filename, contentType, bytes, boundary }) {
  const head = Buffer.from(
    `--${boundary}\r\n` +
      `Content-Disposition: form-data; name="${name}"; filename="${filename}"\r\n` +
      `Content-Type: ${contentType}\r\n\r\n`,
    'utf8'
  );
  const tail = Buffer.from(`\r\n--${boundary}--\r\n`, 'utf8');
  // 整段都是 Buffer，Buffer.concat 的结果长度 == 字节数，可直接当 Content-Length
  return Buffer.concat([head, bytes, tail]);
}

// ---------- SSE 解析：把 "event: X\ndata: Y\n\n" 流拆成事件数组 ----------
function parseSse(text) {
  const events = [];
  let cur = { event: '', data: [] };
  const flush = () => {
    if (cur.event || cur.data.length) events.push({ event: cur.event || 'message', data: cur.data.join('\n') });
    cur = { event: '', data: [] };
  };
  for (const rawLine of String(text).split(/\r?\n/)) {
    if (rawLine === '') { flush(); continue; }         // 空行 = 事件边界
    if (rawLine.startsWith(':')) continue;             // 注释 / 心跳
    const idx = rawLine.indexOf(':');
    const field = idx === -1 ? rawLine : rawLine.slice(0, idx);
    let value = idx === -1 ? '' : rawLine.slice(idx + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'event') cur.event = value;
    else if (field === 'data') cur.data.push(value);
  }
  flush();
  return events;
}

// ---------- 内置素材：8x8 真彩色 PNG（166 字节），现场 base64 解码，不依赖任何外部文件 ----------
// 像素是 x/y 双通道渐变 + 固定蓝，视觉模型有东西可描述；已验证签名/IHDR/IDAT 可解。
// PNG_BASE 是固定底图，真正的上传素材在 runTag 算出后再由 buildRunPng() 逐运行派生。
const PNG_B64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAbUlEQVR42g3NQQEAMAgDsUqpFKQgBSmVwvNkIGWLgUjCokSLERErTkjGpkybMTFrzkiFiyq6mCLFFldIjZtqupkmzTbXSIOHGnqYIcMON/8IDhU6TEjYcPnH4qWWXmbJssvtPw4fdfQxR4497ni85GoBf4s30gAAAABJRU5ErkJggg==';
const PNG_BASE = Buffer.from(PNG_B64, 'base64');
const PNG_SIG = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

// PNG 块校验用的 CRC32（照 RFC 1952 的表驱动实现；不依赖 zlib.crc32，免得挑 Node 版本）
const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();
function pngCrc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}
// 造一个 PNG tEXt 辅助块（keyword\\u0000text）。tEXt 是标准辅助块，解码器不认识也会直接跳过，
// 图片本身完全不变；用它把本次运行的 runTag 写进文件字节，从而让 sha256 逐运行不同。
function pngTextChunk(keyword, text) {
  const data = Buffer.concat([Buffer.from(keyword + '\u0000', 'latin1'), Buffer.from(text, 'latin1')]);
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length, 0);
  const type = Buffer.from('tEXt', 'latin1');
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(pngCrc32(Buffer.concat([type, data])), 0);
  return Buffer.concat([len, type, data, crc]);
}
// 把底图和本运行专属的 tEXt 块拼起来，返回本次真正上传的 PNG 字节。
function buildRunPng(tag) {
  const iend = PNG_BASE.subarray(PNG_BASE.length - 12);
  const ok = iend.length === 12 && iend.subarray(4, 8).toString('latin1') === 'IEND';
  if (!ok) throw new Error('内置 PNG 底图结构异常：末尾 12 字节不是 IEND 块');
  return Buffer.concat([PNG_BASE.subarray(0, PNG_BASE.length - 12), pngTextChunk('Comment', 'day23-verify-' + tag), iend]);
}

// ============================ 主流程 ============================
const stamp = new Date().toISOString();
const d = new Date();
const pad = (n) => String(n).padStart(2, '0');
const runTag = `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
const secret = `紫罗兰-${runTag}`;                 // 暗号词：几乎只可能出现在本次验收文本里
const title = `Day 23 验收：本地多模态知识库文本 ${runTag}`;
const pngName = `verify-day23-${runTag}.png`;
const pngQuery = pngName.replace(/\.png$/i, '');    // 图片检索用"文件名（不含扩展名）"

// 本次运行真正上传的图片字节：底图 + 带 runTag 的 tEXt 块。
// 每次运行的字节都不同，才不会被 kb_document.sha256 去重命中 —— 否则第二次运行会拿到
// 上一次留下的图片文档（200 + duplicated=true + 旧 id），本次的"图片入库"其实没有发生。
const PNG_BYTES = buildRunPng(runTag);

const seedSentences = [
  '本地多模态知识库同时索引纯文本与图片描述：文本块直接向量化，图片块靠视觉模型生成的描述参与召回。',
  '切块策略决定了召回的颗粒度，块太大容易带入无关内容，块太小又会把一句完整的话切碎，语义就不完整了。',
  '真实场景里同一段话常常被切成两块，所以验收脚本必须容忍同一暗号词多次命中，只看排在第一位的命中。',
  '检索链路一般是先用向量做粗召回、再用重排把最相关的一块顶到最前面，粗召回负责不走漏，重排负责排得准。',
];
const paragraphs = [];
for (let i = 1; i <= 16; i++) {
  paragraphs.push(`【验收段落 ${pad(i)}】${seedSentences[(i - 1) % seedSentences.length]}（本段序号 ${i}，专门用来把正文撑长以便切出多块。）`);
}
const textBody = [
  `本文件是 Day 23 端到端验收用的临时文本，暗号词是 ${secret}。这个暗号词只应出现在这一份文本里，任何其他文档都不该包含它。`,
  ...paragraphs,
  `结尾再次确认暗号词：${secret}。如果检索结果排在第一位的文本里读不到它，说明入库或召回链路有问题。`,
].join('\n\n');

const out = {}; // 收集给 transcript 的证据
let textId = null;
let imageId = null;

function needTextId(step) {
  if (textId !== null && textId !== undefined) return false;
  fail(`${step}：跳过后续校验`, '已有文本文档 id', '未拿到 id（入库失败）');
  return true;
}

async function runAll() {
  say('== Day 23 验收：本地多模态 RAG 知识库端到端 ==');
  say(`Base = ${base}`);
  say(`时间 = ${stamp}`);
  say('');

  // ---- 自检：内置 PNG 素材是否完好（脚本自身正确性，和接口无关）----
  const pngLooksOk = PNG_BYTES.length > 8 && PNG_BYTES.subarray(0, 8).equals(PNG_SIG);
  if (!pngLooksOk) {
    fail('[自检] 内置 PNG 素材损坏', 'PNG 签名匹配', `${PNG_BYTES.length} 字节，签名不匹配`);
  } else {
    say(`[自检] 内置 PNG 素材 ${PNG_BYTES.length} 字节（底图 ${PNG_BASE.length} + 本次运行专属 tEXt 块），尺寸 ${PNG_BYTES.readUInt32BE(16)}x${PNG_BYTES.readUInt32BE(20)}，签名正确`);
    const tailType = PNG_BYTES.subarray(PNG_BYTES.length - 8, PNG_BYTES.length - 4).toString('latin1');
    if (tailType !== 'IEND') {
      fail('[自检] 派生 PNG 末尾不是 IEND 块', 'IEND', tailType);
    } else {
      say(`[自检] 派生 PNG 末尾 IEND 块完好，tEXt 标记 = day23-verify-${runTag}（逐运行不同，避免被他次运行的 sha256 去重命中）`);
    }
  }

  // ================= 第 0 步：登录 / 注册（/api/kb/* 需要登录）=================
  say('');
  say('-- 第 0 步：登录 / 注册（知识库接口需要登录）--');
  say(`  账号 = ${username}  密码 = ${'*'.repeat(password.length)}`);
  const login = await reqForm('/login', { username, password });
  let session = pickSession(login);
  let authPath = session ? 'login' : '';
  let registerStatus = null;
  say(`  POST /login -> status=${login.status}${login.headers.location ? `  location=${login.headers.location}` : ''}  session=${session ? maskSession(session) : '(无)'}`);
  // 登录返回 200 但没有下发 session（常见于账号还不存在）：用同样账密注册
  if (!session) {
    const register = await reqForm('/register', { username, password });
    registerStatus = register.status;
    session = pickSession(register);
    if (session) authPath = 'register';
    say(`  POST /register -> status=${register.status}${register.headers.location ? `  location=${register.headers.location}` : ''}  session=${session ? maskSession(session) : '(无)'}`);
    // 注册只建了账号、没发 session 的情况：回头再登录一次
    if (!session) {
      const login2 = await reqForm('/login', { username, password });
      session = pickSession(login2);
      if (session) authPath = 'login-after-register';
      say(`  再 POST /login -> status=${login2.status}${login2.headers.location ? `  location=${login2.headers.location}` : ''}  session=${session ? maskSession(session) : '(无)'}`);
    }
  }
  sessionCookie = session;
  check('拿到 session cookie（登录或注册）', 'session=...', session ? maskSession(session) : '(两次都没拿到)',
    !!session);
  if (session) say(`  认证方式 = ${authPath}；后续 /api/kb/* 请求都会带 Cookie: ${maskSession(session)}`);
  out.auth = {
    username,
    loginStatus: login.status,
    loginLocation: login.headers.location || '',
    registerStatus,
    path: authPath || null,
    cookie: session ? maskSession(session) : '',
  };

  // ================= 第 1 步：探活 =================
  say('');
  say('-- 第 1 步：探活 --');
  const health = await reqJson('GET', '/api/health');
  const healthBody = parseJson(health.text);
  check('探活 GET /api/health 返回 200', 200, health.status, health.status === 200);
  check('探活响应 status="ok"', 'ok', num(healthBody && healthBody.status), !!(healthBody && healthBody.status === 'ok'));
  if (health.error) say(`  连接错误：${health.error}`);
  say(`  探活响应（原样）：${oneLine(health.text, 200)}`);

  // ================= 第 2 步：文本入库 =================
  say('');
  say('-- 第 2 步：文本入库（唯一中文长文本 + 暗号词）--');
  say(`  暗号词 = ${secret}`);
  say(`  标题 = "${title}"`);
  say(`  正文长度 = ${textBody.length} 字符`);
  const created = await reqJson('POST', '/api/kb/text', { title, text: textBody });
  const createdBody = parseJson(created.text);
  textId = createdBody && createdBody.id !== undefined ? createdBody.id : null;
  check('文本入库返回 201', 201, created.status, created.status === 201);
  check('文本入库 ok=true', true, num(createdBody && createdBody.ok), !!(createdBody && createdBody.ok === true));
  check('文本入库返回 id', 'id 非空', num(textId), textId !== null);
  check('文本入库 chunk_count >= 1', '>=1', num(createdBody && createdBody.chunk_count),
    !!(createdBody && typeof createdBody.chunk_count === 'number' && createdBody.chunk_count >= 1));
  // 中文编码的关键断言：回显的标题必须和请求逐字一致（若被按 ASCII 发出会变成 '?'）
  check('入库回显 title 与请求逐字一致（中文未损坏）', title, clip(createdBody && createdBody.title, 60),
    !!(createdBody && createdBody.title === title));
  say(`  入库响应（原样）：${oneLine(created.text, 300)}`);
  say(`  文档 id=${num(textId)}  chunk_count=${num(createdBody && createdBody.chunk_count)}`);
  out.text = { id: textId, title, secret, textLength: textBody.length, response: createdBody, raw: clip(created.text, 2000) };

  // ================= 第 3 步：列表可见 =================
  say('');
  say('-- 第 3 步：列表可见 --');
  if (!needTextId('第 3 步')) {
    const list = await reqJson('GET', '/api/kb/documents');
    const listBody = parseJson(list.text);
    const items = listBody && Array.isArray(listBody.data) ? listBody.data : [];
    check('列表接口返回 200', 200, list.status, list.status === 200);
    check('列表 ok=true', true, num(listBody && listBody.ok), !!(listBody && listBody.ok === true));
    const found = items.find((x) => x.id === textId);
    check('列表里能找到刚写入的 id', textId, found ? `id=${found.id}` : '(未找到)', !!found);
    say(`  列表共 ${items.length} 条`);
    if (found) say(`  该条：kind=${found.kind} chunk_count=${found.chunk_count} created_at=${found.created_at} title="${found.title}"`);
    out.list = { count: items.length, found: found || null, ids: items.map((x) => x.id) };
  }

  // ================= 第 4 步：检索命中 =================
  say('');
  say('-- 第 4 步：检索命中（query = 暗号词）--');
  if (!needTextId('第 4 步')) {
    const search = await reqJson('POST', '/api/kb/search', { query: secret, top_k: 5 });
    const sBody = parseJson(search.text);
    const hits = sBody && Array.isArray(sBody.hits) ? sBody.hits : [];
    check('检索接口返回 200', 200, search.status, search.status === 200);
    check('检索有命中（hits 非空）', 'hits.length >= 1', hits.length, hits.length >= 1);
    const top = hits[0];
    const topText = top && typeof top.text === 'string' ? top.text : '';
    check('排在第一位的命中包含暗号词', `含 ${secret}`, oneLine(topText, 80), topText.includes(secret));
    check('首位命中的 document_id 指向刚写入的文档', textId, num(top && top.document_id), !!(top && top.document_id === textId));
    say(`  首位命中：score=${num(top && top.score)}  chunk_id=${num(top && top.chunk_id)}  kind=${num(top && top.kind)}  document_id=${num(top && top.document_id)}`);
    say(`  命中总数=${hits.length}，各条 score=${hits.map((h) => num(h.score)).join(', ')}`);
    out.search = {
      query: secret,
      hitCount: hits.length,
      top: top ? { document_id: top.document_id, chunk_id: top.chunk_id, score: top.score, kind: top.kind, text: clip(top.text, 400) } : null,
      scores: hits.map((h) => h.score),
    };
  }

  // ================= 第 5 步：详情可查 =================
  say('');
  say('-- 第 5 步：详情可查 --');
  if (!needTextId('第 5 步')) {
    const detail = await reqJson('GET', `/api/kb/documents/${textId}`);
    const dBody = parseJson(detail.text);
    const dData = dBody && dBody.data;
    const chunks = dData && Array.isArray(dData.chunks) ? dData.chunks : [];
    const joined = chunks.map((c) => (c && typeof c.text === 'string' ? c.text : '')).join('\n');
    check('详情接口返回 200', 200, detail.status, detail.status === 200);
    check('详情返回 id 与写入一致', textId, num(dData && dData.id), !!(dData && dData.id === textId));
    check('详情 kind 为 text', 'text', num(dData && dData.kind), !!(dData && dData.kind === 'text'));
    check('详情返回的 chunks 非空', 'chunks.length >= 1', chunks.length, chunks.length >= 1);
    check('chunks 拼接后包含暗号词', `含 ${secret}`, joined.includes(secret) ? '包含' : '不包含', joined.includes(secret));
    say(`  chunks 数量=${chunks.length}，前几个 seq:kind = ${chunks.slice(0, 4).map((c) => `${num(c && c.seq)}:${num(c && c.kind)}`).join('  ')}`);
    say(`  第一个 chunk 片段：${oneLine(chunks.length ? chunks[0].text : '(空)', 100)}`);
    out.detail = {
      id: textId,
      kind: dData && dData.kind,
      chunkCount: chunks.length,
      containsSecret: joined.includes(secret),
      seqKinds: chunks.map((c) => ({ seq: c && c.seq, kind: c && c.kind })),
    };
  }

  // ================= 第 6 步：图片入库（多模态）=================
  say('');
  say('-- 第 6 步：图片入库（multipart/form-data，字段名 file）--');
  const boundary = `----day23kb'${Date.now()}`;
  const mpBody = buildMultipart({ name: 'file', filename: pngName, contentType: 'image/png', bytes: PNG_BYTES, boundary });
  // Content-Length 必须是字节数：这里 body 由 Buffer.concat 得到，.length 就是字节数
  const mpHeaders = {
    'Content-Type': `multipart/form-data; boundary=${boundary}`,
    'Content-Length': String(mpBody.length),
  };
  if (sessionCookie) mpHeaders.Cookie = sessionCookie;
  say(`  上传文件名 = ${pngName}，multipart 体积 = ${mpBody.length} 字节（Content-Length 用字节数，不用字符数）`);
  say(`  上传超时 = ${uploadTimeoutMs}ms（视觉模型推理较慢，可用 --upload-timeout 调整）`);
  const uploaded = await httpReq('POST', '/api/kb/documents', { body: mpBody, headers: mpHeaders, timeoutMs: uploadTimeoutMs });
  steps.push({
    method: 'POST',
    path: '/api/kb/documents',
    status: uploaded.status,
    error: uploaded.error || undefined,
    requestBody: `[multipart ${mpBody.length} bytes, boundary=${boundary}, filename=${pngName}]`,
    responseBody: clip(uploaded.text),
  });
  const upBody = parseJson(uploaded.text);
  imageId = upBody && upBody.id !== undefined ? upBody.id : null;
  check('图片上传返回 201', 201, uploaded.status, uploaded.status === 201);
  check('图片上传 ok=true', true, num(upBody && upBody.ok), !!(upBody && upBody.ok === true));
  check('上传后 kind 判定为 image', 'image', num(upBody && upBody.kind), !!(upBody && upBody.kind === 'image'));
  check('图片入库 chunk_count >= 1', '>=1', num(upBody && upBody.chunk_count),
    !!(upBody && typeof upBody.chunk_count === 'number' && upBody.chunk_count >= 1));
  if (uploaded.error) say(`  连接错误：${uploaded.error}`);
  // 图片块的数量与文本内容取决于视觉模型，这里只如实打印，不断言
  say(`  图片入库响应（原样）：${oneLine(uploaded.text, 400)}`);
  say(`  图片文档 id=${num(imageId)}  chunk_count=${num(upBody && upBody.chunk_count)}（数量由视觉模型/切块策略决定，不做断言）`);
  out.image = { id: imageId, filename: pngName, pngBytes: PNG_BYTES.length, response: upBody, raw: clip(uploaded.text, 2000) };

  // ---- 附：去重契约（同一份字节传两次，第二次应命中 sha256 去重，返回 200 + duplicated=true + 同一个 id）----
  // 这一步同时说明了"素材为什么要逐运行变化"：去重是按内容的，固定素材会让第二次运行直接复用旧文档。
  say('');
  say('-- 附：图片去重契约（同一份字节再传一次）--');
  const boundary2 = `----day23kbdup'${Date.now()}`;
  const mpBody2 = buildMultipart({ name: 'file', filename: pngName, contentType: 'image/png', bytes: PNG_BYTES, boundary: boundary2 });
  const dupHeaders = {
    'Content-Type': `multipart/form-data; boundary=${boundary2}`,
    'Content-Length': String(mpBody2.length),
  };
  if (sessionCookie) dupHeaders.Cookie = sessionCookie;
  const dup = await httpReq('POST', '/api/kb/documents', { body: mpBody2, headers: dupHeaders, timeoutMs: uploadTimeoutMs });
  steps.push({
    method: 'POST',
    path: '/api/kb/documents',
    status: dup.status,
    error: dup.error || undefined,
    requestBody: `[multipart ${mpBody2.length} bytes, 与上一步同一份字节，用于触发去重]`,
    responseBody: clip(dup.text),
  });
  const dupBody = parseJson(dup.text);
  say(`  去重响应（原样）：${oneLine(dup.text, 300)}`);
  check('重复上传同一份字节返回 200（而非 201）', 200, dup.status, dup.status === 200);
  check('重复上传 duplicated=true', true, num(dupBody && dupBody.duplicated), !!(dupBody && dupBody.duplicated === true));
  check('重复上传返回的是同一个文档 id', num(imageId), num(dupBody && dupBody.id), !!(dupBody && dupBody.id === imageId));
  out.imageDedup = { status: dup.status, duplicated: dupBody && dupBody.duplicated, id: dupBody && dupBody.id, sameId: !!(dupBody && dupBody.id === imageId) };

  // ================= 第 7 步：图片可被检索（尽力而为，只记 PASS / WARN）=================
  say('');
  say('-- 第 7 步：图片可被检索（尽力而为：视觉描述不可预测，没命中记 WARN 不记 FAIL）--');
  const imgSearch = await reqJson('POST', '/api/kb/search', { query: pngQuery, top_k: 5 });
  const iBody = parseJson(imgSearch.text);
  const iHits = iBody && Array.isArray(iBody.hits) ? iBody.hits : [];
  check('按图片文件名检索能返回 200', 200, imgSearch.status, imgSearch.status === 200, true);
  check('按图片文件名检索有命中（尽力而为）', 'hits.length >= 1', iHits.length, iHits.length >= 1, true);
  if (iHits.length) {
    say(`  命中 ${iHits.length} 条，首位：score=${num(iHits[0].score)}  document_id=${num(iHits[0].document_id)}  kind=${num(iHits[0].kind)}`);
    say(`  首位文本片段：${oneLine(iHits[0].text, 120)}`);
    say(`  命中文档 id 列表：${iHits.map((h) => num(h.document_id)).join(', ')}（图片文档 id=${num(imageId)}）`);
  } else {
    say('  未命中（视觉描述里没带文件名属正常情况，记为 WARN，不影响退出码）');
  }
  // 再补一刀：用中文"图片"搜一次，纯参考输出
  const imgSearch2 = await reqJson('POST', '/api/kb/search', { query: '图片', top_k: 5 });
  const i2Body = parseJson(imgSearch2.text);
  const i2Hits = i2Body && Array.isArray(i2Body.hits) ? i2Body.hits : [];
  say(`  参考：query="图片" -> status=${imgSearch2.status} 命中 ${i2Hits.length} 条，首位 score=${num(i2Hits.length ? i2Hits[0].score : undefined)}，首位片段：${oneLine(i2Hits.length ? i2Hits[0].text : '(无)', 100)}`);
  out.imageSearch = {
    filenameQuery: {
      query: pngQuery,
      status: imgSearch.status,
      hitCount: iHits.length,
      hits: iHits.map((h) => ({ document_id: h.document_id, score: h.score, kind: h.kind, text: clip(h.text, 300) })),
    },
    chineseQuery: {
      query: '图片',
      status: imgSearch2.status,
      hitCount: i2Hits.length,
      hits: i2Hits.map((h) => ({ document_id: h.document_id, score: h.score, kind: h.kind, text: clip(h.text, 300) })),
    },
  };

  // ================= 附（可选，--ask）：/api/kb/ask SSE 契约冒烟 =================
  if (askEnabled) {
    say('');
    say('-- 附（--ask）：/api/kb/ask SSE 契约冒烟（最多记 WARN）--');
    const askReqBody = { query: secret, model: askModel, top_k: 5 };
    const payload = Buffer.from(JSON.stringify(askReqBody), 'utf8');
    const askHeaders = { 'Content-Type': 'application/json; charset=utf-8', Accept: 'text/event-stream' };
    if (sessionCookie) askHeaders.Cookie = sessionCookie;
    const askRes = await httpReq('POST', '/api/kb/ask', { body: payload, headers: askHeaders, timeoutMs: SSE_TIMEOUT_MS });
    const events = parseSse(askRes.text);
    const names = events.map((e) => e.event);
    const deltas = events.filter((e) => e.event === 'delta');
    const answer = deltas
      .map((e) => { const j = parseJson(e.data); return j && typeof j.content === 'string' ? j.content : ''; })
      .join('');
    steps.push({
      method: 'POST',
      path: '/api/kb/ask',
      status: askRes.status,
      error: askRes.error || undefined,
      requestBody: clip(payload.toString('utf8'), 300),
      responseBody: clip(askRes.text, 2000),
    });
    check('ask 返回 200', 200, askRes.status, askRes.status === 200, true);
    check('ask 第一个事件是 sources', 'sources', names[0] || '(无事件)', names[0] === 'sources', true);
    check('ask 至少有一个 delta 事件', '含 delta', names.includes('delta') ? '有' : '无', names.includes('delta'), true);
    check('ask 以 done 事件结束', 'done', names[names.length - 1] || '(无)', names[names.length - 1] === 'done', true);
    const sourcesEv = events.find((e) => e.event === 'sources');
    const sources = sourcesEv ? parseJson(sourcesEv.data) : null;
    const srcHits = sources && Array.isArray(sources.hits) ? sources.hits : [];
    say(`  事件序列：${names.join(' -> ') || '(空)'}（共 ${events.length} 个事件，delta ${deltas.length} 个）`);
    say(`  sources.hits 数量=${srcHits.length}${srcHits.length ? `，首位 document_id=${num(srcHits[0].document_id)} score=${num(srcHits[0].score)}` : ''}`);
    say(`  拼接出的回答（前 160 字）：${oneLine(answer, 160)}`);
    say(`  SSE 原文（截断）：${oneLine(askRes.text, 300)}`);
    out.ask = { status: askRes.status, model: askModel, events: names, deltaCount: deltas.length, sourcesCount: srcHits.length, answer: clip(answer, 500) };
  } else {
    out.ask = { skipped: true, hint: '加 --ask 可跑 /api/kb/ask 的 SSE 契约冒烟' };
  }

  // ================= 第 8 步：删除闭环 =================
  say('');
  say('-- 第 8 步：删除闭环（DELETE 文本文档 -> 列表消失 -> 再删 404）--');
  if (!needTextId('第 8 步')) {
    const del = await reqJson('DELETE', `/api/kb/documents/${textId}`);
    const delBody = parseJson(del.text);
    check('DELETE 返回 200', 200, del.status, del.status === 200);
    check('DELETE 响应 ok=true', true, num(delBody && delBody.ok), !!(delBody && delBody.ok === true));
    check('DELETE 响应 deleted=true', true, num(delBody && delBody.deleted), !!(delBody && delBody.deleted === true));
    check('DELETE 响应的 id 与请求一致', textId, num(delBody && delBody.id), !!(delBody && delBody.id === textId));

    const list2 = await reqJson('GET', '/api/kb/documents');
    const l2Body = parseJson(list2.text);
    const l2Items = l2Body && Array.isArray(l2Body.data) ? l2Body.data : [];
    const stillThere = l2Items.some((x) => x.id === textId);
    check('删除后列表里不再有该 id', '不包含', stillThere ? '仍包含' : '不包含', !stillThere);

    const detail2 = await reqJson('GET', `/api/kb/documents/${textId}`);
    check('删除后详情返回 404', 404, detail2.status, detail2.status === 404);

    const delAgain = await reqJson('DELETE', `/api/kb/documents/${textId}`);
    check('重复 DELETE 返回 404', 404, delAgain.status, delAgain.status === 404);
    say(`  删除响应（原样）：${oneLine(del.text, 200)}`);
    say(`  删除后列表共 ${l2Items.length} 条`);
    out.del = { id: textId, response: delBody, listCountAfter: l2Items.length, detailAfterStatus: detail2.status, deleteAgainStatus: delAgain.status };
  }

  // 收尾提示：图片文档不做删除（本脚本只验文本删除闭环），留作证据
  if (imageId !== null) {
    say('');
    say(`  提示：图片文档 id=${imageId} 未删除，留作多模态证据；如需清理可手动 DELETE /api/kb/documents/${imageId}`);
  }

  // 若出现 401/403，多半是没登录上或 session 失效，给一条排查提示
  const unauthorized = steps.filter((s) => s.status === 401 || s.status === 403);
  if (unauthorized.length) {
    say('');
    say(`  排查提示：有 ${unauthorized.length} 个请求返回 401/403（${unauthorized.map((s) => s.path).join(', ')}）。`);
    say('  多半是登录/注册没拿到 session，或后端登录字段名不是 username/password；可用 --username/--password 换一对账号重试。');
  }

  // ================= 第 9 步：契约边界（错误路径；都不触发视觉/生成，很快）=================
  say('');
  say('-- 第 9 步：契约边界（未登录 / 非法 id / 空 query / 非法 JSON / 不支持的类型 / 未知模型）--');
  const edge = {};
  const jsonErr = (res) => { const b = parseJson(res.text); return b && typeof b.error === 'string' ? b.error : ''; };

  // 9.1 未登录：这一条故意不带 Cookie，验证 /api 分组统一返回 JSON 401 而不是 302 跳登录
  const anon = await httpReq('GET', '/api/kb/documents');
  steps.push({ method: 'GET', path: '/api/kb/documents', status: anon.status, requestBody: '(故意不带 Cookie)', responseBody: clip(anon.text) });
  edge.anonymous = { status: anon.status, error: jsonErr(anon) };
  check('未登录访问 /api/kb/* 返回 401', 401, anon.status, anon.status === 401);
  check('未登录错误体为 {"error":"未登录"}', '未登录', jsonErr(anon), jsonErr(anon) === '未登录');

  // 9.2 / 9.3 路径参数
  const badId = await reqJson('GET', '/api/kb/documents/abc');
  edge.badId = { status: badId.status, error: jsonErr(badId) };
  check('非数字 id 返回 400', 400, badId.status, badId.status === 400);
  check('非数字 id 的错误信息为「id 非法」', 'id 非法', jsonErr(badId), jsonErr(badId) === 'id 非法');

  const noDoc = await reqJson('GET', '/api/kb/documents/999999999');
  edge.missing = { status: noDoc.status, error: jsonErr(noDoc) };
  check('不存在的文档返回 404', 404, noDoc.status, noDoc.status === 404);
  check('不存在文档的错误信息为「文档不存在」', '文档不存在', jsonErr(noDoc), jsonErr(noDoc) === '文档不存在');

  // 9.4 空 query 检索
  const emptyQ = await reqJson('POST', '/api/kb/search', { query: '   ', top_k: 3 });
  edge.emptyQuery = { status: emptyQ.status, error: jsonErr(emptyQ) };
  check('检索空 query 返回 400', 400, emptyQ.status, emptyQ.status === 400);
  check('检索空 query 的错误信息为「查询内容为空」', '查询内容为空', jsonErr(emptyQ), jsonErr(emptyQ) === '查询内容为空');

  // 9.5 ask 的空 query 必须在写 SSE 响应头之前拦下 → 是 JSON 400，不是 200 + SSE error
  const askEmpty = await httpReq('POST', '/api/kb/ask', {
    body: Buffer.from(JSON.stringify({ query: '   ', model: askModel }), 'utf8'),
    headers: { 'Content-Type': 'application/json; charset=utf-8', ...(sessionCookie ? { Cookie: sessionCookie } : {}) },
  });
  steps.push({ method: 'POST', path: '/api/kb/ask', status: askEmpty.status, requestBody: '{"query":"   ","model":"..."}', responseBody: clip(askEmpty.text) });
  const askEmptyCt = String(askEmpty.headers['content-type'] || '');
  edge.askEmptyQuery = { status: askEmpty.status, error: jsonErr(askEmpty), contentType: askEmptyCt };
  check('ask 空 query 返回 400（而不是 200 + SSE error）', 400, askEmpty.status, askEmpty.status === 400);
  check('ask 空 query 的错误信息为「query 不能为空」', 'query 不能为空', jsonErr(askEmpty), jsonErr(askEmpty) === 'query 不能为空');
  check('ask 空 query 的响应是 JSON 而不是 SSE', 'application/json', askEmptyCt.split(';')[0] || '(空)', askEmptyCt.startsWith('application/json'));

  // 9.6 非法 JSON
  const badJson = await httpReq('POST', '/api/kb/text', {
    body: Buffer.from('{not json', 'utf8'),
    headers: { 'Content-Type': 'application/json; charset=utf-8', ...(sessionCookie ? { Cookie: sessionCookie } : {}) },
  });
  steps.push({ method: 'POST', path: '/api/kb/text', status: badJson.status, requestBody: '{not json', responseBody: clip(badJson.text) });
  edge.badJson = { status: badJson.status, error: jsonErr(badJson) };
  check('非法 JSON 返回 400', 400, badJson.status, badJson.status === 400);
  check('非法 JSON 的错误信息为「请求格式错误」', '请求格式错误', jsonErr(badJson), jsonErr(badJson) === '请求格式错误');

  // 9.7 不支持的扩展名（按扩展名判定，不看 Content-Type）
  const pdfBoundary = '----day23kbbad' + Date.now();
  const pdfBody = buildMultipart({ name: 'file', filename: 'verify-unsupported.pdf', contentType: 'application/pdf', bytes: Buffer.from('not a real pdf', 'utf8'), boundary: pdfBoundary });
  const pdfHeaders = { 'Content-Type': 'multipart/form-data; boundary=' + pdfBoundary, 'Content-Length': String(pdfBody.length) };
  if (sessionCookie) pdfHeaders.Cookie = sessionCookie;
  const pdfRes = await httpReq('POST', '/api/kb/documents', { body: pdfBody, headers: pdfHeaders });
  steps.push({ method: 'POST', path: '/api/kb/documents', status: pdfRes.status, requestBody: '[multipart filename=verify-unsupported.pdf]', responseBody: clip(pdfRes.text) });
  edge.unsupportedExt = { status: pdfRes.status, error: jsonErr(pdfRes) };
  check('.pdf 上传返回 400', 400, pdfRes.status, pdfRes.status === 400);
  check('.pdf 的错误信息含「不支持的文件类型」', '含 不支持的文件类型', jsonErr(pdfRes), jsonErr(pdfRes).includes('不支持的文件类型'));

  // 9.8 未知模型：SSE 仍然先给 sources，再用 error 事件收尾，且不会出现 done
  const unknownModel = await httpReq('POST', '/api/kb/ask', {
    body: Buffer.from(JSON.stringify({ query: '任意问题', model: '__no_such_model__' }), 'utf8'),
    headers: { 'Content-Type': 'application/json; charset=utf-8', ...(sessionCookie ? { Cookie: sessionCookie } : {}) },
    timeoutMs: 60000,
  });
  steps.push({ method: 'POST', path: '/api/kb/ask', status: unknownModel.status, requestBody: '{"model":"__no_such_model__"}', responseBody: clip(unknownModel.text) });
  const umEvents = parseSse(unknownModel.text);
  const umNames = umEvents.map((e) => e.event);
  const umErrEvent = umEvents.find((e) => e.event === 'error');
  const umErrMsg = umErrEvent ? ((parseJson(umErrEvent.data) || {}).error || '') : '';
  edge.unknownModel = { status: unknownModel.status, events: umNames, error: umErrMsg };
  check('ask 未知模型仍是 200 + SSE（头已发出，错误只能走事件）', 200, unknownModel.status, unknownModel.status === 200);
  check('ask 未知模型首个事件是 sources', 'sources', umNames[0] || '(无)', umNames[0] === 'sources');
  check('ask 未知模型以 error 事件收尾，信息为「llm: 端点不存在」', 'llm: 端点不存在', umErrMsg, umErrMsg === 'llm: 端点不存在');
  check('ask 未知模型不会收到 done 事件', '不含 done', umNames.includes('done') ? '含 done' : '不含 done', !umNames.includes('done'));
  out.contractEdges = edge;
}

// ---------- 执行 + 兜底 ----------
let fatal = '';
try {
  await runAll();
} catch (e) {
  fatal = e && (e.stack || e.message) ? String(e.stack || e.message) : String(e);
  fail('脚本执行中断', '无异常地跑完流程', oneLine(fatal, 200));
}

const nPass = checks.filter((c) => c.level === 'PASS').length;
const nWarn = checks.filter((c) => c.level === 'WARN').length;
const nFail = checks.filter((c) => c.level === 'FAIL').length;

say('');
if (nWarn) {
  say('WARN 明细（不影响退出码，仅供参考）：');
  for (const c of checks.filter((c) => c.level === 'WARN')) say(`  - ${c.name}（期望=${c.expect} 实际=${c.actual}）`);
}
if (nFail) {
  say('FAIL 明细：');
  for (const c of checks.filter((c) => c.level === 'FAIL')) say(`  - ${c.name}（期望=${c.expect} 实际=${c.actual}）`);
}
const exitCode = nFail > 0 ? 1 : 0;
say(`== 汇总：PASS ${nPass} / WARN ${nWarn} / FAIL ${nFail} ==`);
say(`退出码 = ${exitCode}（有 FAIL 即 1，否则 0）`);

// ---------- 落盘：日志 + 证据 JSON ----------
if (logPath) {
  fs.mkdirSync(path.dirname(path.resolve(logPath)), { recursive: true });
  fs.writeFileSync(logPath, lines.join('\n') + '\n', 'utf8');
  say(`log written: ${logPath}`);
}
if (transcriptPath) {
  const transcript = {
    base,
    stamp,
    runTag,
    secret,
    textTitle: title,
    image: { filename: pngName, pngBytes: PNG_BYTES.length },
    textId,
    imageId,
    fatal: fatal || undefined,
    evidence: out,
    steps,
    checks,
    summary: { pass: nPass, warn: nWarn, fail: nFail, exitCode },
  };
  fs.mkdirSync(path.dirname(path.resolve(transcriptPath)), { recursive: true });
  fs.writeFileSync(transcriptPath, JSON.stringify(transcript, null, 2), 'utf8');
  console.log(`transcript written: ${transcriptPath}`);
}

// 到此所有 http 请求都已 end 并收到 end 事件（agent:false 短连接），没有悬挂 socket，
// 可以放心用退出码结束进程。
process.exit(exitCode);
