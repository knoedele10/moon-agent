(function () {
  'use strict';

  var state = {
    conversations: [],
    models: [],
    currentId: null,
    model: '',
    streaming: false,
    abortController: null,
    view: 'chat',
    convError: ''
  };

  var els = {
    app: document.querySelector('.app'),
    convList: document.getElementById('conv-list'),
    convFilter: document.getElementById('conv-filter'),
    modelsList: document.getElementById('models-list'),
    keysList: document.getElementById('keys-list'),
    skillsList: document.getElementById('skill-list'),
    skillSearch: document.getElementById('skill-search'),
    skillAuto: document.getElementById('skill-auto'),
    thread: document.getElementById('thread'),
    emptyState: document.getElementById('empty-state'),
    messages: document.getElementById('messages'),
    modelSelect: document.getElementById('model-select'),
    input: document.getElementById('input'),
    composer: document.getElementById('composer'),
    composerBox: document.querySelector('.composer'),
    sendBtn: document.getElementById('send-btn'),
    newChat: document.getElementById('new-chat'),
    topbarTitle: document.getElementById('topbar-title'),
    sidebar: document.getElementById('sidebar'),
    sidebarToggle: document.getElementById('sidebar-toggle'),
    sidebarOverlay: document.getElementById('sidebar-overlay'),
    toast: document.getElementById('toast'),
    keysOpen: document.getElementById('keys-open'),
    keysDialog: document.getElementById('keys-dialog'),
    keysClose: document.getElementById('keys-close'),
    memoryPanel: document.getElementById('memory-panel'),
    memoryCount: document.getElementById('memory-count'),
    wsPick: document.getElementById('ws-pick'),
    assistantSelect: document.getElementById('assistant-select'),
    kindSelect: document.getElementById('kind-select'),
    modeSelect: document.getElementById('mode-select'),
    wsCurrent: document.getElementById('ws-current'),
    runSettingsToggle: document.getElementById('run-settings-toggle'),
    composerSettings: document.getElementById('composer-settings'),
    emptyExamples: document.getElementById('empty-examples'),
    planPanel: document.getElementById('plan-panel'),
    planSteps: document.getElementById('plan-steps'),
    planProgress: document.getElementById('plan-progress'),
    planToggle: document.getElementById('plan-toggle'),
    usageChip: document.getElementById('usage-chip'),
    reviewToggle: document.getElementById('review-toggle'),
    reviewPanel: document.getElementById('review-panel'),
    reviewFiles: document.getElementById('review-files'),
    reviewSummary: document.getElementById('review-summary'),
    reviewDiff: document.getElementById('review-diff'),
    reviewRefresh: document.getElementById('review-refresh'),
    reviewClose: document.getElementById('review-close')
  };

  var stickBottom = true;
  var toastTimer = null;
  var scrollFrame = 0;
  var scrollForced = false;
  // 最近打开的会话先从内存显示，再向服务端校对；只保留少量会话，避免长对话占满内存。
  var historyCache = new Map();
  var HISTORY_CACHE_LIMIT = 3;
  var historyDOMCache = new Map();
  var HISTORY_DOM_CACHE_LIMIT = 2;
  var conversationButtons = new Map();
  var activeConversationButton = null;
  var conversationMeta = {};
  try { conversationMeta = JSON.parse(localStorage.getItem('blogmy:conversation-meta') || '{}') || {}; } catch (e) { conversationMeta = {}; }
  function saveConversationMeta() {
    try { localStorage.setItem('blogmy:conversation-meta', JSON.stringify(conversationMeta)); } catch (e) {}
  }
  function conversationMetaFor(id) {
    var key = String(id);
    if (!conversationMeta[key]) conversationMeta[key] = { pinned: false, unread: false, openedAt: 0 };
    return conversationMeta[key];
  }
  var historyPage = { id: null, before: 0, hasMore: false, busy: false };
  var historyLoadSequence = 0;
  var jsonRequests = new Map();
  var conversationPoll = null;
  function pollVisible() { return state.view === 'chat'; }
  function startPoll(fn, interval) {
    return window.APP_RUNTIME ? window.APP_RUNTIME.every(fn, interval, pollVisible) : setInterval(fn, interval);
  }
  function stopPoll(id) {
    if (window.APP_RUNTIME) window.APP_RUNTIME.stop(id);
    else clearInterval(id);
  }

  function markActiveConversation(id) {
    if (activeConversationButton) {
      activeConversationButton.classList.remove('active');
      activeConversationButton.removeAttribute('aria-current');
    }
    activeConversationButton = conversationButtons.get(id) || null;
    if (activeConversationButton) {
      activeConversationButton.classList.add('active');
      activeConversationButton.setAttribute('aria-current', 'true');
    }
  }

  function stashCurrentThread() {
    var id = state.currentId;
    if (id == null || state.streaming || !historyCache.has(id) || !els.thread.childNodes.length) return;
    if (els.thread.querySelector('.child-run-live') || hasRunningSubagents()) return;
    var fragment = document.createDocumentFragment();
    while (els.thread.firstChild) fragment.appendChild(els.thread.firstChild);
    historyDOMCache.delete(id);
    historyDOMCache.set(id, fragment);
    if (historyDOMCache.size > HISTORY_DOM_CACHE_LIMIT) historyDOMCache.delete(historyDOMCache.keys().next().value);
  }
  // 子任务轮询：statuses 只记当前会话的子任务状态，避免切走后旧轮询继续跑。
  var subagentPoll = {
    timer: null,
    convId: null,
    statuses: {},
    // 宽限窗口：子任务刚结束时父 agent 会被唤醒跑一轮汇总，那一轮的结果要过一会儿才落库。
    // 计数器 >0 时即使已经没有 running 的子任务，也继续拉几拍消息，别在汇总出来前就把轮询停掉。
    grace: 0
  };
  var childRunPoll = { timer: null, convId: null, parentId: null, panel: null };
	var teamPoll = { timer: null, convId: null };
  var teamInFlight = {};

  function toast(message) {
    els.toast.textContent = message;
    els.toast.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      els.toast.classList.remove('show');
    }, 2600);
  }

  // ---------- 右上角「当前任务」卡片 ----------
  // 卡片只是把已有状态换个地方显示：会话标题、本轮工具调用进度、工作目录。
  // 状态源仍然在 chat.js，渲染交给 static/js/taskcard.js（window.TASKCARD）。
  var taskState = {
    workspace: '',
    toolTotal: 0,
    toolDone: 0
  };

  function resetTaskCounters() {
    taskState.toolTotal = 0;
    taskState.toolDone = 0;
  }

  function notifyTaskCard() {
    if (!window.TASKCARD) return;
    var conv = state.conversations.filter(function (c) {
      return c.id === state.currentId;
    })[0];
    var title = (conv && conv.title) || '';
    window.TASKCARD.update({
      kind: (els.kindSelect && els.kindSelect.value) || 'work',
      title: title,
      current: state.streaming ? '正在处理这条消息…' : title,
      done: taskState.toolDone,
      total: taskState.toolTotal,
      busy: state.streaming,
      workspace: taskState.workspace
    });
    if (window.TASKBOARD && typeof window.TASKBOARD.refresh === 'function') {
      window.TASKBOARD.refresh();
    }
  }

  // 空状态与「招呼 + 输入框居中」是同一件事的两面：.app 上挂 is-empty，
  // 样式里靠它把 messages 与 composer 当成一组垂直居中（见 style.css 的 .app.is-empty）。
  // 所以凡是切空状态的地方，都必须同时改这两处，不能只改 hidden。
  function setEmptyState(visible) {
    if (els.emptyState) els.emptyState.hidden = !visible;
    if (els.app) els.app.classList.toggle('is-empty', !!visible);
  }

  function safeParse(text) {
    try {
      return JSON.parse(text);
    } catch (e) {
      return null;
    }
  }

  async function api(path, options) {
    var started = performance.now();
    var res;
    try { res = await fetch(path, options); }
    finally {
      if (window.APP_PERF) window.APP_PERF.record('request', path.split('?')[0], performance.now() - started);
    }
    if (res.status === 401) {
      window.location.href = '/login';
      throw new Error('unauthorized');
    }
    return res;
  }

  function apiJSON(path, options) {
    var shared = !options || !options.method || options.method === 'GET';
    if (shared && jsonRequests.has(path)) return jsonRequests.get(path);
    var pending = requestJSON(path, options);
    if (shared) {
      jsonRequests.set(path, pending);
      pending.then(function () { jsonRequests.delete(path); }, function () { jsonRequests.delete(path); });
    }
    return pending;
  }

  async function requestJSON(path, options) {
    var res = await api(path, options);
    if (!res.ok) {
      var message = 'HTTP ' + res.status;
      try {
        var body = await res.json();
        if (body && body.error) message = body.error;
      } catch (e) {}
      throw new Error(message);
    }
    if (res.status === 204) return null;
    return res.json();
  }

  function scrollToBottom(force) {
    if (!force && !stickBottom) return;
    scrollForced = scrollForced || !!force;
    if (scrollFrame) return;
    scrollFrame = requestAnimationFrame(function () {
      scrollFrame = 0;
      if (scrollForced || stickBottom) els.messages.scrollTop = els.messages.scrollHeight;
      scrollForced = false;
    });
  }

  function autoGrow() {
    var el = els.input;
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, 180) + 'px';
  }

  function setStreaming(active) {
    var wasStreaming = state.streaming;
    state.streaming = active;
    updateOlderButton();
    updateStopButton(active);
    if (els.messages) els.messages.setAttribute('aria-busy', active ? 'true' : 'false');
    if (els.thread) els.thread.setAttribute('aria-busy', active ? 'true' : 'false');
    var activityStatus = document.getElementById('activity-status');
    if (activityStatus && (active || wasStreaming)) {
      activityStatus.textContent = active ? '正在生成回复，请稍候' : '回复生成已结束';
    }
    if (els.thread) {
      els.thread.classList.toggle('is-streaming', active);
      els.messages.classList.toggle('is-streaming', active);
      var buttons = els.thread.querySelectorAll('.msg-actions button');
      for (var i = 0; i < buttons.length; i++) buttons[i].disabled = active;
      var convButtons = els.convList.querySelectorAll('.conv-action');
      for (var j = 0; j < convButtons.length; j++) convButtons[j].disabled = active;
    }
  }

  // Day 26：发送键在生成中变成停止键（同一个按钮换图标/语义）。
  // 后端用 request context 取消上游，所以前端 abort() 就是正确的中断方式。
  var SEND_ICON = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="22" y1="2" x2="11" y2="13"></line><polygon points="22 2 15 22 11 13 2 9 22 2"></polygon></svg>';
  var STOP_ICON = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="6" y="6" width="12" height="12" rx="2"></rect></svg>';

  function updateStopButton(active) {
    if (!els.sendBtn) return;
    // 生成中不能真的禁用，否则点不动就没法停止。
    els.sendBtn.disabled = false;
    els.sendBtn.classList.toggle('is-stop', active);
    els.sendBtn.setAttribute('aria-label', active ? '停止生成' : '发送');
    els.sendBtn.title = active ? '停止生成' : '发送';
    els.sendBtn.innerHTML = active ? STOP_ICON : SEND_ICON;
  }

  function stopGeneration() {
    if (!state.streaming) return;
    var ctrl = state.abortController;
    if (ctrl) {
      try { ctrl.abort(); } catch (e) { /* 已结束也没关系 */ }
    } else {
      toast('当前浏览器不支持停止生成');
    }
  }

  function isAbortError(e) {
    return !!e && (e.name === 'AbortError' || e.code === 20);
  }

  function roleLabel(role) {
    return role === 'user' ? '我' : 'AI';
  }

  function appendMessage(role, content, options) {
    options = options || {};
    var msg = document.createElement('div');
    msg.className = 'msg ' + (role === 'user' ? 'user' : 'assistant') + (options.history ? ' is-history' : '');
    msg.setAttribute('data-role', role);
    var raw = String(content == null ? '' : content);
    msg.setAttribute('data-raw-content', raw);
    if (options.id != null) msg.setAttribute('data-message-id', String(options.id));

    var avatar = document.createElement('div');
    avatar.className = 'avatar';
    avatar.textContent = roleLabel(role);
    msg.appendChild(avatar);

    var bubble = document.createElement('div');
    bubble.className = 'bubble';
    if (role === 'assistant' && raw && window.MD && window.MD.render) {
      var mdBox = document.createElement('div');
      mdBox.className = 'md';
      mdBox.innerHTML = window.MD.render(raw);
      bubble.appendChild(mdBox);
    } else {
      bubble.textContent = raw;
    }
    msg.appendChild(bubble);
    msg.appendChild(buildMessageActions(role, msg, bubble));

    if (options.beforeNode && options.beforeNode.parentNode === els.thread) {
      els.thread.insertBefore(msg, options.beforeNode);
    } else if (options.afterNode && options.afterNode.parentNode === els.thread) {
      els.thread.insertBefore(msg, options.afterNode.nextSibling);
    } else {
      (options.target || els.thread).appendChild(msg);
    }
    return bubble;
  }

  function buildMessageActions(role, msg, bubble) {
    var actions = document.createElement('div');
    actions.className = 'msg-actions';

    if (role === 'user') {
      addMessageAction(actions, '编辑', '编辑这条消息', function () {
        editMessage(msg, bubble);
      });
      addMessageAction(actions, '删除', '删除这条消息', function () {
        deleteMessage(msg);
      });
    } else {
      actions.appendChild(buildCopyButton(bubble));
      addMessageAction(actions, '重新生成', '重新生成这条回复', function () {
        regenerateMessage(msg);
      });
      addMessageAction(actions, '删除', '删除这条消息', function () {
        deleteMessage(msg);
      });
    }
    return actions;
  }

  function addMessageAction(actions, label, title, onClick) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn btn-ghost msg-action-btn';
    btn.textContent = label;
    btn.title = title;
    btn.setAttribute('aria-label', title);
    btn.disabled = state.streaming;
    btn.addEventListener('click', onClick);
    actions.appendChild(btn);
    return btn;
  }

  // Day 11：复制按钮——点击复制原文，按钮短暂变 ✓ 并弹出 toast，
  // 让用户在"动作发生的原位"得到生效确认；连续点击只重置计时，不会出错。
  function buildCopyButton(bubble) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy-btn msg-action-btn';
    btn.title = '复制';
    btn.setAttribute('aria-label', '复制这条消息');
    btn.textContent = '复制';
    btn.disabled = state.streaming;
    var okTimer = null;
    btn.addEventListener('click', function () {
      copyText(bubble.textContent).then(function () {
        btn.textContent = '✓ 已复制';
        btn.classList.add('ok');
        toast('已复制到剪贴板');
        if (okTimer) clearTimeout(okTimer);
        okTimer = setTimeout(function () {
          btn.textContent = '复制';
          btn.classList.remove('ok');
        }, 1200);
      }).catch(function () {
        toast('复制失败，请手动选择文字复制');
      });
    });
    return btn;
  }

  // 写剪贴板：优先用异步 API，老浏览器退回 execCommand 兜底
  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      try {
        if (document.execCommand('copy')) {
          resolve();
        } else {
          reject(new Error('copy failed'));
        }
      } catch (e) {
        reject(e);
      } finally {
        document.body.removeChild(ta);
      }
    });
  }

  // ---- Day 26：代码块复制按钮（md.js 只产出占位按钮，点击靠事件委托） ----
  function bindCodeCopy() {
    if (!els.thread) return;
    els.thread.addEventListener('click', function (ev) {
      var btn = ev.target && ev.target.closest ? ev.target.closest('.md-code-copy') : null;
      if (!btn || !els.thread.contains(btn)) return;
      var pre = btn.closest('.md-code');
      var code = pre ? pre.querySelector('code') : null;
      if (!code) return;
      ev.preventDefault();
      copyText(code.textContent == null ? '' : code.textContent).then(function () {
        btn.textContent = '✓';
        btn.classList.add('ok');
        if (btn._copyTimer) clearTimeout(btn._copyTimer);
        btn._copyTimer = setTimeout(function () {
          btn.textContent = '复制';
          btn.classList.remove('ok');
        }, 1200);
      }).catch(function () {
        toast('复制失败，请手动选择文字复制');
      });
    });
  }

  // ---- Day 26：输入框「/」触发提示词快选 ----
  var promptsCache = null;
  var promptsLoaded = false;
  var pickerState = { el: null, items: [], active: -1, open: false };

  async function loadPromptLibrary() {
    if (promptsLoaded) return;
    promptsLoaded = true;
    try {
      var body = await apiJSON('/api/prompts');
      var list = body && Array.isArray(body.prompts) ? body.prompts : [];
      promptsCache = list.filter(function (p) { return p && p.title; });
    } catch (e) {
      promptsCache = []; // 拉取失败静默：这只是个便利功能，不能干扰正常输入
    }
  }

  function fuzzyMatch(text, keyword) {
    if (!keyword) return true;
    var hay = String(text || '').toLowerCase();
    var needle = String(keyword).toLowerCase();
    if (hay.indexOf(needle) !== -1) return true;
    var i = 0;
    for (var j = 0; j < hay.length && i < needle.length; j++) {
      if (hay.charAt(j) === needle.charAt(i)) i++;
    }
    return i === needle.length;
  }

  // 光标所在行是否以「/」开头；返回关键词与替换范围。
  function slashQuery() {
    var el = els.input;
    if (!el) return null;
    var pos = el.selectionStart;
    if (pos == null) pos = el.value.length;
    var before = el.value.slice(0, pos);
    var lineStart = before.lastIndexOf('\n') + 1;
    var line = before.slice(lineStart);
    var m = /^\/([^\s/]*)$/.exec(line);
    if (!m) return null;
    return { keyword: m[1], start: lineStart, end: pos };
  }

  function buildPromptPicker() {
    if (!els.composerBox || pickerState.el) return;
    var box = document.createElement('div');
    box.className = 'prompt-picker';
    box.hidden = true;
    box.setAttribute('role', 'listbox');
    box.setAttribute('aria-label', '提示词候选');
    els.composerBox.appendChild(box);
    pickerState.el = box;
  }

  function closePromptPicker() {
    if (!pickerState.el) return;
    pickerState.open = false;
    pickerState.active = -1;
    pickerState.items = [];
    pickerState.el.hidden = true;
    pickerState.el.textContent = '';
  }

  function renderPromptPicker() {
    var box = pickerState.el;
    if (!box) return;
    box.textContent = '';
    var items = pickerState.items;
    if (!items.length) { closePromptPicker(); return; }
    if (pickerState.active < 0 || pickerState.active >= items.length) pickerState.active = 0;
    items.forEach(function (item, i) {
      var row = document.createElement('button');
      row.type = 'button';
      row.className = 'prompt-picker-item' + (i === pickerState.active ? ' active' : '');
      row.setAttribute('role', 'option');
      row.setAttribute('aria-selected', i === pickerState.active ? 'true' : 'false');
      row.title = item.title || '';
      row.setAttribute('aria-label', item.title || '提示词');
      var title = document.createElement('span');
      title.className = 'prompt-picker-title';
      title.textContent = item.title || ('#' + item.id);
      var preview = document.createElement('span');
      preview.className = 'prompt-picker-preview';
      preview.textContent = String(item.content || '').replace(/\s+/g, ' ').trim().slice(0, 80);
      row.appendChild(title);
      row.appendChild(preview);
      // mousedown + preventDefault：点选时输入框不丢焦点，避免 blur 先关掉列表。
      row.addEventListener('mousedown', function (ev) {
        ev.preventDefault();
        applyPrompt(item);
      });
      box.appendChild(row);
    });
    box.hidden = false;
    pickerState.open = true;
  }

  function updatePromptPicker() {
    if (!promptsCache || !promptsCache.length) { closePromptPicker(); return; }
    var q = slashQuery();
    if (!q) { closePromptPicker(); return; }
    var matched = promptsCache.filter(function (p) {
      return fuzzyMatch(p.title, q.keyword);
    }).slice(0, 8);
    if (!matched.length) { closePromptPicker(); return; }
    pickerState.items = matched;
    pickerState.active = 0;
    closeParamsPanel(); // 两个浮层不同时出现
    renderPromptPicker();
  }

  function movePicker(delta) {
    var n = pickerState.items.length;
    if (!n) return;
    pickerState.active = (pickerState.active + delta + n) % n;
    renderPromptPicker();
  }

  function applyPrompt(item) {
    if (!item || !els.input) return;
    var el = els.input;
    var q = slashQuery();
    var value = el.value;
    var caret;
    if (q) {
      var head = value.slice(0, q.start);
      var tail = value.slice(q.end);
      el.value = head + item.content + tail;
      caret = (head + item.content).length;
    } else {
      var sep = value && !/\s$/.test(value) ? '\n' : '';
      el.value = value + sep + item.content;
      caret = el.value.length;
    }
    closePromptPicker();
    el.focus();
    if (typeof el.setSelectionRange === 'function') el.setSelectionRange(caret, caret);
    autoGrow();
  }

  function applyActivePrompt() {
    var item = pickerState.items[pickerState.active];
    if (item) applyPrompt(item);
  }

  // ---- Day 26：对话参数（额外系统提示词 / 温度 / 最大 token），按会话存 localStorage ----
  var paramsUi = { el: null, btn: null, textarea: null, range: null, rangeValue: null, useDefault: null, maxTokens: null };

  function paramsKey(conversationId) {
    var id = (conversationId === null || conversationId === undefined || conversationId === '') ? '__draft' : String(conversationId);
    return 'blogmy:chatparams:' + id;
  }

  function normalizeParams(obj) {
    var out = { system_prompt: '', temperature: null, max_tokens: null };
    if (!obj || typeof obj !== 'object') return out;
    if (typeof obj.system_prompt === 'string') out.system_prompt = obj.system_prompt;
    if (typeof obj.temperature === 'number' && isFinite(obj.temperature)) out.temperature = obj.temperature;
    if (typeof obj.max_tokens === 'number' && isFinite(obj.max_tokens) && obj.max_tokens > 0) out.max_tokens = Math.round(obj.max_tokens);
    return out;
  }

  function emptyParams() { return { system_prompt: '', temperature: null, max_tokens: null }; }

  function readChatParams(conversationId) {
    try {
      var raw = localStorage.getItem(paramsKey(conversationId));
      if (!raw) return emptyParams();
      return normalizeParams(JSON.parse(raw));
    } catch (e) {
      return emptyParams();
    }
  }

  function hasParams(p) {
    return !!(p && (p.system_prompt || p.temperature != null || p.max_tokens != null));
  }

  function writeChatParams(conversationId, params) {
    try {
      var key = paramsKey(conversationId);
      if (!hasParams(params)) localStorage.removeItem(key);
      else localStorage.setItem(key, JSON.stringify(params));
    } catch (e) { /* 隐私模式等场景，静默失败 */ }
  }

  // 把面板当前的值读成参数对象（空/默认都不下发）。
  function currentPanelParams() {
    var out = emptyParams();
    if (paramsUi.textarea) out.system_prompt = paramsUi.textarea.value || '';
    if (paramsUi.range && paramsUi.useDefault && !paramsUi.useDefault.checked) {
      var v = parseFloat(paramsUi.range.value);
      if (isFinite(v)) out.temperature = v;
    }
    if (paramsUi.maxTokens && paramsUi.maxTokens.value.trim() !== '') {
      var m = parseInt(paramsUi.maxTokens.value, 10);
      if (isFinite(m) && m > 0) out.max_tokens = m;
    }
    return out;
  }

  function persistPanelParams() {
    if (!paramsUi.el) return;
    writeChatParams(state.currentId, currentPanelParams());
  }

  // 切换会话时把面板刷成该会话自己的值（新会话读 __draft）。
  // 会话模式（chat/work/code/learn）按会话记住。为什么必须还原：
  // 后端用的是**会话上存的**模式，如果下拉不跟着变，用户切回旧会话时会看到
  // 一个跟实际行为不符的下拉，以为模式没生效。
  function kindStorageKey(id) {
    return 'blogmy:chatkind:' + (id === null || id === undefined ? '__draft' : id);
  }

  function refreshKindSelect() {
    if (!els.kindSelect) return;
    var saved = '';
    try { saved = window.localStorage.getItem(kindStorageKey(state.currentId)) || ''; } catch (e) { saved = ''; }
    els.kindSelect.value = saved || 'work';
    // 顶栏的四模式胶囊是同一份状态的另一个视图，切会话后要跟着回到该会话记住的模式。
    if (window.KIND_SWITCH && window.KIND_SWITCH.sync) window.KIND_SWITCH.sync();
  }

  function persistKind() {
    if (!els.kindSelect) return;
    try {
      window.localStorage.setItem(kindStorageKey(state.currentId), els.kindSelect.value || 'work');
    } catch (e) { /* 隐私模式下写不了，忽略 */ }
    // 模式一变，卡片上的模式标签也要变（不管这次变化是下拉触发的还是胶囊触发的）。
    if (window.KIND_SWITCH && window.KIND_SWITCH.sync) window.KIND_SWITCH.sync();
    notifyTaskCard();
  }

  // 换模式就记住（按会话）；面板刷新时再还原回来。
  if (els.kindSelect) els.kindSelect.addEventListener('change', persistKind);

  function refreshParamsPanel() {
    refreshKindSelect();
    if (!paramsUi.el) return;
    var p = readChatParams(state.currentId);
    if (paramsUi.textarea) paramsUi.textarea.value = p.system_prompt || '';
    var hasTemp = typeof p.temperature === 'number';
    if (paramsUi.range) {
      paramsUi.range.value = hasTemp ? String(p.temperature) : '1';
      paramsUi.range.disabled = !hasTemp;
    }
    if (paramsUi.useDefault) paramsUi.useDefault.checked = !hasTemp;
    if (paramsUi.rangeValue) paramsUi.rangeValue.textContent = hasTemp ? Number(p.temperature).toFixed(1) : '默认';
    if (paramsUi.maxTokens) paramsUi.maxTokens.value = p.max_tokens == null ? '' : String(p.max_tokens);
  }

  function buildParamsButton() {
    if (!els.composer || els.composer.querySelector('#chat-params-btn')) return;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.id = 'chat-params-btn';
    btn.className = 'btn btn-ghost chat-params-btn';
    btn.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><line x1="4" y1="21" x2="4" y2="14"></line><line x1="4" y1="10" x2="4" y2="3"></line><line x1="12" y1="21" x2="12" y2="12"></line><line x1="12" y1="8" x2="12" y2="3"></line><line x1="20" y1="21" x2="20" y2="16"></line><line x1="20" y1="12" x2="20" y2="3"></line><line x1="1" y1="14" x2="7" y2="14"></line><line x1="9" y1="8" x2="15" y2="8"></line><line x1="17" y1="16" x2="23" y2="16"></line></svg><span>参数</span>';
    btn.title = '对话参数：额外系统提示词 / 温度 / 最大 token（本会话）';
    btn.setAttribute('aria-label', '对话参数');
    btn.setAttribute('aria-expanded', 'false');
    btn.addEventListener('click', function () { toggleParamsPanel(); });
    // 参数按钮要落在输入框那一排胶囊里（发送键左边），不能直接 append 到 form 上——
    // form 现在是竖向 flex（textarea / 工具条 / …），直接挂上去会让这颗胶囊单独占一行。
    // 旧结构里发送键是 form 的直接子节点，所以两种位置都得兼容。
    var toolbar = els.composer.querySelector('.composer-toolbar');
    if (toolbar && els.sendBtn && els.sendBtn.parentNode === toolbar) toolbar.insertBefore(btn, els.sendBtn);
    else if (els.sendBtn && els.sendBtn.parentNode === els.composer) els.composer.insertBefore(btn, els.sendBtn);
    else (toolbar || els.composer).appendChild(btn);
    paramsUi.btn = btn;
  }

  function buildParamsPanel() {
    if (!els.composerBox || paramsUi.el) return;
    var panel = document.createElement('div');
    panel.className = 'chat-params card';
    panel.hidden = true;

    var tag = document.createElement('div');
    tag.className = 'card-tag';
    tag.textContent = '对话参数';
    panel.appendChild(tag);

    // 额外系统提示词（追加，不替换内置提示词）
    var f1 = document.createElement('div');
    f1.className = 'field';
    var l1 = document.createElement('label');
    l1.textContent = '额外系统提示词';
    l1.setAttribute('for', 'chat-params-system');
    var ta = document.createElement('textarea');
    ta.id = 'chat-params-system';
    ta.className = 'input chat-params-textarea';
    ta.rows = 3;
    ta.placeholder = '追加在系统提示词末尾，留空 = 不加';
    ta.title = '追加在系统提示词末尾的额外指令，不替换内置提示词';
    ta.setAttribute('aria-label', '额外系统提示词');
    f1.appendChild(l1);
    f1.appendChild(ta);
    panel.appendChild(f1);

    // 温度 0~2，步进 0.1；勾「用默认」就不下发该字段
    var f2 = document.createElement('div');
    f2.className = 'field';
    var l2 = document.createElement('label');
    l2.textContent = '温度';
    f2.appendChild(l2);
    var row = document.createElement('div');
    row.className = 'chat-params-temp-row';
    var range = document.createElement('input');
    range.type = 'range';
    range.min = '0';
    range.max = '2';
    range.step = '0.1';
    range.value = '1';
    range.className = 'chat-params-range';
    range.setAttribute('aria-label', '温度');
    range.title = '采样温度：越高越随机，越低越稳定';
    var valEl = document.createElement('span');
    valEl.className = 'chat-params-value';
    valEl.textContent = '默认';
    var useDefault = document.createElement('label');
    useDefault.className = 'chat-params-default';
    useDefault.title = '不发送温度，使用后端默认值';
    var cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = true;
    useDefault.appendChild(cb);
    useDefault.appendChild(document.createTextNode('用默认'));
    row.appendChild(range);
    row.appendChild(valEl);
    row.appendChild(useDefault);
    f2.appendChild(row);
    panel.appendChild(f2);

    // 最大 token：留空 = 用后端默认
    var f3 = document.createElement('div');
    f3.className = 'field';
    var l3 = document.createElement('label');
    l3.textContent = '最大 token';
    l3.setAttribute('for', 'chat-params-maxtokens');
    var num = document.createElement('input');
    num.id = 'chat-params-maxtokens';
    num.type = 'number';
    num.min = '1';
    num.step = '1';
    num.className = 'input chat-params-number';
    num.placeholder = '留空 = 用默认';
    num.title = '限制这条回复的最大 token 数，留空用后端默认';
    num.setAttribute('aria-label', '最大 token');
    f3.appendChild(l3);
    f3.appendChild(num);
    panel.appendChild(f3);

    var note = document.createElement('p');
    note.className = 'card-sub chat-params-note';
    note.textContent = '以上参数只作用于当前会话，切换会话各自独立。';
    panel.appendChild(note);

    els.composerBox.appendChild(panel);
    paramsUi.el = panel;
    paramsUi.textarea = ta;
    paramsUi.range = range;
    paramsUi.rangeValue = valEl;
    paramsUi.useDefault = cb;
    paramsUi.maxTokens = num;

    ta.addEventListener('input', persistPanelParams);
    range.addEventListener('input', function () {
      if (cb.checked) { cb.checked = false; range.disabled = false; }
      valEl.textContent = Number(range.value).toFixed(1);
      persistPanelParams();
    });
    cb.addEventListener('change', function () {
      range.disabled = cb.checked;
      valEl.textContent = cb.checked ? '默认' : Number(range.value).toFixed(1);
      persistPanelParams();
    });
    num.addEventListener('input', persistPanelParams);
  }

  function toggleParamsPanel(force) {
    if (!paramsUi.el) return;
    var want = typeof force === 'boolean' ? force : paramsUi.el.hidden;
    if (!want) { closeParamsPanel(); return; }
    closePromptPicker();
    refreshParamsPanel();
    paramsUi.el.hidden = false;
    if (paramsUi.btn) paramsUi.btn.setAttribute('aria-expanded', 'true');
  }

  function closeParamsPanel() {
    if (!paramsUi.el || paramsUi.el.hidden) return;
    paramsUi.el.hidden = true;
    if (paramsUi.btn) paramsUi.btn.setAttribute('aria-expanded', 'false');
  }

  // 把本会话的参数合并进请求体：没设的字段一律不带，避免覆盖后端默认值。
  function applyChatParamsToPayload(payload, conversationId) {
    var p = readChatParams(conversationId);
    if (p.system_prompt && p.system_prompt.trim()) payload.system_prompt = p.system_prompt;
    if (typeof p.temperature === 'number') payload.temperature = p.temperature;
    if (typeof p.max_tokens === 'number' && p.max_tokens > 0) payload.max_tokens = p.max_tokens;
    return payload;
  }

  // 新会话选中参数后发送：把 __draft 的参数迁移到刚创建的会话，下个新会话重新干净。
  function migrateDraftParams(newId) {
    var p = readChatParams(null);
    if (!hasParams(p)) return;
    writeChatParams(newId, p);
    writeChatParams(null, emptyParams());
  }

  // 子会话折叠状态：按父会话 id 记，刷新后保持。搜索时一律展开，避免匹配项被藏起来。
  var CONV_COLLAPSED_KEY = 'blogmy:conv-collapsed';
  var collapsedConvParents = (function () {
    try {
      var raw = JSON.parse(window.localStorage.getItem(CONV_COLLAPSED_KEY) || '[]');
      return new Set((Array.isArray(raw) ? raw : []).map(Number).filter(function (n) { return n > 0; }));
    } catch (e) {
      return new Set();
    }
  })();

  function saveCollapsedConvParents() {
    try {
      window.localStorage.setItem(CONV_COLLAPSED_KEY, JSON.stringify(Array.from(collapsedConvParents)));
    } catch (e) {
      // 隐私模式忽略。
    }
  }

  // 会话排序：置顶优先，其次按最近打开/更新时间。
  function sortConversations(rows) {
    rows.sort(function (a, b) {
      var am = conversationMetaFor(a.id);
      var bm = conversationMetaFor(b.id);
      if (!!am.pinned !== !!bm.pinned) return am.pinned ? -1 : 1;
      return (Number(bm.openedAt) || Number(b.updated_at) || Number(b.id) || 0) - (Number(am.openedAt) || Number(a.updated_at) || Number(a.id) || 0);
    });
    return rows;
  }

  function convRowEl(conv, opts) {
    opts = opts || {};
    var row = document.createElement('div');
    var meta = conversationMetaFor(conv.id);
    row.className = 'conv-row' + (meta.pinned ? ' is-pinned' : '') + (meta.unread ? ' is-unread' : '') +
      (opts.hasChildren ? ' has-children' : '') + (opts.collapsed ? ' is-collapsed' : '');

    var isChild = !!conv.parent_id;
    var parentTitle = conv.parent_title || '';
    if (isChild && !parentTitle) {
      var parent = state.conversations.filter(function (c) {
        return c.id === conv.parent_id;
      })[0];
      parentTitle = (parent && parent.title) || '';
    }

    // 有子会话的行才给折叠箭头；它和标题同一行，但用绝对定位放在原来圆点的位置，
    // 这样父行与普通行的标题仍然左对齐。
    if (opts.hasChildren && !opts.filtering) {
      var toggle = document.createElement('button');
      toggle.type = 'button';
      toggle.className = 'conv-toggle';
      toggle.setAttribute('aria-expanded', opts.collapsed ? 'false' : 'true');
      toggle.setAttribute('aria-label', (opts.collapsed ? '展开' : '收起') + '子会话：' + (conv.title || '新对话'));
      toggle.title = opts.collapsed ? '展开子会话' : '收起子会话';
      toggle.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6"></path></svg>';
      row.appendChild(toggle);
    }

    var item = document.createElement('button');
    item.type = 'button';
    item.className = 'conv-item' + (conv.id === state.currentId ? ' active' : '') + (isChild ? ' is-child' : '');
    item.setAttribute('aria-label', (conv.title || '新对话') + (isChild ? '，子会话' : ''));
    item.title = isChild ? ('子会话 · 父会话：' + (parentTitle || '未知')) : (conv.title || '新对话');
    if (conv.id === state.currentId) item.setAttribute('aria-current', 'true');
    conversationButtons.set(conv.id, item);
    if (conv.id === state.currentId) activeConversationButton = item;

    if (isChild) {
      var childBadge = document.createElement('span');
      childBadge.className = 'conv-child-badge';
      childBadge.textContent = '子';
      item.appendChild(childBadge);
    }

    var title = document.createElement('span');
    title.className = 'conv-title';
    title.textContent = conv.title || '新对话';
    item.appendChild(title);
    if (meta.unread) {
      var unread = document.createElement('span');
      unread.className = 'conv-unread-dot';
      unread.setAttribute('aria-label', '有新消息');
      item.appendChild(unread);
    }
    item.addEventListener('click', function () {
      openConversation(conv.id);
    });
    row.appendChild(item);

    var actions = document.createElement('div');
    actions.className = 'conv-actions';
    actions.appendChild(convActionButton(meta.pinned ? '★' : '☆', meta.pinned ? '取消置顶会话' : '置顶会话', meta.pinned ? '取消置顶「' + (conv.title || '新对话') + '」' : '置顶「' + (conv.title || '新对话') + '」', function () {
      meta.pinned = !meta.pinned;
      saveConversationMeta();
      renderConvList();
    }, meta.pinned ? 'is-pinned' : ''));
    actions.appendChild(convActionButton('✎', '重命名会话', '重命名「' + (conv.title || '新对话') + '」', function () {
      renameConversation(conv.id);
    }));
    actions.appendChild(convActionButton('⇩', '导出会话', '导出「' + (conv.title || '新对话') + '」为 Markdown', function () {
      exportConversation(conv.id);
    }));
    actions.appendChild(convActionButton('×', '删除会话', '删除会话：' + (conv.title || '新对话'), function () {
      deleteConversation(conv.id);
    }, 'is-danger'));
    row.appendChild(actions);

    return row;
  }

  function renderConvList() {
    els.convList.textContent = '';
    conversationButtons.clear();
    activeConversationButton = null;
    if (state.convError) {
      els.convList.appendChild(stateBox('会话加载失败：' + state.convError, loadConversations));
      return;
    }
    if (!state.conversations.length) {
      var hint = document.createElement('div');
      hint.className = 'list-hint';
      hint.textContent = '还没有会话，点击上方「新建对话」。';
      els.convList.appendChild(hint);
      return;
    }
    var kw = (els.convFilter && els.convFilter.value ? els.convFilter.value : '').trim().toLowerCase();
    var list = state.conversations.filter(function (conv) {
      return !kw || (conv.title || '').toLowerCase().indexOf(kw) !== -1;
    });
    if (!list.length) {
      var noMatch = document.createElement('div');
      noMatch.className = 'list-hint';
      noMatch.textContent = '没有找到匹配「' + els.convFilter.value.trim() + '」的会话。';
      els.convList.appendChild(noMatch);
      return;
    }

    // 子会话挂到父会话下面：先按 parent_id 建索引，再只渲染根会话 + 各自的孩子。
    // 父会话不在当前筛选结果里时，孩子按根会话处理（不会被吞掉）。
    var inList = {};
    list.forEach(function (conv) { inList[conv.id] = true; });
    var childrenOf = {};
    list.forEach(function (conv) {
      if (conv.parent_id && inList[conv.parent_id]) {
        (childrenOf[conv.parent_id] = childrenOf[conv.parent_id] || []).push(conv);
      }
    });
    var roots = sortConversations(list.filter(function (conv) {
      return !(conv.parent_id && inList[conv.parent_id]);
    }));
    var filtering = !!kw;

    roots.forEach(function (conv) {
      var kids = childrenOf[conv.id] ? sortConversations(childrenOf[conv.id]) : [];
      var hasKids = kids.length > 0;
      // 正在看的会话、或置顶的子会话在折叠组里时强制展开：
      // 否则「当前会话」或用户手动置顶的那条会被藏起来。
      var activeInside = kids.some(function (kid) { return kid.id === state.currentId; });
      var pinnedInside = kids.some(function (kid) { return conversationMetaFor(kid.id).pinned; });
      var collapsed = hasKids && !filtering && !activeInside && !pinnedInside && collapsedConvParents.has(conv.id);
      var row = convRowEl(conv, { hasChildren: hasKids, collapsed: collapsed, filtering: filtering });
      els.convList.appendChild(row);
      if (!hasKids) return;

      var group = document.createElement('div');
      group.className = 'conv-children' + (collapsed ? ' is-collapsed' : '');
      group.id = 'conv-children-' + conv.id;
      group.dataset.parentId = String(conv.id);
      var inner = document.createElement('div');
      inner.className = 'conv-children-inner';
      kids.forEach(function (kid) { inner.appendChild(convRowEl(kid, {})); });
      group.appendChild(inner);
      els.convList.appendChild(group);

      var toggle = row.querySelector('.conv-toggle');
      if (!toggle) return;
      toggle.setAttribute('aria-controls', group.id);
      toggle.addEventListener('click', function () {
        var next = !group.classList.contains('is-collapsed');
        group.classList.toggle('is-collapsed', next);
        row.classList.toggle('is-collapsed', next);
        toggle.setAttribute('aria-expanded', next ? 'false' : 'true');
        toggle.title = next ? '展开子会话' : '收起子会话';
        toggle.setAttribute('aria-label', (next ? '展开' : '收起') + '子会话：' + (conv.title || '新对话'));
        if (next) collapsedConvParents.add(conv.id);
        else collapsedConvParents.delete(conv.id);
        saveCollapsedConvParents();
      });
    });
  }

  function convActionButton(label, title, ariaLabel, onClick, extraClass) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn btn-ghost conv-action' + (extraClass ? ' ' + extraClass : '');
    btn.textContent = label;
    btn.title = title;
    btn.setAttribute('aria-label', ariaLabel);
    btn.disabled = state.streaming;
    btn.addEventListener('click', function (ev) {
      ev.stopPropagation();
      onClick();
    });
    return btn;
  }

  // ---- Day 13：多视图（hash 路由）与列表四种状态 ----

  // 契约 §7：页签由 5 个减为 3 个（密钥移到右上角按钮、知识库并入对话视图）。
  // 新增的 assistants / prompts 两个视图的 DOM 与脚本在 index.html 里，逻辑各在自己的 js 文件里；
  // 这里只把它们登记进视图表，切过去时由 index.html 末尾的内联钩子调 init()。
  var VIEW_TITLES = { chat: '对话', taskboard: '任务可视化', models: '模型', assistants: '助手', prompts: '提示词', skills: '技能', cua: '电脑操控' };

  function stateBox(text, onRetry) {
    var box = document.createElement('div');
    box.className = 'list-state';
    var msg = document.createElement('div');
    msg.className = 'list-state-text';
    msg.textContent = text;
    box.appendChild(msg);
    if (onRetry) {
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'btn btn-ghost list-state-btn';
      btn.textContent = '重试';
      btn.addEventListener('click', onRetry);
      box.appendChild(btn);
    }
    return box;
  }

  // 列表四种状态统一渲染：loading / empty / error / ready
  function renderListPanel(container, options) {
    container.textContent = '';
    if (options.status === 'loading') {
      var load = document.createElement('div');
      load.className = 'list-hint';
      load.textContent = '加载中…';
      container.appendChild(load);
      return;
    }
    if (options.status === 'empty') {
      var empty = document.createElement('div');
      empty.className = 'list-hint';
      empty.textContent = options.emptyText;
      container.appendChild(empty);
      return;
    }
    if (options.status === 'error') {
      container.appendChild(stateBox(options.errorText, options.onRetry));
      return;
    }
    options.rows.forEach(function (row) {
      var item = document.createElement('div');
      item.className = 'item-row';
      var name = document.createElement('span');
      name.className = 'item-name';
      name.textContent = row.title;
      item.appendChild(name);
      if (row.badge) {
        var badge = document.createElement('span');
        badge.className = 'item-badge' + (row.badgeOk ? '' : ' item-badge-warn');
        badge.textContent = row.badge;
        item.appendChild(badge);
      }
      container.appendChild(item);
    });
  }

  var modelsViewCache = null;
  var modelsViewPending = false;

  function renderModelsRows(models) {
    if (!models.length) {
      renderListPanel(els.modelsList, {
        status: 'empty',
        emptyText: '暂无可用模型，去「密钥」里配置 API Key 后再试。'
      });
      return;
    }
    renderListPanel(els.modelsList, {
      status: 'ready',
      rows: models.map(function (model) {
        return {
          title: model.label || model.id,
          badge: model.has_key ? '已配置' : '未配置 Key',
          badgeOk: !!model.has_key
        };
      })
    });
  }

  async function loadModelsView() {
    if (!els.modelsList) return;
    if (!els.modelsList.childElementCount) {
      if (state.models.length) {
        renderModelsRows(state.models);
        modelsViewCache = state.models;
      }
      else renderListPanel(els.modelsList, { status: 'loading' });
    }
    if (modelsViewPending) return;
    modelsViewPending = true;
    var models;
    try {
      models = await apiJSON('/api/models');
    } catch (e) {
      if (!modelsViewCache) {
        renderListPanel(els.modelsList, {
          status: 'error',
          errorText: '模型加载失败：' + (e.message || '未知错误'),
          onRetry: loadModelsView
        });
      }
      modelsViewPending = false;
      return;
    }
    models = Array.isArray(models) ? models : [];
    if (!modelsViewCache || JSON.stringify(modelsViewCache) !== JSON.stringify(models)) {
      renderModelsRows(models);
    }
    modelsViewCache = models;
    modelsViewPending = false;
  }

  async function loadKeysView() {
    if (!els.keysList) return;
    renderListPanel(els.keysList, { status: 'loading' });
    var keys;
    try {
      keys = await apiJSON('/api/keys');
    } catch (e) {
      renderListPanel(els.keysList, {
        status: 'error',
        errorText: '密钥加载失败：' + (e.message || '未知错误'),
        onRetry: loadKeysView
      });
      return;
    }
    keys = Array.isArray(keys) ? keys : [];
    if (!keys.length) {
      renderListPanel(els.keysList, {
        status: 'empty',
        emptyText: '还没有保存 API Key，去设置页添加一个吧。'
      });
      return;
    }
    renderListPanel(els.keysList, {
      status: 'ready',
      rows: keys.map(function (key) {
        return {
          title: key.endpoint || ('Key #' + key.id),
          badge: key.masked || '已保存',
          badgeOk: true
        };
      })
    });
  }

  function rawHashName() {
    return (location.hash || '').replace(/^#\/?/, '');
  }

  function viewFromHash() {
    var name = rawHashName();
    return VIEW_TITLES[name] ? name : 'chat';
  }

  // 契约 §7：密钥入口在右上角，但 #/keys 路由仍要可达；#/kb 是旧书签。
  // 两者都**不切换视图**——在当前视图上打开弹层 / 展开记忆面板，所以页签只有 3 个也不会迷路。
  function handleRoute() {
    var name = rawHashName();
    if (name === 'keys') {
      showView(state.view || 'chat');
      openKeysPanel();
      return;
    }
    if (name === 'kb') {
      showView(state.view || 'chat');
      openMemoryPanel();
      return;
    }
    showView(viewFromHash());
  }

  // ---- 契约 §7：密钥弹层（右上角入口 + #/keys 路由）----
  function openKeysPanel() {
    var dlg = els.keysDialog;
    if (!dlg) return;
    if (els.keysOpen) els.keysOpen.setAttribute('aria-expanded', 'true');
    if (!dlg.open) {
      if (typeof dlg.showModal === 'function') dlg.showModal();
      else dlg.setAttribute('open', 'open');
    }
    loadKeysView();
  }

  function closeKeysPanel() {
    var dlg = els.keysDialog;
    if (!dlg || !dlg.open) return;
    if (typeof dlg.close === 'function') dlg.close();
    else dlg.removeAttribute('open');
  }

  // ---- 契约 §7：记忆面板（原知识库，搬进对话视图，kb.js 与元素 id 都不动）----
  function openMemoryPanel() {
    if (!els.memoryPanel) return;
    // 设 .open 会触发 toggle 事件，刷新交给 toggle 处理器，避免同一次展开拉两遍。
    if (els.memoryPanel.open) {
      refreshMemory();
      return;
    }
    els.memoryPanel.open = true;
  }

  function refreshMemory() {
    if (window.KB) window.KB.onShow();
    syncMemoryCount();
  }

  // 摘要行上的文档数直接镜像 kb.js 写进 #kb-stat-docs 的值，避免两边各算一遍。
  function syncMemoryCount() {
    if (!els.memoryCount) return;
    var docs = document.getElementById('kb-stat-docs');
    var text = docs ? docs.textContent : '';
    els.memoryCount.textContent = text && text !== '—' ? text + ' 个文档' : '';
  }

  // 视图切换：能开原生 View Transition 就包一层（对话 ↔ 任务看板/技能/电脑操控），
  // 关掉动效或浏览器不支持时退回原来的瞬时切换。
  function showView(name) {
    if (state.view === name && els.app && els.app.getAttribute('data-view') === name) return;
    var reduced = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (document.startViewTransition && !reduced) {
      try {
        document.startViewTransition(function () { applyView(name); });
        return;
      } catch (e) {
        // 动画起不来就直切，别让路由卡住。
      }
    }
    applyView(name);
  }

  function applyView(name) {
    state.view = name;
    if (window.APP_RUNTIME) window.APP_RUNTIME.wake();
    var viewStart = performance.now();
    requestAnimationFrame(function () {
      if (window.APP_PERF) window.APP_PERF.record('view', name, performance.now() - viewStart);
    });
    if (els.app) els.app.setAttribute('data-view', name);
    var tabs = document.querySelectorAll('.view-tab');
    for (var i = 0; i < tabs.length; i++) {
      var active = tabs[i].getAttribute('data-view') === name;
      tabs[i].classList.toggle('active', active);
      if (active) {
        tabs[i].setAttribute('aria-current', 'page');
      } else {
        tabs[i].removeAttribute('aria-current');
      }
    }
    if (name === 'chat') {
      var conv = state.conversations.filter(function (c) {
        return c.id === state.currentId;
      })[0];
      els.topbarTitle.textContent = (conv && conv.title) || '新对话';
    } else {
      els.topbarTitle.textContent = VIEW_TITLES[name];
    }
    if (name === 'models') loadModelsView();
    if (name === 'taskboard' && window.TASKBOARD) window.TASKBOARD.onShow();
    // 技能视图：skills.js 和 kb.js / cua.js 一样先于本文件加载，列表加载交给它自己。
    if (name === 'skills' && window.SKILLS) window.SKILLS.onShow();
    // Day 25：电脑操控视图（契约 §4）。cua.js 与 kb.js 一样先于本文件加载，所以这里必然可用。
    if (name === 'cua' && window.CUA) window.CUA.onShow();
  }

  // currentAssistantId 返回当前选中的助手 id；0 表示「不使用助手」，全部走全局设置。
  // 后端据此把助手的灵魂/档位/工作区覆盖到本轮对话上。
  function currentAssistantId() {
    if (!els.assistantSelect) return 0;
    var v = parseInt(els.assistantSelect.value, 10);
    return isFinite(v) && v > 0 ? v : 0;
  }

  // loadAssistants 把助手列表填进左下角的下拉框。
  // 拉不到就只保留「不使用助手」一项——助手是锦上添花，不该把对话卡住。
  async function loadAssistants() {
    if (!els.assistantSelect) return;
    var list = [];
    try {
      var body = await apiJSON('/api/assistants');
      list = (body && body.assistants) || [];
    } catch (e) {
      list = [];
    }

    var saved = '';
    try { saved = window.localStorage.getItem('blogmy:assistant') || ''; } catch (e) { saved = ''; }

    els.assistantSelect.textContent = '';
    var none = document.createElement('option');
    none.value = '';
    none.textContent = '不使用助手';
    els.assistantSelect.appendChild(none);

    var usable = list.filter(function (a) { return a && a.enabled !== false; });
    usable.forEach(function (a) {
      var opt = document.createElement('option');
      opt.value = String(a.id);
      opt.textContent = a.name + (a.model ? ' · ' + a.model : '');
      els.assistantSelect.appendChild(opt);
    });

    // 上次选的助手可能已被删除或停用：这种情况自动回落到「不使用助手」，
    // 否则下拉框会指向一个不存在的 id，请求发出去后端也认不出来。
    var stillThere = usable.some(function (a) { return String(a.id) === saved; });
    els.assistantSelect.value = stillThere ? saved : '';

    if (!els.assistantSelect.dataset.bound) {
      els.assistantSelect.dataset.bound = '1';
      els.assistantSelect.addEventListener('change', function () {
        try { window.localStorage.setItem('blogmy:assistant', els.assistantSelect.value); } catch (e) { /* 隐私模式下写不了，忽略 */ }
      });
    }
  }

  async function loadModels() {
    try {
      var models = await apiJSON('/api/models');
      state.models = Array.isArray(models) ? models : [];
    } catch (e) {
      state.models = [];
      toast('模型列表加载失败：' + e.message);
    }

    els.modelSelect.textContent = '';
    if (!state.models.length) {
      var empty = document.createElement('option');
      empty.value = '';
      empty.textContent = '暂无可用模型';
      els.modelSelect.appendChild(empty);
      state.model = '';
      return;
    }

    state.models.forEach(function (model) {
      var opt = document.createElement('option');
      opt.value = model.id;
      opt.textContent = model.has_key ? model.label : model.label + ' · 未配置 Key';
      els.modelSelect.appendChild(opt);
    });

    var stored = null;
    try {
      stored = localStorage.getItem('chat.model');
    } catch (e) {}
    var exists = state.models.some(function (model) {
      return model.id === stored;
    });
    state.model = exists ? stored : state.models[0].id;
    els.modelSelect.value = state.model;
    // 知识库视图的问答提示语依赖顶栏模型，模型列表是异步到的，这里补一次刷新。
    if (window.KB && window.KB.refreshAskModel) window.KB.refreshAskModel();
  }

  async function loadConversations() {
    state.convError = '';
    try {
      var list = await apiJSON('/api/conversations');
      state.conversations = Array.isArray(list) ? list : [];
      state.conversations.forEach(function (conv) {
        if (!conv || conv.id === state.currentId) return;
        var meta = conversationMetaFor(conv.id);
        var updated = Date.parse(conv.updated_at || '');
        if (conv.unread || (meta.openedAt && updated && updated > Number(meta.openedAt))) meta.unread = true;
      });
      saveConversationMeta();
    } catch (e) {
      state.conversations = [];
      state.convError = e.message || '未知错误';
    }
    renderConvList();
  }

  function startConversationPolling() {
    if (conversationPoll) return;
    conversationPoll = startPoll(function () {
      if (state.streaming) return;
      return loadConversations();
    }, 30000);
  }

  function titleFromQuestion(text) {
    return Array.from(String(text || '').replace(/\s+/g, ' ').trim()).slice(0, 30).join('') || '新对话';
  }

  function setConversationTitle(id, title) {
    var conv = state.conversations.filter(function (item) { return Number(item.id) === Number(id); })[0];
    if (conv && (!conv.title || conv.title === '新对话')) conv.title = title;
    if (Number(state.currentId) === Number(id)) els.topbarTitle.textContent = title;
    renderConvList();
  }

  async function refreshConversationTitle(id) {
    try {
      var list = await apiJSON('/api/conversations');
      if (!Array.isArray(list)) return;
      state.conversations = list;
      renderConvList();
      var conv = list.filter(function (item) { return Number(item.id) === Number(id); })[0];
      if (conv && Number(state.currentId) === Number(id)) {
        els.topbarTitle.textContent = conv.title || '新对话';
      }
    } catch (e) {
      // Keep the optimistic title if the sidebar refresh is temporarily unavailable.
    }
  }

  async function createConversation() {
    var conv = await apiJSON('/api/conversations', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: state.model })
    });
    state.conversations.unshift({
      id: conv.id,
      title: conv.title || '新对话',
      model: conv.model || state.model
    });
    return conv;
  }

  function newChat() {
    // 先收侧栏遮罩，而且**要在 streaming 守卫之前**：
    // 遮罩盖在页面上时，用户看到的是“点什么都没反应、打不了字”，
    // 而提前 return 会让 closeSidebar() 永远不执行。
    closeSidebar();
    if (state.streaming) return;
    if (state.view !== 'chat') location.hash = '#/chat';
    stashCurrentThread();
    state.currentId = null;
    stopSubagentPolling(true);
    stopChildRunPolling();
    els.thread.textContent = '';
    setEmptyState(true);
    els.topbarTitle.textContent = '新对话';
    markActiveConversation(null);
    refreshParamsPanel();
    resetTaskCounters();
    notifyTaskCard();
    clearTeamCard(); // 新对话没有团队数据，别把上一个会话的成员留在卡片上
    clearPlan(); // 计划属于会话：新对话不能沿用上一个会话的步骤
    clearUsage(); // 用量同样属于会话
    closeReview();
    els.input.focus();
  }

  async function openConversation(id, fromSubagentCard, parentIdHint, titleHint) {
    var currentConv = state.conversations.filter(function (c) {
      return c.id === state.currentId;
    })[0];
    var returningToParent = !!(currentConv && Number(currentConv.parent_id) === Number(id));
    if (state.streaming && !fromSubagentCard && !returningToParent) {
      toast('请等待当前回复结束');
      return;
    }
    closeSidebar();
    if (state.view !== 'chat') location.hash = '#/chat';
    if (id === state.currentId) return;
    stashCurrentThread();
    stopSubagentPolling(true);
    stopChildRunPolling();

    state.currentId = id;
    // 计划是进程内状态，打开会话时向后端恢复；请求回来前先清掉上一个会话的步骤。
    clearPlan();
    loadConversationPlan(id);
    var openedMeta = conversationMetaFor(id);
    openedMeta.openedAt = Date.now();
    openedMeta.unread = false;
    saveConversationMeta();
    clearTeamCard(); // 先藏掉上一个会话的团队行，等新数据回来再显示
    var conv = state.conversations.filter(function (c) {
      return c.id === id;
    })[0];
    if (!conv && parentIdHint) {
      conv = { id: id, parent_id: Number(parentIdHint), title: titleHint || '子 agent' };
      state.conversations.push(conv);
    }
    els.topbarTitle.textContent = (conv && conv.title) || '对话';
    if (conv && conv.model) {
      var known = state.models.some(function (m) {
        return m.id === conv.model;
      });
      if (known) {
        state.model = conv.model;
        els.modelSelect.value = conv.model;
      }
    }
    markActiveConversation(id);
    refreshParamsPanel();

    var cached = historyCache.get(id);
    historyPage = { id: id, before: cached && cached.before || 0, hasMore: !!(cached && cached.hasMore), busy: false };
    var savedDOM = historyDOMCache.get(id);
    if (cached) {
      historyCache.delete(id);
      historyCache.set(id, cached);
    }
    if (cached && savedDOM) {
      historyDOMCache.delete(id);
      els.thread.textContent = '';
      els.thread.appendChild(savedDOM);
      setEmptyState(!cached.history.length);
      scrollToBottom(true);
    } else if (cached) {
      renderConversationHistory(cached.history, cached.runs);
    } else {
      els.thread.textContent = '';
      setEmptyState(false);
      var loading = document.createElement('div');
      loading.className = 'list-hint';
      loading.textContent = '加载历史…';
      els.thread.appendChild(loading);
    }

    try {
      await loadConversationMessages(id);
      if (returningToParent && state.streaming) {
        // 主会话的流在切入子会话后仍会完成，但它的原始消息节点已离开 DOM。
        // 等该轮结束后重新拉一次历史，避免返回时只看到半截或旧内容。
        (function refreshParentAfterStream() {
          if (state.currentId !== id) return;
          if (state.streaming) {
            setTimeout(refreshParentAfterStream, 500);
            return;
          }
          historyCache.delete(id);
          loadConversationMessages(id).catch(function () {});
        })();
      }
      if (state.currentId === id) startChildRunPolling(conv);
    } catch (e) {
      if (!cached && state.currentId === id) {
        els.thread.textContent = '';
        setEmptyState(true);
      }
      toast('历史加载失败：' + e.message);
    }
  }

  window.CHAT = window.CHAT || {};
  window.CHAT.openConversation = function (id, parentId, title) {
    return openConversation(Number(id), true, parentId, title);
  };
  // 任务看板只读当前会话快照，不直接依赖 chat.js 的内部变量。
  // historyCache 中保留最近一次服务端校对后的消息，DOM 则补上当前尚未落库的流式内容。
  window.CHAT.getTaskSnapshot = function () {
    var conv = state.conversations.filter(function (c) { return c.id === state.currentId; })[0] || null;
    var cached = state.currentId != null ? historyCache.get(state.currentId) : null;
    var messages = cached && Array.isArray(cached.history) ? cached.history.filter(function (m) {
      return m && (m.role === 'user' || m.role === 'assistant');
    }).slice() : [];
    // 流式生成时最新一轮还没落库，补读当前 DOM，避免看板落后一整轮。
    if (els.thread) {
      var liveMessages = els.thread.querySelectorAll('.msg[data-role]');
      for (var mi = 0; mi < liveMessages.length; mi++) {
        var live = liveMessages[mi];
        var role = live.getAttribute('data-role');
        if (role !== 'user' && role !== 'assistant') continue;
        var raw = live.getAttribute('data-raw-content') || '';
        if (!raw) {
          var bubble = live.querySelector('.bubble');
          raw = bubble ? bubble.textContent : '';
        }
        var id = live.getAttribute('data-message-id');
        var exists = messages.some(function (m) {
          return (id && String(m.id || '') === id) || (!id && m.role === role && String(m.content || '') === raw);
        });
        if (!exists && raw) messages.push({ id: id || null, role: role, content: raw });
      }
    }
    var toolCards = [];
    if (els.thread) {
      var cards = els.thread.querySelectorAll('.tool-card, .subagent-card, .activity-memory');
      for (var i = 0; i < cards.length; i++) {
        var card = cards[i];
        toolCards.push({
          type: card.classList.contains('subagent-card') ? 'agent' : (card.classList.contains('activity-memory') ? 'memory' : 'tool'),
          name: (card.querySelector('.tool-card-name, .subagent-card-label, .activity-memory > summary') || {}).textContent || '',
          status: card.classList.contains('is-error') || card.classList.contains('is-failed') ? 'failed' : (card.classList.contains('is-running') ? 'running' : 'completed'),
          text: (card.querySelector('.tool-card-summary, .subagent-card-activity, .activity-memory-body') || {}).textContent || '',
          summary: (card.querySelector('.tool-card-summary, .subagent-card-activity, .activity-memory-body') || {}).textContent || ''
        });
      }
    }
    return {
      id: state.currentId,
      title: conv && conv.title ? conv.title : (els.topbarTitle && els.topbarTitle.textContent) || '新任务',
      model: state.model || (els.modelSelect && els.modelSelect.value) || '',
      kind: (els.kindSelect && els.kindSelect.value) || 'work',
      streaming: !!state.streaming,
      workspace: taskState.workspace || '',
      toolTotal: taskState.toolTotal,
      toolDone: taskState.toolDone,
      messages: messages,
      toolCards: toolCards
    };
  };

  async function deleteConversation(id) {
    if (state.streaming) {
      toast('请等待当前回复结束');
      return;
    }
    var conv = state.conversations.filter(function (c) {
      return c.id === id;
    })[0];
    var name = (conv && conv.title) || '该会话';
    // 用页内确认框而不是 window.confirm：系统弹窗和这套界面不搭，而且它在这里是同步阻塞的。
    var sure = window.UIASK
      ? await window.UIASK.confirm({
        title: '删除会话',
        message: '「' + name + '」以及它的全部消息都会被删掉，不可撤销。',
        okText: '删除',
        danger: true
      })
      : window.confirm('删除「' + name + '」？此操作不可撤销。');
    if (!sure) return;

    try {
      await apiJSON('/api/conversations/' + id, { method: 'DELETE' });
    } catch (e) {
      toast('删除失败：' + e.message);
      return;
    }

    state.conversations = state.conversations.filter(function (c) {
      return c.id !== id;
    });
    historyCache.delete(id);
    historyDOMCache.delete(id);
    if (state.currentId === id) {
      newChat();
      // 双保险：newChat 在 streaming 时会提前返回，那就不会收遮罩、也拿不回焦点。
      // 实测反馈是“删完当前会话就打不了字，得点另一个会话才恢复”，这里把这几件事都做完。
      closeSidebar();
      setStreaming(false);
      if (els.sendBtn) els.sendBtn.disabled = false;
      if (els.input) els.input.focus();
    } else {
      renderConvList();
    }
  }

  async function renameConversation(id) {
    if (state.streaming) {
      toast('请等待当前回复结束');
      return;
    }
    var conv = state.conversations.filter(function (c) { return c.id === id; })[0];
    var current = (conv && conv.title) || '新对话';
    // 同样不能用 prompt（见 onPickWorkspace 里的说明）。
    var next = window.UIASK
      ? await window.UIASK.prompt({ title: '重命名会话', label: '标题', value: current, okText: '保存' })
      : null;
    if (next == null) return;
    next = next.trim();
    if (!next) {
      toast('标题不能为空');
      return;
    }
    if (next === current) return;

    var body;
    try {
      body = await apiJSON('/api/conversations/' + id, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: next })
      });
    } catch (e) {
      toast('重命名失败：' + (e.message || '未知错误'));
      return;
    }
    var saved = (body && body.title) || next;
    if (conv) conv.title = saved;
    if (state.currentId === id) els.topbarTitle.textContent = saved;
    renderConvList();
    toast('已重命名');
  }

  function exportConversation(id) {
    if (state.streaming) {
      toast('请等待当前回复结束');
      return;
    }
    window.location = '/api/conversations/' + encodeURIComponent(id) + '/export';
  }

  // ---- 契约 §5/§7：工具调用卡片 ----
  // tool_call 先建卡片（运行中），同 id 的 tool_result 到了就地更新为完成/失败；
  // 完整结果塞进 <details> 折叠，长文本由 CSS 限高内部滚动。
  function summarizeToolArgs(args) {
    var obj = args;
    if (typeof obj === 'string') {
      var parsed = safeParse(obj);
      if (parsed === null) return obj.length > 120 ? obj.slice(0, 120) + '…' : obj;
      obj = parsed;
    }
    if (!obj || typeof obj !== 'object') return obj == null ? '' : String(obj);
    var parts = [];
    for (var k in obj) {
      if (!Object.prototype.hasOwnProperty.call(obj, k)) continue;
      var v = obj[k];
      if (typeof v === 'string') {
        v = v.replace(/\s+/g, ' ');
        if (v.length > 48) v = v.slice(0, 48) + '…';
      } else {
        try { v = JSON.stringify(v); } catch (e) { v = String(v); }
      }
      parts.push(k + '=' + v);
    }
    return parts.join(' · ');
  }

  function appendToolCards(beforeNode, target) {
    var wrap = document.createElement('details');
    wrap.className = 'tool-cards';
    wrap.open = true;
    wrap.hidden = true;
    wrap.setAttribute('data-active', 'true');
    var summary = document.createElement('summary');
    summary.className = 'tool-cards-summary';
    summary.textContent = '正在处理…';
    wrap.appendChild(summary);
    var list = document.createElement('div');
    list.className = 'tool-cards-list';
    wrap.appendChild(list);
    if (beforeNode && beforeNode.parentNode === els.thread) {
      els.thread.insertBefore(wrap, beforeNode);
    } else {
      (target || els.thread).appendChild(wrap);
    }
    return wrap;
  }

  // 工具轨迹用面向用户的动作名称，而不是把内部工具名直接暴露出来。
  // 内部名仍保存在 data-tool-name/title，便于调试和历史回放；视觉上保持截图里的
  // 「思考 / 网页获取 / 查看目标 / 运行命令」四类节奏。
  function toolDisplayMeta(name) {
    var raw = String(name || '').trim();
    var lower = raw.toLowerCase();
    if (/run[_-]?command|shell|terminal|exec/.test(lower)) {
      return { label: '运行命令', icon: '›_', kind: 'command' };
    }
    if (/memory|think|thought|reason|plan|goal/.test(lower)) {
      return { label: '思考', icon: '⚛', kind: 'thought' };
    }
    if (/web|fetch|browse|browser|http|url|^(?:search)(?:_|$)/.test(lower)) {
      return { label: '网页获取', icon: '▤', kind: 'web' };
    }
    if (/read|list|inspect|target|file|image|sql|query/.test(lower)) {
      return { label: '查看目标', icon: '◉', kind: 'inspect' };
    }
    return { label: raw || '工具', icon: '◇', kind: 'tool' };
  }

  function toolCardTitle(card, name) {
    if (!card) return;
    var meta = toolDisplayMeta(name);
    card.setAttribute('data-tool-kind', meta.kind);
    card.setAttribute('data-tool-name', String(name || ''));
    card.title = name ? String(name) : '';
    var icon = card.querySelector('.tool-card-icon');
    if (icon) {
      icon.textContent = meta.icon;
      icon.setAttribute('aria-label', meta.label);
    }
    var label = card.querySelector('.tool-card-name');
    if (label) label.textContent = meta.label;
    var separator = card.querySelector('.tool-card-separator');
    if (separator) separator.hidden = !name;
    return meta;
  }

  function toolCardFor(container, data, createIfMissing) {
    var id = data && data.id ? String(data.id) : '';
    var card = null;
    if (id) card = container.querySelector('[data-tool-id="' + id.replace(/"/g, '') + '"]');
    if (card || !createIfMissing) return card;
    card = document.createElement('div');
    card.className = 'tool-card is-running';
    if (id) card.setAttribute('data-tool-id', id);
    var head = document.createElement('div');
    head.className = 'tool-card-head';
    var icon = document.createElement('span');
    icon.className = 'tool-card-icon';
    icon.setAttribute('aria-hidden', 'true');
    head.appendChild(icon);
    var name = document.createElement('span');
    name.className = 'tool-card-name';
    head.appendChild(name);
    var separator = document.createElement('span');
    separator.className = 'tool-card-separator';
    separator.textContent = '·';
    separator.setAttribute('aria-hidden', 'true');
    head.appendChild(separator);
    // 历史轨迹里没有 arguments（契约 §5 只存 name/ok/summary/detail），空摘要就不占位。
    var argsText = summarizeToolArgs(data && data.arguments);
    if (argsText) {
      var args = document.createElement('span');
      args.className = 'tool-card-args';
      args.textContent = argsText;
      head.appendChild(args);
    }
    var status = document.createElement('span');
    status.className = 'tool-card-status';
    status.textContent = '运行中…';
    head.appendChild(status);
    card.appendChild(head);
    var summary = document.createElement('p');
    summary.className = 'tool-card-summary';
    summary.textContent = '正在调用…';
    card.appendChild(summary);
    toolCardTitle(card, (data && data.name) || '工具');
    var list = container.querySelector('.tool-cards-list') || container;
    container.hidden = false;
    container.open = true;
    list.appendChild(card);
    refreshActivitySummary(container);
    return card;
  }

  function refreshActivitySummary(container, finish) {
    if (!container || !container.classList || !container.classList.contains('tool-cards')) return;
    if (finish) container.setAttribute('data-active', 'false');
    var summary = container.querySelector('.tool-cards-summary');
    if (!summary) return;
    var toolCount = container.querySelectorAll('.tool-card, .subagent-card').length;
    var memoryCount = container.querySelectorAll('.activity-memory').length;
    var total = toolCount + memoryCount;
    if (finish && !total) container.hidden = true;
    if (container.getAttribute('data-active') === 'true') {
      var running = container.querySelector('.tool-card.is-running .tool-card-name');
      summary.textContent = running ? '正在调用 ' + running.textContent + '…' : (total ? '正在处理 · ' + total + ' 个步骤' : '正在整理上下文…');
    } else {
      var label = container.getAttribute('data-incomplete') === 'true' ? '执行已暂停' : '处理过程';
      summary.textContent = total ? label + ' · ' + total + ' 个步骤' : label;
    }
    if (finish) container.open = false;
  }

  function renderMemoryActivity(container, data) {
    if (!container || !data) return;
    var list = container.querySelector('.tool-cards-list') || container;
    container.hidden = false;
    container.open = true;
    var item = document.createElement('details');
    item.className = 'activity-memory' + ((data.degraded || data.saved === false) ? ' is-warn' : '');
    var summary = document.createElement('summary');
    summary.textContent = data.saved === false ? '本轮记忆未保存' : (data.degraded ? '记忆本轮未生效' : '已回忆相关记忆');
    item.appendChild(summary);
    var detail = data.preview || data.reason;
    if (detail) {
      var body = document.createElement('div');
      body.className = 'activity-memory-body';
      body.textContent = detail;
      item.appendChild(body);
    }
    list.appendChild(item);
    refreshActivitySummary(container);
  }

  // ---- 任务计划条 -------------------------------------------------------------
  // 数据源有两个：打开会话时 GET /api/agent/plan（后端内存态），以及实时 tool_result
  // 里的 display.plan。计划只读，增删改都由 agent 的 update_plan 完成。
  var planState = { steps: [], explanation: '' };

  function planMark(status) {
    switch (status) {
      case 'completed': return '✓';
      case 'in_progress': return '▶';
      case 'blocked': return '!';
      default: return '○';
    }
  }

  function renderPlan(plan) {
    if (!els.planPanel || !els.planSteps) return;
    var steps = plan && Array.isArray(plan.steps) ? plan.steps.filter(function (s) {
      return s && typeof s.step === 'string' && s.step.trim();
    }) : [];
    planState = { steps: steps, explanation: (plan && plan.explanation) || '' };
    if (!steps.length) {
      clearPlan();
      return;
    }
    var done = steps.filter(function (s) { return s.status === 'completed'; }).length;
    if (els.planProgress) els.planProgress.textContent = done + '/' + steps.length;
    els.planSteps.textContent = '';
    steps.forEach(function (step) {
      var li = document.createElement('li');
      li.className = 'plan-step is-' + (step.status || 'pending');
      var mark = document.createElement('span');
      mark.className = 'plan-step-mark';
      mark.setAttribute('aria-hidden', 'true');
      mark.textContent = planMark(step.status);
      var text = document.createElement('span');
      text.className = 'plan-step-text';
      text.textContent = step.step;
      li.appendChild(mark);
      li.appendChild(text);
      els.planSteps.appendChild(li);
    });
    els.planPanel.hidden = false;
    if (els.planPanel.classList.contains('is-collapsed')) {
      els.planToggle.setAttribute('aria-expanded', 'false');
    } else {
      els.planToggle.setAttribute('aria-expanded', 'true');
    }
  }

  function clearPlan() {
    planState = { steps: [], explanation: '' };
    if (!els.planPanel) return;
    els.planPanel.hidden = true;
    if (els.planSteps) els.planSteps.textContent = '';
    if (els.planProgress) els.planProgress.textContent = '';
  }

  async function loadConversationPlan(id) {
    if (!id) {
      clearPlan();
      return;
    }
    try {
      var plan = await apiJSON('/api/agent/plan?conversation_id=' + encodeURIComponent(id));
      // 用户可能在请求途中切走：只渲染当前会话的结果。
      if (state.currentId !== id) return;
      renderPlan(plan);
    } catch (e) {
      // 计划是辅助信息；接口失败不影响对话，保持当前位置。
    }
  }

  function setPlanCollapsed(collapsed) {
    if (!els.planPanel || !els.planToggle) return;
    els.planPanel.classList.toggle('is-collapsed', !!collapsed);
    els.planToggle.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    els.planToggle.setAttribute('aria-label', collapsed ? '展开任务计划' : '收起任务计划');
  }

  // ---- 本轮用量：后端在 done 之前推一条 usage 事件；价格是估算，仅供参考。----
  function formatTokens(n) {
    var v = Number(n) || 0;
    if (v >= 1000000) return (v / 1000000).toFixed(1) + 'M';
    if (v >= 1000) return (v / 1000).toFixed(1) + 'k';
    return String(v);
  }

  function showUsage(usage) {
    if (!els.usageChip || !usage) return;
    var parts = ['本轮 ' + formatTokens(usage.input_tokens) + ' in / ' + formatTokens(usage.output_tokens) + ' out'];
    if (Number(usage.cached_tokens) > 0) parts.push('缓存 ' + formatTokens(usage.cached_tokens));
    if (typeof usage.estimated_usd === 'number' && isFinite(usage.estimated_usd)) {
      parts.push('≈$' + usage.estimated_usd.toFixed(4));
    }
    els.usageChip.textContent = parts.join(' · ');
    els.usageChip.hidden = false;
    els.usageChip.title = usage.model ? ('模型：' + usage.model) : '';
  }

  function clearUsage() {
    if (!els.usageChip) return;
    els.usageChip.hidden = true;
    els.usageChip.textContent = '';
    els.usageChip.title = '';
  }

  // ---- 本轮改动：GET /api/agent/diff 拿 unified diff，按文件撤销走 /api/agent/restore。----
  function diffFiles(diff) {
    var seen = {};
    var out = [];
    String(diff || '').split('\n').forEach(function (line) {
      var m = /^diff --git a\/(.+?) b\/(.+)$/.exec(line);
      if (!m) return;
      var path = m[2] || m[1];
      if (!path || seen[path]) return;
      seen[path] = true;
      out.push(path);
    });
    return out;
  }

  function renderReview(body) {
    if (!els.reviewFiles || !els.reviewSummary) return;
    var diff = body && typeof body.diff === 'string' ? body.diff : '';
    els.reviewSummary.textContent = body && body.dirty
      ? (body.note ? body.note.replace(/\s+/g, ' ').trim() : '工作区有未提交改动')
      : '工作区干净';
    els.reviewFiles.textContent = '';
    var files = diffFiles(diff);
    if (!files.length) {
      var empty = document.createElement('li');
      empty.className = 'review-empty';
      empty.textContent = diff ? '改动无法按文件拆分（可能是二进制或未跟踪文件）。' : '没有可展示的改动。';
      els.reviewFiles.appendChild(empty);
    }
    files.forEach(function (path) {
      var li = document.createElement('li');
      li.className = 'review-file';
      var name = document.createElement('span');
      name.className = 'review-file-path';
      name.textContent = path;
      var undo = document.createElement('button');
      undo.type = 'button';
      undo.className = 'review-undo';
      undo.textContent = '撤销此文件';
      undo.setAttribute('data-path', path);
      undo.addEventListener('click', function () { restoreReviewFile(path, undo); });
      li.appendChild(name);
      li.appendChild(undo);
      els.reviewFiles.appendChild(li);
    });
    if (els.reviewDiff) {
      els.reviewDiff.textContent = diff;
      els.reviewDiff.hidden = !diff;
    }
  }

  async function refreshReview() {
    if (!els.reviewPanel) return;
    if (els.reviewFiles) {
      els.reviewFiles.textContent = '';
      var loading = document.createElement('li');
      loading.className = 'review-empty';
      loading.textContent = '正在读取改动…';
      els.reviewFiles.appendChild(loading);
    }
    try {
      var body = await apiJSON('/api/agent/diff');
      renderReview(body || {});
    } catch (e) {
      if (els.reviewFiles) {
        els.reviewFiles.textContent = '';
        var failed = document.createElement('li');
        failed.className = 'review-empty is-error';
        failed.textContent = '读取改动失败：' + (e.message || '未知错误');
        els.reviewFiles.appendChild(failed);
      }
    }
  }

  async function restoreReviewFile(path, button) {
    if (!path) return;
    if (button) { button.disabled = true; button.textContent = '撤销中…'; }
    try {
      var res = await api('/api/agent/restore', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path: path, session_id: state.currentId == null ? '' : String(state.currentId) })
      });
      var body = await res.json().catch(function () { return {}; });
      if (!res.ok || body.is_error) throw new Error(body.error || body.content || '撤销失败');
      toast('已撤销 ' + path);
      await refreshReview();
    } catch (e) {
      toast('撤销失败：' + (e.message || '未知错误'));
      if (button) { button.disabled = false; button.textContent = '撤销此文件'; }
    }
  }

  function openReview() {
    if (!els.reviewPanel) return;
    els.reviewPanel.hidden = false;
    refreshReview();
  }

  function closeReview() {
    if (els.reviewPanel) els.reviewPanel.hidden = true;
  }

  function renderToolEvent(container, kind, data) {
    if (!container || !data) return;
    if (kind === 'tool_result' && data.name === 'update_plan') {
      var livePlan = data.display && data.display.plan;
      if (Array.isArray(livePlan)) {
        renderPlan({ steps: livePlan, explanation: data.display.explanation });
      } else if (state.currentId) {
        loadConversationPlan(state.currentId);
      }
    }
    if (kind === 'tool_call') {
      toolCardFor(container, data, true);
      return;
    }
    var card = toolCardFor(container, data, true);
    var meta = toolCardTitle(card, data.name || card.getAttribute('data-tool-name') || '工具');
    card.classList.toggle('tool-card--thought', !!meta && meta.kind === 'thought');
    var ok = data.ok !== false;
    card.classList.remove('is-running');
    card.classList.add(ok ? 'is-ok' : 'is-error');
    var status = card.querySelector('.tool-card-status');
    if (status) status.textContent = ok ? '完成' : '失败';
    var summary = card.querySelector('.tool-card-summary');
    if (summary) summary.textContent = data.summary || (ok ? '已执行' : '执行失败');
    card.classList.add('has-result');
    var args = card.querySelector('.tool-card-args');
    if (data.summary && args) {
      args.textContent = String(data.summary);
    } else if (data.summary && !args) {
      var headRow = card.querySelector('.tool-card-head');
      var statusNode = card.querySelector('.tool-card-status');
      if (headRow) {
        args = document.createElement('span');
        args.className = 'tool-card-args';
        args.textContent = String(data.summary);
        if (statusNode) headRow.insertBefore(args, statusNode);
        else headRow.appendChild(args);
      }
    }
    // 思考事件有时只带一行 summary；仍给它保留一个可展开的原文入口，
    // 这样不会把长推理直接铺满时间线，也能与带 detail 的事件保持一致。
    var detail = typeof data.detail === 'string' ? data.detail : '';
    if (!detail && meta && meta.kind === 'thought' && data.summary) detail = String(data.summary);
    if (detail) {
      var box = card.querySelector('.tool-card-detail');
      if (!box) {
        box = document.createElement('details');
        box.className = 'tool-card-detail';
        var head = document.createElement('summary');
        head.textContent = meta && meta.kind === 'thought' ? '查看原文' : '查看结果';
        box.appendChild(head);
        var pre = document.createElement('pre');
        pre.className = 'tool-card-body';
        box.appendChild(pre);
        card.appendChild(box);
      }
      var body = card.querySelector('.tool-card-body');
      if (body) body.textContent = detail;
    }
    refreshActivitySummary(container);
  }

  // activity is the richer lifecycle stream. Legacy tool_call/tool_result
  // frames still render the card; activity only enriches that same card with
  // explicit phase/status/timing so clients never show duplicate rows.
  function renderActivityEvent(container, data) {
    if (!container || !data) return;
    var phase = data.phase || data.type || '';
    if (phase === 'tool_call') {
      renderToolEvent(container, 'tool_call', data);
    } else if (phase === 'tool_result') {
      renderToolEvent(container, 'tool_result', data);
    } else if (phase === 'error') {
      var id = data.id || ('activity-error-' + Date.now());
      renderToolEvent(container, 'tool_result', {
        id: id,
        name: data.name || '思考',
        ok: false,
        summary: data.summary || data.detail || '执行失败',
        detail: data.detail || data.summary || ''
      });
    } else {
      return;
    }
    var idValue = data.id ? String(data.id) : '';
    var card = idValue ? container.querySelector('[data-tool-id="' + idValue.replace(/"/g, '') + '"]') : null;
    if (!card) return;
    if (data.status) card.setAttribute('data-activity-status', data.status);
    if (data.started_at) card.setAttribute('data-started-at', data.started_at);
    if (data.finished_at) card.setAttribute('data-finished-at', data.finished_at);
    if (typeof data.duration_ms === 'number') card.setAttribute('data-duration-ms', String(data.duration_ms));
    if (data.status === 'waiting') card.classList.add('is-waiting');
    if (data.status === 'paused') card.classList.add('is-paused');
    refreshActivitySummary(container);
  }

  // 契约 §5：历史里的工具轨迹是单独一条 role="tool_trace" 的消息，
  // Content 是 [{"name","ok","summary","detail"}] 的 JSON 数组——**不能当正文渲染**。
  // 后端先存轨迹再存 assistant 正文，所以按顺序渲染出来的卡片天然排在答案上方，与实时流一致。
  function renderHistoryTraces(content, target) {
    var items = safeParse(content);
    if (!Array.isArray(items) || !items.length) return;
    var wrap = appendToolCards(null, target);
    items.forEach(function (item, i) {
      if (!item || typeof item !== 'object') return;
      var id = 'hist_' + i + '_' + (item.name || 'tool');
      renderToolEvent(wrap, 'tool_call', { id: id, name: item.name, arguments: null });
      renderToolEvent(wrap, 'tool_result', {
        id: id,
        name: item.name,
        ok: item.ok !== false,
        summary: item.summary,
        detail: item.detail
      });
    });
    refreshActivitySummary(wrap, true);
  }

  // ---- 子 agent：子任务卡片与状态轮询 ----
  function subagentId(run) {
    if (!run) return '';
    if (run.child_id != null) return String(run.child_id);
    if (run.id != null) return String(run.id);
    return '';
  }

  function subagentStatusMeta(status) {
    switch (status) {
      case 'done': return { text: '完成', cls: 'is-ok' };
      case 'failed': return { text: '失败', cls: 'is-error' };
      case 'aborted': return { text: '已中止', cls: 'is-aborted' };
      case 'running': return { text: '运行中', cls: 'is-running' };
      default: return { text: status || '运行中', cls: 'is-running' };
    }
  }

  function isTerminalSubagentStatus(status) {
    return status === 'done' || status === 'failed' || status === 'aborted';
  }

  function subagentReplyText(run) {
    var status = (run && run.status) || 'running';
    var parts = [];
    if (status === 'running' && run && run.progress) parts.push(String(run.progress));
    else if (run && run.reply) parts.push(String(run.reply));
    if (run && run.error && status === 'failed') parts.push('失败原因：' + run.error);
    if (!parts.length) {
      if (status === 'running') return '正在处理…';
      if (status === 'failed') return '执行失败';
      if (status === 'aborted') return '已中止';
      return '没有留下结论';
    }
    return parts.join('\n');
  }

  function subagentDurationText(run) {
    if (!run) return '';
    var ms = null;
    if (typeof run.duration_ms === 'number') {
      ms = run.duration_ms;
    } else if (run.started_at && run.finished_at) {
      var start = Date.parse(run.started_at);
      var end = Date.parse(run.finished_at);
      if (!isNaN(start) && !isNaN(end) && end >= start) ms = end - start;
    }
    if (ms == null) return '';
    return (ms / 1000).toFixed(1) + ' 秒';
  }

  function subagentMetaText(run) {
    var parts = [];
    if (run && run.depth != null && run.depth !== '') parts.push('深度 ' + run.depth);
    var duration = subagentDurationText(run);
    if (duration) parts.push(duration);
    return parts.join(' · ');
  }

  function updateSubagentCard(card, run) {
    if (!card || !run) return;
    var status = String(run.status || 'running');
    var meta = subagentStatusMeta(status);
    card.classList.remove('is-running', 'is-ok', 'is-error', 'is-aborted');
    card.classList.add(meta.cls);

    var label = card.querySelector('.subagent-card-label');
    if (label) label.textContent = run.label || '子任务';

    var statusEl = card.querySelector('.subagent-card-status');
    if (statusEl) statusEl.textContent = meta.text;

    var activityEl = card.querySelector('.subagent-card-activity');
    if (activityEl) {
      activityEl.textContent = run.activity || (status === 'running' ? '正在处理' : '');
      activityEl.hidden = !activityEl.textContent;
    }

    var reply = card.querySelector('.subagent-card-reply');
    if (reply) {
      var full = subagentReplyText(run);
      var shown = full.length > 400 ? full.slice(0, 400) + '…' : full;
      reply.textContent = shown;
      if (full.length > 400) reply.title = full;
      else reply.removeAttribute('title');
    }

    var metaEl = card.querySelector('.subagent-card-meta');
    if (metaEl) {
      var metaText = subagentMetaText(run);
      metaEl.textContent = metaText;
      metaEl.hidden = !metaText;
    }
  }

  function renderSubagentCard(run) {
    var card = document.createElement('div');
    card.className = 'subagent-card';
    var id = subagentId(run);
    if (id) card.setAttribute('data-subagent-id', id);
    card.setAttribute('role', 'group');
    card.setAttribute('aria-label', '子 agent 运行状态');

    var head = document.createElement('div');
    head.className = 'subagent-card-head';
    var label = document.createElement('span');
    label.className = 'subagent-card-label';
    var status = document.createElement('span');
    status.className = 'subagent-card-status';
    head.appendChild(label);
    head.appendChild(status);

    var reply = document.createElement('p');
    reply.className = 'subagent-card-reply';
    var activity = document.createElement('div');
    activity.className = 'subagent-card-activity';
    var meta = document.createElement('div');
    meta.className = 'subagent-card-meta';
    var open = document.createElement('button');
    open.type = 'button';
    open.className = 'subagent-card-open';
    open.textContent = '打开子会话';
    open.addEventListener('click', function () {
      if (id) openConversation(Number(id), true, run && run.parent_id, run && run.label);
      else toast('暂时无法打开这个子会话');
    });

    card.appendChild(head);
    card.appendChild(activity);
    card.appendChild(reply);
    card.appendChild(meta);
    card.appendChild(open);
    updateSubagentCard(card, run || {});
    return card;
  }

  function renderSubagentEvent(container, run) {
    if (!container || !run) return null;
    var id = subagentId(run);
    var cards = [];
    if (id) {
      var selector = '[data-subagent-id="' + id.replace(/["\\]/g, '') + '"]';
      cards = container.querySelectorAll(selector);
    }
    if (cards.length) {
      for (var i = 0; i < cards.length; i++) updateSubagentCard(cards[i], run);
      refreshActivitySummary(container);
      return cards[0];
    }
    var card = renderSubagentCard(run);
    var list = container.querySelector('.tool-cards-list') || container;
    container.hidden = false;
    container.open = true;
    list.appendChild(card);
    refreshActivitySummary(container);
    return card;
  }

  function indexSubagentRuns(runs) {
    var map = {};
    if (!Array.isArray(runs)) return map;
    runs.forEach(function (run) {
      var id = subagentId(run);
      if (id) map[id] = run;
    });
    return map;
  }

  function noteSubagentRun(run) {
    var id = subagentId(run);
    if (!id) return;
    subagentPoll.statuses[id] = String((run && run.status) || 'running');
  }

  function applySubagentRuns(runs) {
    if (!Array.isArray(runs)) return;
    runs.forEach(noteSubagentRun);
  }

  function hasRunningSubagents() {
    for (var id in subagentPoll.statuses) {
      if (Object.prototype.hasOwnProperty.call(subagentPoll.statuses, id) && subagentPoll.statuses[id] === 'running') {
        return true;
      }
    }
    return false;
  }

  function stopSubagentPolling(clearState) {
    if (subagentPoll.timer) {
      stopPoll(subagentPoll.timer);
      subagentPoll.timer = null;
    }
    if (clearState) {
      subagentPoll.convId = null;
      subagentPoll.statuses = {};
      subagentPoll.grace = 0;
    }
  }

  function pauseSubagentPolling() {
    if (subagentPoll.timer) {
      stopPoll(subagentPoll.timer);
      subagentPoll.timer = null;
    }
  }

  function stopChildRunPolling() {
    if (childRunPoll.timer) stopPoll(childRunPoll.timer);
    childRunPoll.timer = null;
    childRunPoll.convId = null;
    childRunPoll.parentId = null;
    childRunPoll.panel = null;
  }

  function renderChildRunProgress(run) {
    if (!run || state.currentId !== childRunPoll.convId) return;
    var panel = childRunPoll.panel;
    if (!panel || !panel.isConnected) {
      panel = document.createElement('section');
      panel.className = 'child-run-live';
      panel.setAttribute('aria-live', 'polite');
      var head = document.createElement('div');
      head.className = 'child-run-live-head';
      var title = document.createElement('strong');
      title.className = 'child-run-live-title';
      var badge = document.createElement('span');
      badge.className = 'child-run-live-status';
      head.appendChild(title);
      head.appendChild(badge);
      var activity = document.createElement('div');
      activity.className = 'child-run-live-activity';
      var progress = document.createElement('pre');
      progress.className = 'child-run-live-progress';
      panel.appendChild(head);
      panel.appendChild(activity);
      panel.appendChild(progress);
      els.thread.appendChild(panel);
      childRunPoll.panel = panel;
    }
    var status = subagentStatusMeta(run.status || 'running');
    panel.classList.remove('is-running', 'is-ok', 'is-error', 'is-aborted');
    panel.classList.add(status.cls);
    panel.querySelector('.child-run-live-title').textContent = run.label || '子 agent';
    panel.querySelector('.child-run-live-status').textContent = status.text;
    panel.querySelector('.child-run-live-activity').textContent = run.activity || '正在处理';
    panel.querySelector('.child-run-live-progress').textContent = run.progress || '等待子 agent 输出…';
    scrollToBottom(false);
  }

  async function pollCurrentChildRun() {
    if (document.hidden || state.view !== 'chat') return;
    var convId = childRunPoll.convId;
    var parentId = childRunPoll.parentId;
    if (convId == null || parentId == null || state.currentId !== convId) {
      stopChildRunPolling();
      return;
    }
    try {
      var body = await apiJSON('/api/conversations/' + encodeURIComponent(parentId) + '/subagents');
      if (state.currentId !== convId) return;
      var runs = Array.isArray(body) ? body : (body && Array.isArray(body.runs) ? body.runs : []);
      var current = null;
      runs.forEach(function (run) {
        if (String(subagentId(run)) === String(convId)) current = run;
      });
      if (!current) return;
      renderChildRunProgress(current);
      if (isTerminalSubagentStatus(String(current.status || ''))) {
        if (state.streaming) return;
        stopChildRunPolling();
        if (state.currentId === convId) await loadConversationMessages(convId);
      }
    } catch (e) {
      /* 下个轮询周期重试，不中断子会话页面。 */
    }
  }

  function startChildRunPolling(conv) {
    stopChildRunPolling();
    if (!conv || !conv.parent_id || conv.id == null) return;
    childRunPoll.convId = conv.id;
    childRunPoll.parentId = conv.parent_id;
    pollCurrentChildRun();
    childRunPoll.timer = startPoll(pollCurrentChildRun, 1800);
  }

  // 团队面板数据：GET /api/conversations/:id/team（契约见 docs/agent/07-team.md §1.3）。
  // 跟子任务轮询共用同一节拍，不另起定时器；拉不到（后端未上 / 网络错误）就当没有团队，整段隐藏。
  function clearTeamCard() {
	if (teamPoll.timer) stopPoll(teamPoll.timer);
	teamPoll.timer = null;
	teamPoll.convId = null;
    if (window.TASKCARD && typeof window.TASKCARD.setTeam === 'function') {
      window.TASKCARD.setTeam({ members: [], tasks: [] });
    }
  }

  async function refreshTeam(convId) {
    if (document.hidden || state.view !== 'chat') return;
    if (convId == null) return;
    if (!window.TASKCARD || typeof window.TASKCARD.setTeam !== 'function') return;
    if (teamInFlight[convId]) return;
    teamInFlight[convId] = true;
    var body = null;
    try {
      body = await apiJSON('/api/conversations/' + encodeURIComponent(convId) + '/team');
    } catch (e) {
      return; // 暂时失败时保留最近一次成员状态，避免卡片闪空。
    } finally {
      delete teamInFlight[convId];
    }
    if (state.currentId !== convId) return; // 已经切走：丢弃过期响应
    try {
      var teamData = body || { members: [], tasks: [] };
      teamData.parent_id = convId;
      window.TASKCARD.setTeam(teamData);
	  var active = Array.isArray(teamData.members) && teamData.members.some(function (member) {
	    return member && member.role !== 'lead' && member.status === 'running';
	  });
	  if (active && (!teamPoll.timer || teamPoll.convId !== convId)) {
	    if (teamPoll.timer) stopPoll(teamPoll.timer);
	    teamPoll.convId = convId;
	    teamPoll.timer = startPoll(function () { return refreshTeam(convId); }, 2500);
	  } else if (!active && teamPoll.timer) {
	    stopPoll(teamPoll.timer);
	    teamPoll.timer = null;
	    teamPoll.convId = null;
	  }
    } catch (e) {
      /* 渲染异常不影响轮询 */
    }
  }

  function startSubagentPollingIfNeeded() {
    if (state.currentId == null) {
      stopSubagentPolling(true);
      return;
    }
    // 没有 running 子任务时不会起定时器，但团队面板至少也要拉到一次数据，
    // 否则切会话 / 一轮跑完之后面板会一直空着。
    refreshTeam(state.currentId);
    if (!hasRunningSubagents()) {
      stopSubagentPolling(true);
      return;
    }
    if (subagentPoll.timer && subagentPoll.convId === state.currentId) return;
    pauseSubagentPolling();
    subagentPoll.convId = state.currentId;
    subagentPoll.timer = startPoll(function () {
      return pollSubagents();
    }, 4000);
  }

  async function pollSubagents() {
    if (document.hidden || state.view !== 'chat') return;
    if (state.streaming) return;
    var convId = state.currentId;
    if (convId == null || subagentPoll.convId !== convId) {
      stopSubagentPolling(true);
      return;
    }
    // 团队数据搭子任务轮询这班车（同一拍，不再新增定时器）。
    if (!teamPoll.timer) refreshTeam(convId);
    var body;
    try {
      body = await apiJSON('/api/conversations/' + encodeURIComponent(convId) + '/subagents');
    } catch (e) {
      // 后端还没上这个接口时别每 4 秒刷一次 404；其它错误保留定时器继续等。
      if (e && /HTTP 404/.test(e.message || '')) stopSubagentPolling(true);
      return;
    }
    var runs = Array.isArray(body) ? body : (body && Array.isArray(body.runs) ? body.runs : []);
    var changed = false;
    var seen = {};
    runs.forEach(function (run) {
      var id = subagentId(run);
      if (!id) return;
      seen[id] = true;
      var prev = subagentPoll.statuses[id];
      var next = String((run && run.status) || 'running');
      renderSubagentEvent(els.thread, run);
      if (prev === 'running' && isTerminalSubagentStatus(next)) changed = true;
      subagentPoll.statuses[id] = next;
    });
    Object.keys(subagentPoll.statuses).forEach(function (id) {
      if (!seen[id]) delete subagentPoll.statuses[id];
    });
    if (changed && state.currentId === convId) {
      // 刚有子任务结束：给 6 拍（约 24 秒）宽限，等父 agent 的汇总结果落库。
      subagentPoll.grace = 6;
      try {
        await loadConversationMessages(convId, runs);
      } catch (e) {
        /* 拉历史失败时保留现有卡片，下一轮再试。 */
      }
    }
    if (!hasRunningSubagents()) {
      if (subagentPoll.grace > 0) {
        subagentPoll.grace--;
        try {
          await loadConversationMessages(convId, runs);
        } catch (e) {
          /* 下一拍再试 */
        }
      } else {
        stopSubagentPolling(true);
      }
    }
  }

  function structuredSubagentFromMessage(message) {
    if (!message) return null;
    var candidates = [message.subagent, message.display, message.run, message.meta];
    for (var i = 0; i < candidates.length; i++) {
      var value = candidates[i];
      if (!value) continue;
      if (typeof value === 'string') value = safeParse(value);
      if (!value || typeof value !== 'object') continue;
      if (value.subagent && typeof value.subagent === 'object') return value.subagent;
      if (value.child_id != null || value.status || value.label) return value;
    }
    return null;
  }

  function childIdFromNotice(content) {
    var match = String(content || '').match(/id=(\d+)/);
    return match ? match[1] : '';
  }

  function subagentRunFromNotice(content, messageId) {
    var text = String(content || '');
    var parsed = safeParse(text);
    if (parsed && typeof parsed === 'object') {
      if (parsed.subagent && typeof parsed.subagent === 'object') return parsed.subagent;
      if (parsed.child_id != null || parsed.status) return parsed;
    }
    var status = 'done';
    if (/状态：\s*失败/.test(text)) status = 'failed';
    else if (/状态：\s*被中止/.test(text)) status = 'aborted';
    var labelMatch = text.match(/【子任务完成】\s*([^\n（(]+)/);
    var childId = childIdFromNotice(text);
    return {
      child_id: childId || ('notice-' + (messageId != null ? messageId : Date.now())),
      label: labelMatch ? labelMatch[1].trim() : '子任务',
      status: status,
      reply: text
    };
  }

  function renderHistorySubagent(message, runsById, target) {
    var run = structuredSubagentFromMessage(message);
    if (!run) {
      var childId = childIdFromNotice(message && message.content);
      if (childId && runsById && runsById[childId]) run = runsById[childId];
    }
    if (!run) run = subagentRunFromNotice(message && message.content, message && message.id);
    target.appendChild(renderSubagentCard(run));
    noteSubagentRun(run);
  }

  function renderConversationHistory(history, runs) {
    var renderStart = performance.now();
    els.thread.textContent = '';
    if (!Array.isArray(history) || !history.length) {
      setEmptyState(true);
      resetTaskCounters();
      notifyTaskCard();
      return;
    }
    setEmptyState(false);
    var runsById = indexSubagentRuns(runs);
    var fragment = document.createDocumentFragment();
    renderHistoryRows(history, runsById, fragment);
    els.thread.appendChild(fragment);
    updateOlderButton();
    scrollToBottom(true);
    resetTaskCounters();
    notifyTaskCard();
    if (window.APP_PERF) window.APP_PERF.record('render', 'history', performance.now() - renderStart);
  }

  function renderHistoryRows(history, runsById, target) {
    history.forEach(function (m) {
      if (!m || typeof m !== 'object') return;
      if (m.role === 'tool_trace') {
        renderHistoryTraces(m.content, target);
        return;
      }
      if (m.role === 'subagent') {
        renderHistorySubagent(m, runsById, target);
        return;
      }
      appendMessage(m.role === 'user' ? 'user' : 'assistant', m.content, { id: m.id, history: true, target: target });
    });
  }

  function updateOlderButton() {
    var button = els.thread.querySelector('.history-older');
    if (button) button.remove();
    if (!historyPage.hasMore || historyPage.id !== state.currentId) return;
    button = document.createElement('button');
    button.type = 'button';
    button.className = 'history-older btn btn-ghost';
    button.textContent = historyPage.busy ? '正在加载…' : '查看更早的消息';
    button.disabled = historyPage.busy || state.streaming;
    button.addEventListener('click', loadOlderMessages);
    els.thread.insertBefore(button, els.thread.firstChild);
  }

  async function loadOlderMessages() {
    if (historyPage.busy || !historyPage.hasMore || state.streaming) return;
    var page = historyPage;
    page.busy = true;
    updateOlderButton();
    try {
      var body = await apiJSON('/api/conversations/' + encodeURIComponent(page.id) + '/messages?limit=60&before=' + page.before);
      if (historyPage !== page || state.currentId !== page.id) return;
      var older = Array.isArray(body) ? body : body.messages || [];
      var anchor = els.thread.querySelector('.history-older').nextElementSibling;
      var anchorTop = anchor && anchor.getBoundingClientRect().top;
      var fragment = document.createDocumentFragment();
      var cached = historyCache.get(page.id);
      renderHistoryRows(older, indexSubagentRuns(cached && cached.runs), fragment);
      els.thread.insertBefore(fragment, els.thread.firstChild);
      page.before = older.length ? older[0].id : page.before;
      page.hasMore = !Array.isArray(body) && !!body.has_more;
      if (cached) {
        cached.history = older.concat(cached.history);
        cached.before = page.before;
        cached.hasMore = page.hasMore;
      }
      updateOlderButton();
      if (anchor) els.messages.scrollTop += anchor.getBoundingClientRect().top - anchorTop;
    } catch (e) {
      toast('加载更早消息失败：' + e.message);
    } finally {
      page.busy = false;
      if (historyPage === page) updateOlderButton();
    }
  }

  function sameHistory(a, b) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
    for (var i = 0; i < a.length; i++) {
      if (a[i].id !== b[i].id || a[i].role !== b[i].role || a[i].content !== b[i].content) return false;
    }
    return true;
  }

  function rememberHistory(id, history, runs) {
    historyCache.delete(id);
    historyCache.set(id, { history: history, runs: runs, before: historyPage.before, hasMore: historyPage.hasMore });
    if (historyCache.size > HISTORY_CACHE_LIMIT) {
      var oldest = historyCache.keys().next().value;
      historyCache.delete(oldest);
      historyDOMCache.delete(oldest);
    }
  }

  async function loadConversationMessages(id, knownRuns) {
    var sequence = ++historyLoadSequence;
    var previous = historyCache.get(id);
    // 校对已经加载过的范围，保留用户向上翻出的消息；首次只拿最近 60 条。
    var limit = previous ? Math.min(200, Math.max(60, previous.history.length)) : 60;
    var body = await apiJSON('/api/conversations/' + encodeURIComponent(id) + '/messages?limit=' + limit);
    var history = Array.isArray(body) ? body : body.messages || [];
    if (sequence !== historyLoadSequence || state.currentId !== id) return history;
    var hasMore = !Array.isArray(body) && !!body.has_more;
    if (previous && previous.history.length > limit && history.length) {
      history = previous.history.filter(function (m) { return Number(m.id) < Number(history[0].id); }).concat(history);
      hasMore = previous.hasMore;
    }
    historyPage = { id: id, before: history.length ? history[0].id : 0, hasMore: hasMore, busy: false };
    var runs = knownRuns;
    if (runs == null && Array.isArray(history) && history.some(function (m) {
      return m.role === 'subagent';
    })) {
      try {
        var body = await apiJSON('/api/conversations/' + encodeURIComponent(id) + '/subagents');
        runs = Array.isArray(body) ? body : (body && Array.isArray(body.runs) ? body.runs : []);
      } catch (e) {
        runs = null;
      }
    }
    if (sequence !== historyLoadSequence || state.currentId !== id) return history;
    rememberHistory(id, history, runs);
    if (state.currentId !== id) return history;
    stopSubagentPolling(true);
    subagentPoll.convId = id;
    if (Array.isArray(runs)) applySubagentRuns(runs);
    if (!previous || !sameHistory(previous.history, history)) {
      renderConversationHistory(history, runs);
    } else if (Array.isArray(runs)) {
      runs.forEach(function (run) { renderSubagentEvent(els.thread, run); });
    }
    updateOlderButton();
    startSubagentPollingIfNeeded();
    return history;
  }


  // signal 是可选中止句柄（AbortSignal）；传入后 fetch / 流读取可被 abort() 中断。
  async function streamChat(path, payload, onDelta, onDone, onError, onTool, signal) {
    var options = {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'text/event-stream'
      },
      body: JSON.stringify(payload)
    };
    // 把停止句柄挂到 fetch 上：中止时 reader.read() 会抛 AbortError，交 runAssistantStream 识别。
    if (signal) options.signal = signal;
    var res = await api(path, options);

    if (!res.ok) {
      var message = 'HTTP ' + res.status;
      try {
        var type = res.headers.get('content-type') || '';
        if (type.indexOf('application/json') !== -1) {
          var body = await res.json();
          if (body && body.error) message = body.error;
        } else {
          var text = await res.text();
          if (text) message = text;
        }
      } catch (e) {}
      throw new Error(message);
    }

    await consumeSSE(res, {
      onDelta: onDelta,
      onDone: onDone,
      onError: onError,
      onTool: onTool
    });
  }

  // 消费逻辑与具体接口解耦：/api/chat 和 /api/conversations/:id/regenerate 复用同一套 SSE 帧解析。
  async function consumeSSE(res, handlers) {
    handlers = handlers || {};
    var onDelta = handlers.onDelta || function () {};
    var onDone = handlers.onDone || function () {};
    var onError = handlers.onError || function () {};
    var onTool = handlers.onTool || function () {};

    if (!res.body || !res.body.getReader) {
      throw new Error('当前浏览器不支持流式读取');
    }

    var reader = res.body.getReader();
    var decoder = new TextDecoder('utf-8');
    var buffer = '';
    var finished = false;

    function handleBlock(block) {
      if (finished || !block) return;
      var event = 'message';
      var dataLines = [];
      var lines = block.split('\n');
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i];
        if (line.charAt(0) === ':') continue;
        var colon = line.indexOf(':');
        var field, value;
        if (colon === -1) {
          field = line;
          value = '';
        } else {
          field = line.slice(0, colon);
          value = line.slice(colon + 1);
          if (value.charAt(0) === ' ') value = value.slice(1);
        }
        if (field === 'event') {
          event = value;
        } else if (field === 'data') {
          dataLines.push(value);
        }
      }
      var data = dataLines.join('\n');
      if (event === 'delta') {
        var delta = safeParse(data);
        if (delta && typeof delta.content === 'string') onDelta(delta.content);
      } else if (event === 'tool_call' || event === 'tool_result') {
        var tool = safeParse(data);
        if (tool) onTool(event, tool);
      } else if (event === 'activity') {
        var activity = safeParse(data);
        if (activity) onTool('activity', activity);
      } else if (event === 'need_input') {
        var ask = safeParse(data);
        if (ask) onTool('need_input', ask);
      } else if (event === 'memory') {
        var mem = safeParse(data);
        if (mem) onTool('memory', mem);
      } else if (event === 'skills') {
        var pickedSkills = safeParse(data);
        if (pickedSkills && window.SKILLS && window.SKILLS.onAutoPicked) window.SKILLS.onAutoPicked(pickedSkills);
      } else if (event === 'subagent') {
        var subagent = safeParse(data);
        if (subagent) onTool('subagent', subagent);
      } else if (event === 'usage') {
        showUsage(safeParse(data));
      } else if (event === 'done') {
        finished = true;
        onDone(safeParse(data) || {});
      } else if (event === 'error') {
        var err = safeParse(data);
        finished = true;
        onError(err && err.error ? err.error : '服务端返回错误');
      }
    }

    function drain() {
      var pending = '';
      if (buffer.charAt(buffer.length - 1) === '\r') {
        pending = '\r';
        buffer = buffer.slice(0, -1);
      }
      buffer = buffer.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
      var idx;
      while ((idx = buffer.indexOf('\n\n')) !== -1) {
        handleBlock(buffer.slice(0, idx));
        buffer = buffer.slice(idx + 2);
      }
      buffer += pending;
    }

    for (;;) {
      var chunk = await reader.read();
      if (chunk.done) break;
      buffer += decoder.decode(chunk.value, { stream: true });
      drain();
    }
    buffer += decoder.decode();
    drain();
    if (!finished && buffer.replace(/\s/g, '') !== '') {
      handleBlock(buffer.replace(/\r\n/g, '\n').replace(/\r/g, '\n'));
    }
  }

  async function syncMessageIds() {
    if (state.currentId == null) return;
    var convId = state.currentId;
    var history;
    try {
      var page = await apiJSON('/api/conversations/' + encodeURIComponent(convId) + '/messages?limit=60');
      history = Array.isArray(page) ? page : page.messages;
    } catch (e) {
      return;
    }
    if (!Array.isArray(history) || state.currentId !== convId) return;
    // 子任务通知渲染成卡片而不是 .msg，ID 对齐时要跳过它们，否则后续消息会整体错位。
    var visible = history.filter(function (m) {
      return m.role !== 'tool_trace' && m.role !== 'subagent';
    });
    var nodes = els.thread.querySelectorAll('.msg');
    var used = {};
    for (var i = 0; i < nodes.length; i++) {
      var known = nodes[i].getAttribute('data-message-id');
      if (known) used[known] = true;
    }
    // 分页后不能按数组下标对齐；子任务可能在本轮结束后又追加消息。
    // 只给尚未落库的节点匹配角色和原文，保留旧页已有的 ID。
    for (var n = nodes.length - 1; n >= 0; n--) {
      var node = nodes[n];
      if (node.getAttribute('data-message-id')) continue;
      for (var j = visible.length - 1; j >= 0; j--) {
        var row = visible[j];
        if (used[row.id] || row.role !== node.getAttribute('data-role') ||
            row.content !== node.getAttribute('data-raw-content')) continue;
        node.setAttribute('data-message-id', String(row.id));
        used[row.id] = true;
        break;
      }
    }
  }

  async function messageIdFor(msg) {
    var id = msg.getAttribute('data-message-id');
    if (id) return id;
    await syncMessageIds();
    return msg.getAttribute('data-message-id') || '';
  }

  function removeFollowing(msg) {
    while (msg.nextSibling) msg.parentNode.removeChild(msg.nextSibling);
  }

  async function deleteMessage(msg) {
    if (state.streaming) return;
    var id = await messageIdFor(msg);
    if (!id) {
      toast('这条消息还没有保存，稍后再试');
      return;
    }
    var sureMsg = window.UIASK
      ? await window.UIASK.confirm({ title: '删除这条消息？', message: '删掉之后它不会再出现在对话里。', okText: '删除', danger: true })
      : window.confirm('删除这条消息？');
    if (!sureMsg) return;
    try {
      await apiJSON('/api/messages/' + encodeURIComponent(id), { method: 'DELETE' });
    } catch (e) {
      toast('删除失败：' + (e.message || '未知错误'));
      return;
    }
    msg.remove();
    historyCache.delete(state.currentId);
    if (!els.thread.querySelector('.msg')) setEmptyState(true);
  }

  async function editMessage(msg, bubble) {
    if (state.streaming) return;
    var id = await messageIdFor(msg);
    if (!id) {
      toast('这条消息还没有保存，稍后再试');
      return;
    }
    var raw = msg.getAttribute('data-raw-content') || bubble.textContent || '';
    var actions = msg.querySelector('.msg-actions');
    if (actions) actions.hidden = true;
    bubble.classList.add('is-editing');
    bubble.textContent = '';

    var editor = document.createElement('div');
    editor.className = 'msg-editor';
    var area = document.createElement('textarea');
    area.className = 'input msg-editor-input';
    area.value = raw;
    area.rows = 3;
    area.setAttribute('aria-label', '编辑消息');
    var note = document.createElement('p');
    note.className = 'msg-editor-note';
    note.textContent = '保存会删除此消息之后的内容，并自动重新生成回复。';
    var row = document.createElement('div');
    row.className = 'msg-editor-actions';
    var save = document.createElement('button');
    save.type = 'button';
    save.className = 'btn';
    save.textContent = '保存';
    save.title = '保存修改并重新生成';
    save.setAttribute('aria-label', '保存修改并重新生成');
    var cancel = document.createElement('button');
    cancel.type = 'button';
    cancel.className = 'btn btn-ghost';
    cancel.textContent = '取消';
    cancel.title = '取消编辑';
    cancel.setAttribute('aria-label', '取消编辑');

    function restore() {
      bubble.textContent = raw;
      bubble.classList.remove('is-editing');
      if (actions) actions.hidden = false;
    }
    cancel.addEventListener('click', restore);
    save.addEventListener('click', async function () {
      var content = area.value.trim();
      if (!content) {
        toast('消息内容不能为空');
        return;
      }
      save.disabled = true;
      cancel.disabled = true;
      try {
        await apiJSON('/api/messages/' + encodeURIComponent(id), {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: content })
        });
      } catch (e) {
        save.disabled = false;
        cancel.disabled = false;
        toast('保存失败：' + (e.message || '未知错误'));
        return;
      }
      msg.setAttribute('data-raw-content', content);
      historyCache.delete(state.currentId);
      raw = content;
      bubble.textContent = content;
      bubble.classList.remove('is-editing');
      if (actions) actions.hidden = false;
      removeFollowing(msg);
      await regenerateAfterUser(msg);
    });

    row.appendChild(save);
    row.appendChild(cancel);
    editor.appendChild(area);
    editor.appendChild(note);
    editor.appendChild(row);
    bubble.appendChild(editor);
    area.focus();
    area.setSelectionRange(area.value.length, area.value.length);
  }

  async function regenerateAfterUser(userMsg) {
    if (state.currentId == null || state.streaming) return;
    if (!state.model) {
      toast('没有可用模型，请先在设置中配置 Key');
      return;
    }
    pauseSubagentPolling();
    historyCache.delete(state.currentId);
    setStreaming(true);
    stickBottom = true;
    var toolCards = appendToolCards(null);
    var bubble = appendMessage('assistant', '');
    scrollToBottom(true);
    try {
      var regenPayload = {
        model: state.model,
        assistant_id: currentAssistantId(),
        execution_mode: els.modeSelect ? els.modeSelect.value : ''
      };
      applyChatParamsToPayload(regenPayload, state.currentId);
      await runAssistantStream(bubble, toolCards, '/api/conversations/' + encodeURIComponent(state.currentId) + '/regenerate', regenPayload);
    } finally {
      setStreaming(false);
      await syncMessageIds();
      startSubagentPollingIfNeeded();
      scrollToBottom(true);
      if (userMsg) userMsg.classList.remove('is-regenerating');
    }
  }

  async function regenerateMessage(msg) {
    if (state.streaming) return;
    if (state.currentId == null || !state.model) {
      toast('没有可用模型，或当前没有会话');
      return;
    }
    pauseSubagentPolling();
    historyCache.delete(state.currentId);
    var bubble = msg.querySelector('.bubble');
    if (!bubble) return;
    var previous = msg.previousElementSibling;
    while (previous && previous.classList && previous.classList.contains('tool-cards')) {
      var oldTools = previous;
      previous = previous.previousElementSibling;
      oldTools.remove();
    }
    setStreaming(true);
    stickBottom = true;
    var toolCards = appendToolCards(msg);
    scrollToBottom(true);
    try {
      var regenPayload = {
        model: state.model,
        assistant_id: currentAssistantId(),
        execution_mode: els.modeSelect ? els.modeSelect.value : ''
      };
      applyChatParamsToPayload(regenPayload, state.currentId);
      await runAssistantStream(bubble, toolCards, '/api/conversations/' + encodeURIComponent(state.currentId) + '/regenerate', regenPayload);
    } finally {
      setStreaming(false);
      await syncMessageIds();
      startSubagentPollingIfNeeded();
      scrollToBottom(true);
    }
  }

  async function runAssistantStream(bubble, toolCards, path, payload) {
    bubble.textContent = '';
    bubble.classList.remove('is-error');
    var mdBox = document.createElement('div');
    mdBox.className = 'md';
    var mdText = '';
    var gotAny = false;
    var settled = false;
    var paintFrame = 0;
    var paintDirty = false;
    var controller = null;
    if (typeof AbortController !== 'undefined') controller = new AbortController();
    state.abortController = controller;

    function paintMd() {
      if (!paintDirty) return;
      paintDirty = false;
      if (bubble.parentNode) bubble.parentNode.setAttribute('data-raw-content', mdText);
      if (window.MD && window.MD.render) {
        mdBox.innerHTML = window.MD.render(mdText);
      } else {
        mdBox.textContent = mdText;
      }
    }
    function appendErrorActions(message) {
      if (!bubble || bubble.querySelector('.error-actions')) return;
      var row = document.createElement('div');
      row.className = 'error-actions';
      var retry = document.createElement('button');
      retry.type = 'button';
      retry.className = 'btn btn-ghost';
      retry.textContent = '重试';
      retry.setAttribute('aria-label', '重试这次请求');
      retry.addEventListener('click', function () {
        var msg = bubble.closest ? bubble.closest('.msg') : null;
        if (msg) regenerateMessage(msg);
      });
      var copy = document.createElement('button');
      copy.type = 'button';
      copy.className = 'btn btn-ghost';
      copy.textContent = '复制错误';
      copy.setAttribute('aria-label', '复制错误信息');
      copy.addEventListener('click', function () {
        copyText(String(message || '请求失败')).then(function () { toast('错误信息已复制'); });
      });
      row.appendChild(retry);
      row.appendChild(copy);
      bubble.appendChild(row);
    }
    function flushPaint() {
      if (paintFrame) {
        cancelAnimationFrame(paintFrame);
        paintFrame = 0;
      }
      paintMd();
    }
    function schedulePaint() {
      paintDirty = true;
      if (paintFrame) return;
      paintFrame = requestAnimationFrame(function () {
        paintFrame = 0;
        paintMd();
        scrollToBottom(false);
      });
    }
    bubble.appendChild(mdBox);
    var cursor = document.createElement('span');
    cursor.className = 'cursor';
    bubble.appendChild(cursor);

    function finishError(message) {
      if (settled) return;
      settled = true;
      refreshActivitySummary(toolCards, true);
      flushPaint();
      cursor.remove();
      state.abortController = null;
      if (gotAny) {
        var note = document.createElement('span');
        note.style.color = 'var(--danger)';
        note.textContent = '\n[出错] ' + message;
        bubble.appendChild(note);
      } else {
        bubble.classList.add('is-error');
        bubble.textContent = message;
      }
      appendErrorActions(message);
      setStreaming(false);
      scrollToBottom(true);
    }

    function finishDone(result) {
      if (settled) return;
      settled = true;
      if (toolCards) {
        if (result && result.incomplete) toolCards.setAttribute('data-incomplete', 'true');
        else toolCards.removeAttribute('data-incomplete');
      }
      refreshActivitySummary(toolCards, true);
      if (!gotAny) { mdText = '（模型未返回内容）'; paintDirty = true; }
      flushPaint();
      cursor.remove();
      state.abortController = null;
      setStreaming(false);
      scrollToBottom(true);
    }

    // 用户主动停止：保留已生成的内容，不算错误，不弹红色报错。
    function finishAbort() {
      if (settled) return;
      settled = true;
      refreshActivitySummary(toolCards, true);
      if (!gotAny) { mdText = '（已停止）'; paintDirty = true; }
      flushPaint();
      cursor.remove();
      state.abortController = null;
      setStreaming(false);
      scrollToBottom(true);
      toast('已停止生成');
    }

    try {
      await streamChat(path, payload,
        function (piece) {
          gotAny = true;
          mdText += piece;
          schedulePaint();
        },
        finishDone,
        finishError,
        function (kind, data) {
          if (kind === 'activity') {
            renderActivityEvent(toolCards, data);
            return;
          }
          if (kind === 'memory') {
            renderMemoryActivity(toolCards, data);
            return;
          }
          if (kind === 'need_input') {
            gotAny = true;
            var nd = data.display || {};
            mdText += (nd.question || data.question || '') + '\n\n';
            paintDirty = true;
            flushPaint();
            scrollToBottom(true);
            if (data.confirm) {
              renderConfirmButtons(bubble, mdBox);
            } else if (nd.options && nd.options.length) {
              renderAskOptions(bubble, mdBox, nd.options, nd.question || data.question || '');
            }
            if (els.input) els.input.focus();
            return;
          }
          if (kind === 'subagent') {
            renderSubagentEvent(toolCards, data);
            noteSubagentRun(data);
            return;
          }
          if (kind === 'tool_result' && data && data.display && data.display.team_member) {
            var member = data.display.team_member;
            if (member.child_id != null) {
              var childRun = {
                child_id: member.child_id,
                parent_id: state.currentId,
                label: member.description || member.name || '团队成员',
                status: 'running',
                activity: '已派发，正在启动'
              };
              renderSubagentEvent(toolCards, childRun);
              noteSubagentRun(childRun);
            }
          }
          // 工具进度是卡片上唯一能给出真实百分比的数据源，就在这个统一出口处记账。
          if (kind === 'tool_call') taskState.toolTotal++;
          if (kind === 'tool_result' && data && data.ok !== false) taskState.toolDone++;
          renderToolEvent(toolCards, kind, data);
          notifyTaskCard();
        },
        controller ? controller.signal : null
      );
    } catch (e) {
      if (isAbortError(e)) { finishAbort(); return; }
      finishError(e.message || '网络错误');
      return;
    }
    if (!settled) finishDone();
  }

  // sendAfterIdle：等本轮流式结束再把一句话发出去。
  // 确认/取消按钮是在 need_input 事件里就画出来的，而那一刻流还没收完；
  // 直接调 sendMessage 会因为 state.streaming 还是 true 而被静默丢弃。
  function sendAfterIdle(text) {
    if (state.streaming) {
      setTimeout(function () { sendAfterIdle(text); }, 120);
      return;
    }
    els.input.value = text;
    sendMessage();
  }

  function renderConfirmButtons(bubble, mdBox) {
    var row = document.createElement('div');
    row.className = 'confirm-row';

    var ok = document.createElement('button');
    ok.type = 'button';
    ok.className = 'btn confirm-ok';
    ok.textContent = '确认执行';
    ok.title = '确认执行这个操作';
    ok.setAttribute('aria-label', '确认执行这个操作');
    ok.addEventListener('click', function () {
      row.remove();
      sendAfterIdle('确认');
    });

    var no = document.createElement('button');
    no.type = 'button';
    no.className = 'btn btn-ghost confirm-no';
    no.textContent = '取消';
    no.title = '取消这个操作';
    no.setAttribute('aria-label', '取消这个操作');
    no.addEventListener('click', function () {
      row.remove();
      sendAfterIdle('取消');
    });

    row.appendChild(ok);
    row.appendChild(no);
    bubble.insertBefore(row, mdBox);
  }

  function renderAskOptions(bubble, mdBox, options, question) {
    // 选项式提问：ask_user 的候选项画成按钮，点一下就把该文本当回答发出去。
    // 末尾固定加一个「其他…」走页内对话框自由输入（Electron 里 window.prompt 不可用，
    // 只能用 UIASK）。用户不点按钮、直接在输入框打字发送也仍然有效——按钮只是快捷方式。
    var row = document.createElement('div');
    row.className = 'confirm-row ask-options';
    options.forEach(function (opt) {
      var label = typeof opt === 'string' ? opt : (opt && opt.label) || '';
      if (!label) return;
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'btn btn-outline ask-option';
      btn.textContent = label;
      btn.title = (opt && opt.description) || label;
      btn.setAttribute('aria-label', (opt && opt.description) || label);
      btn.addEventListener('click', function () {
        row.remove();
        sendAfterIdle(label);
      });
      row.appendChild(btn);
    });

    var other = document.createElement('button');
    other.type = 'button';
    other.className = 'btn btn-ghost ask-option-other';
    other.textContent = '其他…';
    other.title = '自己输入回答';
    other.setAttribute('aria-label', '其他回答，自己输入');
    other.addEventListener('click', function () {
      if (!window.UIASK || typeof window.UIASK.prompt !== 'function') {
        // 没有页内对话框的环境：退回输入框手打，选项按钮留着，不堵死回答的路。
        if (els.input) els.input.focus();
        return;
      }
      window.UIASK.prompt({
        title: '其他回答',
        message: question || '',
        label: '你的回答',
        placeholder: '直接输入你的答案',
        okText: '发送'
      }).then(function (value) {
        if (value === null) return; // 取消：按钮留着，用户还能点选项
        var text = String(value).trim();
        if (!text) return;
        row.remove();
        sendAfterIdle(text);
      });
    });
    row.appendChild(other);

    if (row.childNodes.length) bubble.insertBefore(row, mdBox);
  }

  async function sendMessage() {
    if (state.streaming) return;
    var text = els.input.value.trim();
    if (!text) return;
    if (!state.model) {
      toast('没有可用模型，请先在设置中配置 Key');
      return;
    }
    pauseSubagentPolling();
    if (state.currentId != null) historyCache.delete(state.currentId);

    setStreaming(true);
    els.input.value = '';
    autoGrow();
    stickBottom = true;

    setEmptyState(false);
    resetTaskCounters();
    notifyTaskCard();
    appendMessage('user', text);
    var toolCards = appendToolCards(null);
    var bubble = appendMessage('assistant', '');
    scrollToBottom(true);

    var conversationId = state.currentId;
    var createdNow = false;
    var refreshTitle = false;
    if (conversationId == null) {
      try {
        var conv = await createConversation();
        conversationId = conv.id;
        state.currentId = conv.id;
        clearPlan(); // 新会话从空计划开始
        conversationMetaFor(conversationId).openedAt = Date.now();
        conversationMetaFor(conversationId).unread = false;
        saveConversationMeta();
        createdNow = true;
        refreshTitle = true;
        els.topbarTitle.textContent = conv.title || '新对话';
        renderConvList();
        // 新会话第一次发送：把草稿参数迁到刚建立的会话 id 下，面板随之显示。
        migrateDraftParams(conversationId);
        refreshParamsPanel();
      } catch (e) {
        bubble.classList.add('is-error');
        bubble.textContent = '创建会话失败：' + e.message;
        setStreaming(false);
        return;
      }
    } else {
      var currentConversation = state.conversations.filter(function (item) {
        return Number(item.id) === Number(conversationId);
      })[0];
      refreshTitle = !currentConversation || !currentConversation.title || currentConversation.title === '新对话';
    }

    if (createdNow) setConversationTitle(conversationId, titleFromQuestion(text));

    try {
      // 契约 §5：tools=true 让后端走 agent 循环；后端未接时该字段被忽略，行为与从前一致。
      var chatPayload = {
        conversation_id: conversationId,
        model: state.model,
        content: text,
        tools: true,
        // 会话模式：决定后端给哪些工具、用哪段提示词、记忆算哪一类。
        kind: (els.kindSelect && els.kindSelect.value) ? els.kindSelect.value : 'work',
        assistant_id: currentAssistantId(),
        execution_mode: els.modeSelect ? els.modeSelect.value : ''
      };
      // 对话参数：只带用户设过的字段，没设就不传，留给后端默认。
      applyChatParamsToPayload(chatPayload, conversationId);
      await runAssistantStream(bubble, toolCards, '/api/chat', chatPayload);
    } finally {
      setStreaming(false);
      if (refreshTitle) await refreshConversationTitle(conversationId);
      await syncMessageIds();
      startSubagentPollingIfNeeded();
      scrollToBottom(true);
    }
  }

  function openSidebar() {
    els.sidebar.classList.add('open');
    els.sidebarOverlay.classList.add('show');
    els.sidebarToggle.setAttribute('aria-expanded', 'true');
  }

  function closeSidebar() {
    els.sidebar.classList.remove('open');
    els.sidebarOverlay.classList.remove('show');
    els.sidebarToggle.setAttribute('aria-expanded', 'false');
  }

  els.messages.addEventListener('scroll', function () {
    var el = els.messages;
    stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
  });

  els.sidebarToggle.addEventListener('click', openSidebar);

  // 桌面端侧栏收拢/展开：宽度由 CSS 过渡，这里只管状态与持久化。
  var sidebarCollapse = document.getElementById('sidebar-collapse');
  function applySidebarCollapsed(collapsed) {
    document.documentElement.setAttribute('data-sidebar', collapsed ? 'collapsed' : 'expanded');
    if (sidebarCollapse) {
      sidebarCollapse.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      sidebarCollapse.setAttribute('aria-label', collapsed ? '展开侧栏' : '收拢侧栏');
      sidebarCollapse.setAttribute('title', collapsed ? '展开侧栏' : '收拢侧栏');
    }
    try {
      window.localStorage.setItem('blogmy:sidebar', collapsed ? 'collapsed' : 'expanded');
    } catch (e) {
      // 隐私模式忽略。
    }
  }
  if (sidebarCollapse) {
    applySidebarCollapsed(document.documentElement.getAttribute('data-sidebar') === 'collapsed');
    sidebarCollapse.addEventListener('click', function () {
      applySidebarCollapsed(document.documentElement.getAttribute('data-sidebar') !== 'collapsed');
    });
  }
  // ---------- 工作目录（左下角入口） ----------

  var activeWorkspaces = [];
  var savedWorkspaces = [];
  var wsDialog = document.getElementById('workspace-dialog');
  var wsList = document.getElementById('workspace-list');

  function uniqueWorkspacePaths(paths) {
    var seen = Object.create(null);
    return (Array.isArray(paths) ? paths : []).map(function (p) { return String(p || '').trim(); }).filter(function (p) {
      var key = p.replace(/[\\/]+$/, '').toLowerCase();
      if (!p || seen[key]) return false;
      seen[key] = true;
      return true;
    });
  }

  function renderWorkspaces() {
    if (!wsList) return;
    wsList.textContent = '';
    if (!activeWorkspaces.length) {
      var empty = document.createElement('p');
      empty.className = 'list-hint';
      empty.textContent = '还没有启用工作目录。';
      wsList.appendChild(empty);
      return;
    }
    activeWorkspaces.forEach(function (path, index) {
      var row = document.createElement('div');
      row.className = 'workspace-row';
      var number = document.createElement('span');
      number.className = 'workspace-row-primary';
      number.textContent = index === 0 ? '主目录' : ('目录 ' + (index + 1));
      var value = document.createElement('span');
      value.className = 'workspace-row-path';
      value.textContent = path;
      value.title = path;
      var primary = document.createElement('button');
      primary.type = 'button';
      primary.className = 'btn btn-outline';
      primary.textContent = index === 0 ? '默认' : '设为主目录';
      primary.disabled = index === 0;
      primary.addEventListener('click', function () {
        activeWorkspaces.splice(index, 1);
        activeWorkspaces.unshift(path);
        renderWorkspaces();
      });
      var remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'btn btn-danger';
      remove.textContent = '移除';
      remove.addEventListener('click', function () {
        activeWorkspaces.splice(index, 1);
        renderWorkspaces();
      });
      row.append(number, value, primary, remove);
      wsList.appendChild(row);
    });
  }

  function workspaceNote(message, kind) {
    var node = document.getElementById('workspace-note');
    if (!node) return;
    node.textContent = message || '';
    node.className = kind === 'error' ? 'kb-note kb-note-error' : (kind === 'ok' ? 'kb-note kb-note-ok' : 'kb-note');
  }

  async function loadWorkspace() {
    if (!els.wsCurrent) return;
    try {
      var st = await apiJSON('/api/agent/settings');
      var paths = uniqueWorkspacePaths(st.workspaces && st.workspaces.length ? st.workspaces : (st.workspace ? [st.workspace] : []));
      activeWorkspaces = paths;
      savedWorkspaces = paths.slice();
      var p = paths[0] || '';
      var summary = paths.length ? (p + (paths.length > 1 ? (' 等 ' + paths.length + ' 个目录') : '')) : '未设置';
      els.wsCurrent.textContent = '活动目录：' + summary;
      els.wsCurrent.title = paths.join('\n');
      if (els.wsPick) els.wsPick.title = '管理 AI 可访问的 ' + paths.length + ' 个工作目录\n' + paths.join('\n');
      // 胶囊显示主目录末级名及启用数量。
      var wsLabel = document.querySelector('#ws-pick .composer-ws-label');
      if (wsLabel) {
        var wsParts = (p || '').split(/[\\/]/).filter(Boolean);
        wsLabel.textContent = (wsParts.length ? wsParts[wsParts.length - 1] : '工作区') + (paths.length > 1 ? (' +' + (paths.length - 1)) : '');
      }
      taskState.workspace = p;
      notifyTaskCard();
      if (els.modeSelect && st.mode) els.modeSelect.value = st.mode;
    } catch (e) {
      els.wsCurrent.textContent = '工作目录：读取失败';
    }
  }

  async function saveWorkspaces() {
    var body = await apiJSON('/api/agent/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workspaces: activeWorkspaces })
    });
    savedWorkspaces = activeWorkspaces.slice();
    if (wsDialog && wsDialog.open) wsDialog.close();
    toast('已保存 ' + (body.workspaces || []).length + ' 个活动工作目录');
    await loadWorkspace();
  }

  async function addWorkspace() {
    if (activeWorkspaces.length >= 20) {
      workspaceNote('最多同时启用 20 个目录。', 'error');
      return;
    }
    try {
      var picked = null;
      if (window.desktop && typeof window.desktop.pickDirectory === 'function') {
        // 桌面端：系统原生目录选择框，避免让用户手打 Windows 路径。
        picked = await window.desktop.pickDirectory();
        if (!picked) return; // 用户取消
      } else {
        // 网页端 / 内嵌浏览器没有原生选择框，用页内对话框手填绝对路径。
        // 不能用 window.prompt：Electron 从来没实现过它（调用只会在控制台报
        // "prompt() is and will not be supported" 并返回 null），点下去会像卡住一样。
        if (!window.UIASK) {
          toast('这个环境不支持目录选择，请到设置页里填工作目录');
          return;
        }
        picked = await window.UIASK.prompt({
          title: '添加活动工作目录',
          message: '可以添加多个目录；AI 只会在已启用的目录中使用相对路径操作文件。',
          label: '绝对路径',
          value: '',
          placeholder: '例如 D:\\my-workspace',
          note: '目录不存在会自动创建。',
          okText: '添加目录'
        });
        if (picked === null) return; // 取消
        picked = picked.trim();
        if (!picked) return;
      }
      picked = picked.trim();
      if (activeWorkspaces.some(function (p) { return p.replace(/[\\/]+$/, '').toLowerCase() === picked.replace(/[\\/]+$/, '').toLowerCase(); })) {
        workspaceNote('这个目录已经在列表里了。');
        return;
      }
      activeWorkspaces.push(picked);
      renderWorkspaces();
      workspaceNote('目录已添加到待保存列表。');
    } catch (e) {
      workspaceNote('添加目录失败：' + (e.message || '未知错误'), 'error');
    }
  }

  if (els.wsPick) els.wsPick.addEventListener('click', function () {
    activeWorkspaces = savedWorkspaces.slice();
    renderWorkspaces();
    workspaceNote('第一项为主目录；其他目录可用 @workspace2/路径 访问。');
    if (wsDialog && !wsDialog.open) wsDialog.showModal();
  });
  var wsAdd = document.getElementById('workspace-add');
  if (wsAdd) wsAdd.addEventListener('click', addWorkspace);
  var wsSave = document.getElementById('workspace-save');
  if (wsSave) wsSave.addEventListener('click', async function () {
    wsSave.disabled = true;
    workspaceNote('保存中…');
    try { await saveWorkspaces(); } catch (e) { workspaceNote('保存失败：' + (e.message || '未知错误'), 'error'); }
    finally { wsSave.disabled = false; }
  });
  var wsClose = document.getElementById('workspace-dialog-close');
  if (wsClose && wsDialog) wsClose.addEventListener('click', function () { wsDialog.close(); });
  // 关闭页面 / 应用时，把还没入库的会话补进知识库。
  // 用 sendBeacon 而不是 fetch：页面卸载后 fetch 会被浏览器取消，beacon 仍会发出去。
  window.addEventListener('pagehide', function () {
    try {
      if (navigator.sendBeacon) navigator.sendBeacon('/api/agent/flush-memory');
    } catch (e) { /* 关闭时的失败不弹任何东西 */ }
  });

  if (els.modeSelect) {
    els.modeSelect.addEventListener('change', async function () {
      try {
        var body = await apiJSON('/api/agent/settings', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ mode: els.modeSelect.value })
        });
        toast('Agent 档位已切换：' + els.modeSelect.options[els.modeSelect.selectedIndex].text);
        if (body && body.mode) els.modeSelect.value = body.mode;
      } catch (e) {
        toast('切换档位失败：' + (e.message || '未知错误'));
      }
    });
  }
  loadWorkspace();

  els.sidebarOverlay.addEventListener('click', closeSidebar);
  els.newChat.addEventListener('click', newChat);
  if (els.planToggle) {
    els.planToggle.addEventListener('click', function () {
      setPlanCollapsed(!els.planPanel.classList.contains('is-collapsed'));
    });
  }
  if (els.reviewToggle) els.reviewToggle.addEventListener('click', openReview);
  if (els.reviewRefresh) els.reviewRefresh.addEventListener('click', refreshReview);
  if (els.reviewClose) els.reviewClose.addEventListener('click', closeReview);

  // 契约 §7：密钥弹层。入口是右上角按钮；关掉后把 hash 收回当前视图，
  // 否则停在 #/keys 上刷新页面会又弹一次，用户会觉得关不掉。
  if (els.keysOpen) els.keysOpen.addEventListener('click', openKeysPanel);
  if (els.keysClose) els.keysClose.addEventListener('click', closeKeysPanel);
  if (els.keysDialog) {
    els.keysDialog.addEventListener('close', function () {
      if (els.keysOpen) {
        els.keysOpen.setAttribute('aria-expanded', 'false');
        els.keysOpen.focus();
      }
      if (rawHashName() === 'keys') location.hash = '#/' + (state.view || 'chat');
    });
  }

  // 契约 §7：记忆面板。展开时才拉数据（懒加载），摘要行的文档数镜像 kb.js 写的那一格。
  if (els.memoryPanel) {
    els.memoryPanel.addEventListener('toggle', function () {
      if (els.memoryPanel.open) refreshMemory();
    });
    var statDocsEl = document.getElementById('kb-stat-docs');
    if (statDocsEl && window.MutationObserver) {
      new MutationObserver(syncMemoryCount).observe(statDocsEl, { childList: true, characterData: true, subtree: true });
    }
  }
  els.convFilter.addEventListener('input', function () {
    renderConvList();
  });

  if (els.runSettingsToggle && els.composerSettings) {
    els.runSettingsToggle.addEventListener('click', function () {
      var open = els.composerSettings.hidden;
      els.composerSettings.hidden = !open;
      els.runSettingsToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
      if (open) {
        var first = els.composerSettings.querySelector('button, select');
        if (first) first.focus();
      }
    });
  }
  if (els.emptyExamples) {
    els.emptyExamples.addEventListener('click', function (ev) {
      var btn = ev.target.closest ? ev.target.closest('[data-example]') : null;
      if (!btn || !els.input) return;
      els.input.value = btn.getAttribute('data-example') || '';
      autoGrow();
      els.input.focus();
    });
  }

  els.composer.addEventListener('submit', function (ev) {
    ev.preventDefault();
    if (state.streaming) { stopGeneration(); return; }
    sendMessage();
  });

  // 生成中发送键变停止键：同一个按钮，点击语义随状态切换。
  if (els.sendBtn) {
    els.sendBtn.addEventListener('click', function (ev) {
      if (state.streaming) {
        ev.preventDefault();
        stopGeneration();
      }
    });
  }

  els.input.addEventListener('input', function () {
    autoGrow();
    updatePromptPicker();
  });
  els.input.addEventListener('click', updatePromptPicker);
  els.input.addEventListener('keyup', function (ev) {
    // 光标移动也要重算（方向键、Home/End）；Enter 交给 keydown 处理。
    if (ev.key !== 'Enter') updatePromptPicker();
  });
  els.input.addEventListener('keydown', function (ev) {
    // 候选列表打开时，方向键/Enter/Esc 归列表用，Enter 不再是发送。
    if (pickerState.open) {
      if (ev.key === 'ArrowDown') { ev.preventDefault(); movePicker(1); return; }
      if (ev.key === 'ArrowUp') { ev.preventDefault(); movePicker(-1); return; }
      if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing) {
        ev.preventDefault();
        applyActivePrompt();
        return;
      }
      if (ev.key === 'Escape') { ev.preventDefault(); closePromptPicker(); return; }
    }
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      sendMessage();
    }
  });
  els.input.addEventListener('blur', function () {
    // 点选候选项用的是 mousedown + preventDefault，不会触发这里的关闭。
    setTimeout(closePromptPicker, 120);
  });

  // 点击参数面板 / 按钮以外的区域就收起面板。
  document.addEventListener('click', function (ev) {
    if (!paramsUi.el || paramsUi.el.hidden) return;
    if (paramsUi.el.contains(ev.target)) return;
    if (paramsUi.btn && paramsUi.btn.contains(ev.target)) return;
    closeParamsPanel();
  });

  els.modelSelect.addEventListener('change', function () {
    state.model = els.modelSelect.value;
    try {
      localStorage.setItem('chat.model', state.model);
    } catch (e) {}
  });

  window.addEventListener('hashchange', function () {
    handleRoute();
  });

  async function init() {
    buildPromptPicker();
    buildParamsButton();
    buildParamsPanel();
    bindCodeCopy();
    updateStopButton(false);
    refreshParamsPanel();
    handleRoute();
    if (window.APP_RUNTIME) window.APP_RUNTIME.idle(loadPromptLibrary);
    else loadPromptLibrary();
    loadAssistants();
    await Promise.all([loadModels(), loadConversations()]);
    startConversationPolling();
    autoGrow();
    // 首屏就把「当前任务」卡片画出来（内容是「暂无任务 / 0/0 已完成」），
    // 和参考图一样常驻，而不是等第一次对话才出现。
    notifyTaskCard();
    els.input.focus();
  }

  init();
})();
