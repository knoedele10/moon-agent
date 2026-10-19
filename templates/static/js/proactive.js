// 主动聊天（proactive）前端：轮询后端，把「助手主动说的一句话」用页内横幅提示出来。
//
// 设计要点：
//   - 不用系统通知：Electron 的通知权限链路复杂，页内横幅在网页端 / 桌面端表现一致；
//   - 样式动态注入（proactive- 前缀，复用页面已有的 CSS 变量），不改 style.css；
//   - init() 幂等：重复调用只会启动一次轮询。
//
// 容器与脚本标签由主 agent 在 templates/chat/index.html 里加：
//   <div id="proactive-banner" hidden></div>
//   <script src="/static/js/proactive.js"></script>
(function () {
  'use strict';

  var POLL_MS = 60000;              // 每 60 秒轮询一次
  var STORAGE_KEY = 'blogmy:proactive:lastId';
  // 与后端 web/proactive.go 的 proactiveConversationTitle 保持一致。
  var CONV_TITLE = '主动消息';
  var FIRST_RUN_FRESH_MS = 6 * 60 * 60 * 1000; // 首次运行只提示 6 小时内的消息

  var state = {
    inited: false,
    timer: null,
    lastId: 0
  };

  // ---- 本地「已提示到哪条」水位 ----

  function readLastId() {
    try {
      var v = parseInt(localStorage.getItem(STORAGE_KEY), 10);
      return isFinite(v) && v > 0 ? v : 0;
    } catch (e) {
      return 0;
    }
  }

  function writeLastId(id) {
    try {
      localStorage.setItem(STORAGE_KEY, String(id));
    } catch (e) { /* 隐私模式等场景，静默失败 */ }
  }

  // ---- 样式（动态注入，proactive- 前缀） ----

  function injectStyle() {
    if (document.getElementById('proactive-style')) return;
    var css = [
      '#proactive-banner{position:fixed;left:50%;bottom:104px;transform:translateX(-50%);z-index:60;',
      'display:flex;align-items:center;gap:10px;width:max-content;max-width:min(560px,calc(100vw - 24px));',
      'padding:10px 12px;box-sizing:border-box;',
      'background:var(--surface-raised,#1e231d);border:1px solid var(--border-strong,#3a4337);',
      'border-radius:var(--radius,12px);box-shadow:var(--shadow-md,0 18px 48px rgba(0,0,0,.24));',
      'color:var(--text,#eef0e9);font-family:var(--font-ui,sans-serif);font-size:14px;}',
      '#proactive-banner[hidden]{display:none !important;}',
      '.proactive-main{display:flex;flex-direction:column;gap:2px;align-items:flex-start;',
      'background:transparent;border:0;padding:0;margin:0;cursor:pointer;text-align:left;',
      'color:inherit;font:inherit;min-width:0;}',
      '.proactive-tag{color:var(--accent,#c7d997);font-size:12px;font-weight:600;letter-spacing:0;}',
      '.proactive-text{color:var(--text,#eef0e9);white-space:normal;word-break:break-word;line-height:1.45;}',
      '.proactive-close{flex:0 0 auto;background:transparent;border:1px solid var(--border-strong,#3a4337);',
      'border-radius:var(--radius-sm,8px);color:var(--text-muted,#a3aa9d);cursor:pointer;',
      'padding:5px 10px;font:inherit;font-size:12px;}',
      '.proactive-close:hover{background:var(--surface-hover,#252b23);color:var(--text,#eef0e9);}'
    ].join('');
    var style = document.createElement('style');
    style.id = 'proactive-style';
    style.textContent = css;
    document.head.appendChild(style);
  }

  function bannerEl() {
    return document.getElementById('proactive-banner');
  }

  function hideBanner() {
    var box = bannerEl();
    if (!box) return;
    box.hidden = true;
    box.textContent = '';
  }

  // ---- 横幅 ----

  function showBanner(msg) {
    var box = bannerEl();
    if (!box) return;
    box.textContent = '';
    box.hidden = false;

    var main = document.createElement('button');
    main.type = 'button';
    main.className = 'proactive-main';
    main.title = '查看助手的主动消息';
    main.setAttribute('aria-label', '查看助手的主动消息');

    var tag = document.createElement('span');
    tag.className = 'proactive-tag';
    tag.textContent = '助手有话想跟你说';
    var text = document.createElement('span');
    text.className = 'proactive-text';
    text.textContent = String(msg.content || '').trim();
    main.appendChild(tag);
    main.appendChild(text);
    main.addEventListener('click', function () {
      openConversation(msg.conversation_id);
    });

    var close = document.createElement('button');
    close.type = 'button';
    close.className = 'proactive-close';
    close.textContent = '知道了';
    close.title = '知道了，收起这条提示';
    close.setAttribute('aria-label', '知道了，收起这条提示');
    close.addEventListener('click', function () {
      hideBanner();
    });

    box.appendChild(main);
    box.appendChild(close);
  }

  // 跳到主动消息所在会话。
  // chat.js 没有把 openConversation 暴露到 window，所以退化为「在侧栏里点同名会话」；
  // 若主 agent 以后暴露了 window.CHAT.openConversation，则优先用它。
  function openConversation(conversationId) {
    hideBanner();
    if (location.hash !== '#/chat') location.hash = '#/chat';

    if (window.CHAT && typeof window.CHAT.openConversation === 'function') {
      try {
        window.CHAT.openConversation(conversationId);
        return;
      } catch (e) { /* 回落到侧栏点击 */ }
    }

    var tries = 0;
    (function attempt() {
      var list = document.getElementById('conv-list');
      if (list) {
        var items = list.querySelectorAll('.conv-item');
        for (var i = 0; i < items.length; i++) {
          if (items[i].getAttribute('aria-label') === CONV_TITLE) {
            items[i].click();
            return;
          }
        }
      }
      if (tries++ < 15) setTimeout(attempt, 200);
    })();
  }

  // ---- 轮询 ----

  async function poll() {
    if (document.hidden) return;
    var res;
    try {
      res = await fetch('/api/proactive/messages');
    } catch (e) {
      return; // 网络抖动静默跳过，下个周期再来
    }
    if (!res.ok) return;
    var body;
    try {
      body = await res.json();
    } catch (e) {
      return;
    }
    var list = body && Array.isArray(body.messages) ? body.messages : [];
    if (!list.length) return;

    // 容器还没进 DOM（脚本被放在前面 / 主 agent 还没加）时先不推进水位，
    // 否则这条消息会被当成“已提示”而永远不再显示。
    if (!bannerEl()) return;

    var newest = list[0]; // 后端按 id 倒序，最新在前
    var id = Number(newest.id) || 0;
    if (id <= state.lastId) return;

    var firstRun = state.lastId === 0;
    state.lastId = id;
    writeLastId(id);

    // 首次运行（本地没有水位）时，别把很久以前的历史主动消息当“新消息”弹出来。
    if (firstRun) {
      var created = Date.parse(newest.created_at || '');
      if (!isFinite(created) || Date.now() - created > FIRST_RUN_FRESH_MS) return;
    }
    showBanner(newest);
  }

  function init() {
    if (state.inited) return; // 幂等
    state.inited = true;
    state.lastId = readLastId();
    injectStyle();
    poll();
    if (window.APP_RUNTIME) state.timer = window.APP_RUNTIME.every(poll, POLL_MS);
    else {
      state.timer = setInterval(poll, POLL_MS);
      document.addEventListener('visibilitychange', function () { if (!document.hidden) poll(); });
    }
  }

  window.PROACTIVE = { init: init };

  // 自启：主 agent 只需要加 <script>，不必记得再调 init()。
  // 脚本若在 <head> 先于 #proactive-banner 加载，就等 DOMContentLoaded 再启动。
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
