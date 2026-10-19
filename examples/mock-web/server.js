'use strict';
// blog-my mock 版公网服务：一个端口同时提供 /api/health、mock 业务接口、前端静态页面。
// 部署：node server.js，监听 PORT 环境变量（发布平台注入），绑定 0.0.0.0。
const http = require('http');
const fs = require('fs');
const path = require('path');

const PUBLIC_DIR = path.join(__dirname, 'public');
const MIME = {
  '.html': 'text/html', '.css': 'text/css', '.js': 'text/javascript',
  '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png', '.ico': 'image/x-icon'
};

function readPage(name) {
  return fs.readFileSync(path.join(PUBLIC_DIR, name), 'utf8');
}
const PAGES = {
  index: readPage('index.html'),
  chat: readPage('chat.html'),
  login: readPage('login.html'),
  register: readPage('register.html'),
  hot: readPage('hot.html'),
  // Day 28：验收清单页（附录 J）——独立页面，不改动任何已验收的接口与页面
  accept: readPage('accept.html')
};

// ---- mock 数据（Day 11/12/13 用过的假数据，让公网页面能完整演示）----
const CONVERSATIONS = [
  { id: 1, title: '帮我写一段自我介绍' },
  { id: 2, title: '把这段话翻译成英文，语气正式一点' },
  { id: 3, title: '用生活例子解释递归' },
  { id: 4, title: '给朋友写一条生日祝福' },
  { id: 5, title: '周报的标题起个草稿' },
  { id: 6, title: '推荐三首适合练听力的英文歌' }
];
const MESSAGES = {
  1: [
    { role: 'user', content: '帮我写一段 30 秒的自我介绍，面试用。' },
    { role: 'assistant', content: '面试官您好，我叫王小明，擅长把复杂问题拆成小步骤逐个解决。\n\n过去一年我坚持每天记录学习笔记，完成了一个多模型对话站的小项目：从后端接口、数据库到前端页面都是自己搭的。\n\n我上手快、不怕问"为什么"，希望能加入贵团队继续成长。谢谢！' }
  ],
  2: [
    { role: 'user', content: '把这段话翻译成英文，语气正式一点：我们下周开会讨论方案。' },
    { role: 'assistant', content: 'Here is a formal version:\n\nWe would like to schedule a meeting next week to discuss the proposal. Please let us know your availability.' }
  ],
  3: [
    { role: 'user', content: '用生活例子解释一下什么是递归。' },
    { role: 'assistant', content: '想象你站在镜子前，镜子里还有一面镜子，一层套一层——这就是最直观的"自己调用自己"。\n\n程序里的递归：函数在解决问题时，把大问题拆成同样形状的小问题，直到碰到一个不用再拆的"镜子底"（终止条件）。' }
  ],
  4: [
    { role: 'user', content: '给好朋友写一条生日祝福，要真诚不油腻。' },
    { role: 'assistant', content: '生日快乐！认识你这么多年，最幸运的就是有事情总能第一个想到你。\n\n新的一岁，愿你想做的事都来得及，想见的人都在身边。' }
  ],
  5: [
    { role: 'user', content: '帮我的周报标题起个草稿，本周主要在修前端样式。' },
    { role: 'assistant', content: '给你三个方向：\n\n1. 稳一字当头：前端体验修复周记\n2. 数据向：本周完成 6 处界面问题修复\n3. 轻松向：给页面做了一次"体检"和"补牙"' }
  ],
  6: [
    { role: 'user', content: '推荐三首适合练英语听力的歌，慢一点的。' },
    { role: 'assistant', content: '1. Yesterday Once More – Carpenters（吐字极清晰）\n2. Photograph – Ed Sheeran（语速慢，词汇日常）\n3. City of Stars – La La Land（对唱形式，句子短）' }
  ]
};
const MODELS = [
  { id: 'deepseek-chat', label: 'DeepSeek', has_key: true },
  { id: 'gpt-4o-mini', label: 'OpenAI', has_key: true },
  { id: 'qwen-plus', label: '通义千问', has_key: false }
];

