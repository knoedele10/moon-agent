// Day 26 只读探针：确认线上站点当前跑的是什么（页面 / 静态资源 / 接口），不写入任何数据
const BASE = process.argv[2] || 'https://blog-my-check-10974.app.workbuddy.host';
const targets = [
  ['/', '首页'],
  ['/chat', '对话页'],
  ['/hot', '热搜页'],
  ['/login', '登录页'],
  ['/register', '注册页'],
  ['/static/css/style.css', '样式表'],
  ['/static/js/chat.js', '前端脚本'],
  ['/api/health', '健康接口'],
  ['/api/hot?limit=3', '热搜接口'],
  ['/api/favorites', '收藏接口'],
  ['/api/models', '模型列表接口']
];
(async () => {
  console.log('目标站点: ' + BASE);
  console.log('探测时间: ' + new Date().toLocaleString('zh-CN') + ' (' + new Date().toISOString() + ')\n');
  for (const [path, name] of targets) {
    try {
      const res = await fetch(BASE + path, { headers: { Origin: 'http://localhost:8091' } });
      const text = await res.text();
      let note = '';
      const ct = res.headers.get('content-type') || '';
      if (ct.includes('json')) {
        try {
          const j = JSON.parse(text);
          if (j.version) note = 'version=' + j.version + ' status=' + j.status + ' service=' + j.service;
          else if (j.data) note = 'data ' + j.data.length + ' 条' + (j.fetched_at ? ' fetched_at=' + j.fetched_at : '');
          else note = JSON.stringify(j).slice(0, 80);
        } catch (e) { note = '(JSON 解析失败)'; }
      } else {
        const t = text.match(/<title>(.*?)<\/title>/);
        const mark = text.includes('blog-my 检查台') ? '[检查台首页]' : (text.includes('blog-my-mock') ? '[mock 标记]' : '');
        note = 'title=' + (t ? t[1] : '(无)') + ' 字节=' + text.length + ' ' + mark;
      }
      console.log(String(res.status).padEnd(4) + path.padEnd(24) + name.padEnd(10) + note +
        '  ACAO=' + (res.headers.get('access-control-allow-origin') || '-'));
    } catch (e) {
      console.log('ERR ' + path.padEnd(24) + name.padEnd(10) + e.message);
    }
  }
})();