// Day 17：当日真实热搜快照 + 收藏（与 db/seed-day17.sql 同一批真实数据）
const HOT = JSON.parse(fs.readFileSync(path.join(__dirname, 'data', 'hot.json'), 'utf8'));
const FAVORITES_FILE = path.join(__dirname, 'data', 'favorites.json');
const FAVORITES = JSON.parse(fs.readFileSync(FAVORITES_FILE, 'utf8'));
const HOT_FETCHED_AT = '2026-10-03';

// Day 18：收藏写入持久化（把内存里的 FAVORITES 回写 favorites.json，进程重启不丢）
function persistFavorites() {
  fs.writeFileSync(FAVORITES_FILE, JSON.stringify(FAVORITES, null, 2), 'utf8');
}

// 服务端日志（余力加练）：统一带时间戳，方便排查
function log(level, msg) {
  console.log(new Date().toISOString() + ' [' + level + '] ' + msg);
}

function getMock(req) {
  var q = new URL('http://x' + (req.url || '/')).searchParams.get('mock');
  if (q) return q;
  var m = (req.headers.cookie || '').match(/mock=(\w+)/);
  return m ? m[1] : '';
}

// Day 20：跨域配置（CORS）。所有 /api/* 响应都带这三个头，
// 浏览器才允许"别的源"的页面（如 localhost 预览页）读取返回内容。
const CORS_HEADERS = {
  'Access-Control-Allow-Origin': '*',
  'Access-Control-Allow-Methods': 'GET,POST,PATCH,DELETE,OPTIONS',
  'Access-Control-Allow-Headers': 'Content-Type'
};
function withCORS(headers) {
  return Object.assign({}, CORS_HEADERS, headers);
}

const server = http.createServer((req, res) => {
  const parsed = new URL('http://x' + (req.url || '/'));
  const url = parsed.pathname;
  const method = (req.method || 'GET').toUpperCase();
  const queryMock = parsed.searchParams.get('mock');
  const mock = getMock(req);
  const mockDelay = mock === 'slow' ? 10000 : (mock === 'hang' ? 120000 : 0);
  const mockFail = mock === 'error';

  // Day 20：OPTIONS 预检请求直接 204 放行。
  // 浏览器跨域发 POST（带 JSON）前会先发一个 OPTIONS"探路"，
  // 服务端不带 CORS 头回应的话，真正的请求根本发不出去。
  if (method === 'OPTIONS' && url.startsWith('/api/')) {
    res.writeHead(204, withCORS({}));
    res.end();
    return;
  }

  function sendJSON(data, status) {
    setTimeout(function () {
      res.writeHead(status || 200, withCORS({
        'Content-Type': 'application/json; charset=utf-8',
        'Cache-Control': 'no-store'
      }));
      res.end(JSON.stringify(data));
    }, mockDelay);
  }

  function sendStatic(filePath) {
    fs.readFile(filePath, function (err, data) {
      if (err) { res.writeHead(404); res.end('not found'); return; }
      var ext = path.extname(filePath);
      res.writeHead(200, {
        'Content-Type': (MIME[ext] || 'application/octet-stream') + '; charset=utf-8',
        'Cache-Control': 'public, max-age=3600'
      });
      res.end(data);
    });
  }

  // /api/health —— 健康检查，永远立即返回，不受 mock 影响
  if (url === '/api/health') {
    res.writeHead(200, withCORS({
      'Content-Type': 'application/json; charset=utf-8',
      'Cache-Control': 'no-store'
    }));
    res.end(JSON.stringify({
      status: 'ok',
      service: 'blog-my',
      version: 'mock-1',
      time: new Date().toISOString(),
      uptime: Math.round(process.uptime())
    }));
    return;
  }

  // GET /api/hot?limit=N —— 当日真实热搜（读取数据快照，limit 为条数限制）
  if (url === '/api/hot') {
    var limitParam = parseInt(parsed.searchParams.get('limit'), 10);
    var limit = Number.isFinite(limitParam) && limitParam > 0 ? limitParam : HOT.length;
    res.writeHead(200, withCORS({
      'Content-Type': 'application/json; charset=utf-8',
      'Cache-Control': 'no-store'
    }));
    res.end(JSON.stringify({ ok: true, data: HOT.slice(0, limit), fetched_at: HOT_FETCHED_AT }));
    return;
  }

  // GET /api/favorites —— 收藏列表（Day 22：软删除的记录不进列表）
  if (url === '/api/favorites' && method === 'GET') {
    var alive = FAVORITES.filter(function (f) { return !f.is_deleted; });
    log('info', 'GET /api/favorites -> ' + alive.length + ' 条（另有 ' + (FAVORITES.length - alive.length) + ' 条已软删除，不返回）');
    res.writeHead(200, withCORS({
      'Content-Type': 'application/json; charset=utf-8',
      'Cache-Control': 'no-store'
    }));
    res.end(JSON.stringify({ ok: true, data: alive }));
    return;
  }

  // POST /api/favorites —— 收藏写入（Day 18）
  if (url === '/api/favorites' && method === 'POST') {
    var chunks = [];
    req.on('data', function (c) { chunks.push(c); });
    req.on('end', function () {
      var body = {};
      try {
        body = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
      } catch (e) {
        res.writeHead(400, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
        res.end(JSON.stringify({ ok: false, error: '请求体不是合法的 JSON' }));
        return;
      }
      var title = (body.title == null ? '' : String(body.title)).trim();
      // 缺必填字段：中文提示
      if (!title) {
        res.writeHead(400, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
        res.end(JSON.stringify({ ok: false, error: '缺少必填字段 title（收藏内容标题不能为空）' }));
        return;
      }
      // 重复提交被拒
      var dup = FAVORITES.some(function (f) { return !f.is_deleted && f.title === title; });
      if (dup) {
        res.writeHead(409, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
        res.end(JSON.stringify({ ok: false, error: '该内容已在收藏中，请勿重复收藏' }));
        return;
      }
      var item = {
        id: FAVORITES.reduce(function (m, f) { return Math.max(m, f.id); }, 0) + 1,
        title: title,
        url: body.url ? String(body.url) : null,
        user_id: body.user_id ? String(body.user_id) : null,
        created_at: new Date().toISOString()
      };
      FAVORITES.push(item);
      persistFavorites();
      log('info', 'POST /api/favorites 新增 id=' + item.id + ' title=' + title);
      res.writeHead(201, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
      res.end(JSON.stringify({ ok: true, id: item.id, title: item.title, url: item.url, created_at: item.created_at }));
    });
    return;
  }

  // Day 22：单条收藏的 PATCH（改）与 DELETE（删），路径形如 /api/favorites/3。
  // 注意：路由必须排在精确匹配 '/api/favorites' 之后，否则会抢走列表请求。
  var favMatch = url.match(/^\/api\/favorites\/(\d+)$/);
  if (favMatch) {
    var favId = parseInt(favMatch[1], 10);
    // 已软删除的记录等同于不存在：查不到就 404。
    var target = null;
    for (var fi = 0; fi < FAVORITES.length; fi++) {
      if (FAVORITES[fi].id === favId && !FAVORITES[fi].is_deleted) { target = FAVORITES[fi]; break; }
    }
    if (!target) {
      res.writeHead(404, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
      res.end(JSON.stringify({ ok: false, error: '收藏不存在或已删除（id=' + favId + '）' }));
      return;
    }

    // PATCH：只改请求体里出现的字段，没传的保持原值。
    if (method === 'PATCH') {
      var pchunks = [];
      req.on('data', function (c) { pchunks.push(c); });
      req.on('end', function () {
        var pbody = {};
        try {
          pbody = JSON.parse(Buffer.concat(pchunks).toString('utf8') || '{}');
        } catch (e) {
          res.writeHead(400, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
          res.end(JSON.stringify({ ok: false, error: '请求体不是合法的 JSON' }));
          return;
        }
        var hasTitle = Object.prototype.hasOwnProperty.call(pbody, 'title');
        var hasUrl = Object.prototype.hasOwnProperty.call(pbody, 'url');
        if (!hasTitle && !hasUrl) {
          res.writeHead(400, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
          res.end(JSON.stringify({ ok: false, error: '没有可更新的字段（title / url 至少给一个）' }));
          return;
        }
        var newTitle = target.title;
        if (hasTitle) {
          newTitle = (pbody.title == null ? '' : String(pbody.title)).trim();
          if (!newTitle) {
            res.writeHead(400, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
            res.end(JSON.stringify({ ok: false, error: 'title 不能改成空' }));
            return;
          }
          var clash = FAVORITES.some(function (f) { return f.id !== favId && !f.is_deleted && f.title === newTitle; });
          if (clash) {
            res.writeHead(409, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
            res.end(JSON.stringify({ ok: false, error: '该内容已在收藏中，请勿重复收藏' }));
            return;
          }
        }
        // before 随响应一起返回：截图与验收需要「改之前 vs 改之后」的原始值，不靠事后回忆。
        var before = { title: target.title, url: target.url };
        if (hasTitle) target.title = newTitle;
        if (hasUrl) target.url = pbody.url ? String(pbody.url) : null;
        target.updated_at = new Date().toISOString();
        persistFavorites();
        log('info', 'PATCH /api/favorites/' + favId + ' title: ' + before.title + ' -> ' + target.title);
        res.writeHead(200, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
        res.end(JSON.stringify({ ok: true, before: before, data: target }));
      });
      return;
    }

    // DELETE：软删除（余力加练）—— 只打 is_deleted 标记，不真删数据，删错还能从文件里找回。
    if (method === 'DELETE') {
      target.is_deleted = true;
      target.deleted_at = new Date().toISOString();
      persistFavorites();
      log('info', 'DELETE /api/favorites/' + favId + ' 软删除 title=' + target.title);
      res.writeHead(200, withCORS({ 'Content-Type': 'application/json; charset=utf-8' }));
      res.end(JSON.stringify({ ok: true, id: favId, deleted: true, soft: true, deleted_at: target.deleted_at }));
      return;
    }
  }

  if (url === '/api/conversations') {
    if (mockFail) return sendJSON({ error: '服务暂时不可用（模拟错误）' }, 500);
    return sendJSON(CONVERSATIONS);
  }
  var msgMatch = url.match(/^\/api\/conversations\/(\d+)\/messages$/);
  if (msgMatch) {
    if (mockFail) return sendJSON({ error: '服务暂时不可用（模拟错误）' }, 500);
    return sendJSON(MESSAGES[msgMatch[1]] || []);
  }
  if (url === '/api/models') {
    if (mockFail) return sendJSON({ error: '服务暂时不可用（模拟错误）' }, 500);
    return sendJSON(MODELS);
  }
  if (url === '/api/keys') {
    if (mockFail) return sendJSON({ error: '服务暂时不可用（模拟错误）' }, 500);
    var keysParam = parsed.searchParams.get('keys');
    return sendJSON(keysParam === '1'
      ? [{ id: 1, endpoint: 'https://api.deepseek.com', masked: 'sk-****1234' }]
      : []);
  }

  // Day 27：未实现的 /api/* 一律返回 JSON 404，不再掉到下面的页面兜底。
  // 修复前：POST /api/chat 没有匹配到路由，会走到"页面"分支，把登录页 HTML
  // 连同 HTTP 200 一起返回；前端拿到 200 就当成功，把 HTML 当 SSE 流解析，
  // 一个 data 字段都读不到，最终只显示"模型未返回内容"——用户完全不知道发生了什么。
  if (url.startsWith('/api/')) {
    log('warn', '未实现的接口 ' + method + ' ' + url + ' -> 404（JSON）');
    return sendJSON({
      ok: false,
      error: '接口不存在或未启用：' + method + ' ' + url + '（当前部署版是 mock 服务，对话接口未接入真实模型）'
    }, 404);
  }

  // 静态资源
  if (url.startsWith('/static/')) {
    return sendStatic(path.join(PUBLIC_DIR, url));
  }

  // 页面（Day 20：首页改为检查台，展示健康状态 + 真实数据；对话页仍在 /chat）
  var page = PAGES.login;
  if (url === '/register') page = PAGES.register;
  if (url === '/' || url === '/index') page = PAGES.index;
  if (url === '/chat') page = PAGES.chat;
  if (url === '/hot') page = PAGES.hot;
  if (url === '/accept') page = PAGES.accept;
  var headers = { 'Content-Type': 'text/html; charset=utf-8' };
  if (queryMock) headers['Set-Cookie'] = 'mock=' + queryMock + '; Path=/; SameSite=Lax';
  res.writeHead(200, headers);
  res.end(page);
});

var port = process.env.PORT || 3000;
server.listen(port, '0.0.0.0', function () {
  console.log('blog-my mock running on port ' + port);
});
