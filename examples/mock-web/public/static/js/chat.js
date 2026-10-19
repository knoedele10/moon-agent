(function () {
  'use strict';

  var state = {
    conversations: [],
    models: [],
    currentId: null,
    model: '',
    streaming: false,
    view: 'chat',
    convError: ''
  };

  var els = {
    app: document.querySelector('.app'),
    convList: document.getElementById('conv-list'),
    convFilter: document.getElementById('conv-filter'),
    modelsList: document.getElementById('models-list'),
    keysList: document.getElementById('keys-list'),
    thread: document.getElementById('thread'),
    emptyState: document.getElementById('empty-state'),
    messages: document.getElementById('messages'),
    modelSelect: document.getElementById('model-select'),
    input: document.getElementById('input'),
    composer: document.getElementById('composer'),
    sendBtn: document.getElementById('send-btn'),
    newChat: document.getElementById('new-chat'),
    topbarTitle: document.getElementById('topbar-title'),
    sidebar: document.getElementById('sidebar'),
    sidebarToggle: document.getElementById('sidebar-toggle'),
    sidebarOverlay: document.getElementById('sidebar-overlay'),
    toast: document.getElementById('toast')
  };

  var stickBottom = true;
  var toastTimer = null;

  function toast(message) {
    els.toast.textContent = message;
    els.toast.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      els.toast.classList.remove('show');
    }, 2600);
  }

  function safeParse(text) {
    try {
      return JSON.parse(text);
    } catch (e) {
      return null;
    }
  }

  async function api(path, options) {
    var res = await fetch(path, options);
    if (res.status === 401) {
      window.location.href = '/login';
      throw new Error('unauthorized');
    }
    return res;
  }

  async function apiJSON(path, options) {
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
    var el = els.messages;
    requestAnimationFrame(function () {
      el.scrollTop = el.scrollHeight;
    });
  }

  function autoGrow() {
    var el = els.input;
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, 180) + 'px';
  }

  function setStreaming(active) {
    state.streaming = active;
    els.sendBtn.disabled = active;
  }

  function roleLabel(role) {
    return role === 'user' ? '我' : 'AI';
  }

  function appendMessage(role, content) {
    var msg = document.createElement('div');
    msg.className = 'msg ' + (role === 'user' ? 'user' : 'assistant');

    var avatar = document.createElement('div');
    avatar.className = 'avatar';
    avatar.textContent = roleLabel(role);
    msg.appendChild(avatar);

    var bubble = document.createElement('div');
    bubble.className = 'bubble';
    bubble.textContent = content || '';
    msg.appendChild(bubble);

    // Day 11：AI 消息挂"复制"按钮。按钮放 msg 层而不是 bubble 内，
    // 因为流式回复会不断重写 bubble.textContent，会把气泡里的按钮清掉。
    if (role !== 'user') {
      msg.appendChild(buildCopyButton(bubble));
    }

    els.thread.appendChild(msg);
    return bubble;
  }

  // Day 11：复制按钮——点击复制原文，按钮短暂变 ✓ 并弹出 toast，
  // 让用户在"动作发生的原位"得到生效确认；连续点击只重置计时，不会出错。
  function buildCopyButton(bubble) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy-btn';
    btn.title = '复制';
    btn.setAttribute('aria-label', '复制这条消息');
    btn.textContent = '复制';
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

  function renderConvList() {
    els.convList.textContent = '';
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
    list.forEach(function (conv) {
      var item = document.createElement('div');
      item.className = 'conv-item' + (conv.id === state.currentId ? ' active' : '');

      var title = document.createElement('span');
      title.className = 'conv-title';
      title.textContent = conv.title || '新对话';
      item.appendChild(title);

      var del = document.createElement('button');
      del.type = 'button';
      del.className = 'conv-del';
      del.title = '删除会话';
      del.textContent = '×';
      del.addEventListener('click', function (ev) {
        ev.stopPropagation();
        deleteConversation(conv.id);
      });
      item.appendChild(del);

      item.addEventListener('click', function () {
        openConversation(conv.id);
      });

      els.convList.appendChild(item);
    });
  }

  // ---- Day 13：多视图（hash 路由）与列表四种状态 ----

  var VIEW_TITLES = { chat: '对话', models: '模型', keys: 'API Key' };

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

  async function loadModelsView() {
    if (!els.modelsList) return;
    renderListPanel(els.modelsList, { status: 'loading' });
    var models;
    try {
      models = await apiJSON('/api/models');
    } catch (e) {
      renderListPanel(els.modelsList, {
        status: 'error',
        errorText: '模型加载失败：' + (e.message || '未知错误'),
        onRetry: loadModelsView
      });
      return;
    }
    models = Array.isArray(models) ? models : [];
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

  function viewFromHash() {
    var name = (location.hash || '').replace(/^#\/?/, '');
    return VIEW_TITLES[name] ? name : 'chat';
  }

  function showView(name) {
    state.view = name;
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
    if (name === 'keys') loadKeysView();
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
  }

  async function loadConversations() {
    state.convError = '';
    try {
      var list = await apiJSON('/api/conversations');
      state.conversations = Array.isArray(list) ? list : [];
    } catch (e) {
      state.conversations = [];
      state.convError = e.message || '未知错误';
    }
    renderConvList();
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
    if (state.streaming) return;
    if (state.view !== 'chat') location.hash = '#/chat';
    state.currentId = null;
    els.thread.textContent = '';
    els.emptyState.hidden = false;
    els.topbarTitle.textContent = '新对话';
    renderConvList();
    closeSidebar();
    els.input.focus();
  }

  async function openConversation(id) {
    if (state.streaming) {
      toast('请等待当前回复结束');
      return;
    }
    closeSidebar();
    if (state.view !== 'chat') location.hash = '#/chat';
    if (id === state.currentId) return;

    state.currentId = id;
    var conv = state.conversations.filter(function (c) {
      return c.id === id;
    })[0];
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
    renderConvList();

    els.thread.textContent = '';
    els.emptyState.hidden = true;
    var loading = document.createElement('div');
    loading.className = 'list-hint';
    loading.textContent = '加载历史…';
    els.thread.appendChild(loading);

    var history;
    try {
      history = await apiJSON('/api/conversations/' + id + '/messages');
    } catch (e) {
      els.thread.textContent = '';
      els.emptyState.hidden = false;
      toast('历史加载失败：' + e.message);
      return;
    }

    els.thread.textContent = '';
    if (!Array.isArray(history) || !history.length) {
      els.emptyState.hidden = false;
      return;
    }
    els.emptyState.hidden = true;
    history.forEach(function (m) {
      appendMessage(m.role === 'user' ? 'user' : 'assistant', m.content);
    });
    scrollToBottom(true);
  }

  async function deleteConversation(id) {
    if (state.streaming) {
      toast('请等待当前回复结束');
      return;
    }
    var conv = state.conversations.filter(function (c) {
      return c.id === id;
    })[0];
    var name = (conv && conv.title) || '该会话';
    if (!window.confirm('删除「' + name + '」？此操作不可撤销。')) return;

    try {
      await apiJSON('/api/conversations/' + id, { method: 'DELETE' });
    } catch (e) {
      toast('删除失败：' + e.message);
      return;
    }

    state.conversations = state.conversations.filter(function (c) {
      return c.id !== id;
    });
    if (state.currentId === id) {
      newChat();
    } else {
      renderConvList();
    }
  }

  async function streamChat(payload, onDelta, onDone, onError) {
    var res = await fetch('/api/chat', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'text/event-stream'
      },
      body: JSON.stringify(payload)
    });

    if (res.status === 401) {
      window.location.href = '/login';
      throw new Error('unauthorized');
    }

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
      } else if (event === 'done') {
        finished = true;
        onDone();
      } else if (event === 'error') {
        var err = safeParse(data);
        finished = true;
        onError(err && err.error ? err.error : '服务端返回错误');
      }
    }

    function drain() {
      // 末尾孤立的 \r 可能只是 \r\n 被 chunk 切开的前半个，留到下一轮再判定，
      // 否则会被提前归一化成 \n，与下一轮补上的 \n 凑成假的事件边界。
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

  async function sendMessage() {
    if (state.streaming) return;
    var text = els.input.value.trim();
    if (!text) return;
    if (!state.model) {
      toast('没有可用模型，请先在设置中配置 Key');
      return;
    }

    setStreaming(true);
    els.input.value = '';
    autoGrow();
    stickBottom = true;

    els.emptyState.hidden = true;
    appendMessage('user', text);

    var bubble = appendMessage('assistant', '');
    var textNode = document.createTextNode('');
    bubble.appendChild(textNode);
    var cursor = document.createElement('span');
    cursor.className = 'cursor';
    bubble.appendChild(cursor);
    scrollToBottom(true);

    var conversationId = state.currentId;
    if (conversationId == null) {
      try {
        var conv = await createConversation();
        conversationId = conv.id;
        state.currentId = conv.id;
        els.topbarTitle.textContent = conv.title || '新对话';
        renderConvList();
      } catch (e) {
        cursor.remove();
        bubble.classList.add('is-error');
        bubble.textContent = '创建会话失败：' + e.message;
        setStreaming(false);
        return;
      }
    }

    var gotAny = false;
    var settled = false;

    function finishError(message) {
      if (settled) return;
      settled = true;
      cursor.remove();
      if (gotAny) {
        var note = document.createElement('span');
        note.style.color = 'var(--danger)';
        note.textContent = '\n[出错] ' + message;
        bubble.appendChild(note);
      } else {
        bubble.classList.add('is-error');
        bubble.textContent = message;
      }
      setStreaming(false);
      scrollToBottom(true);
    }

    function finishDone() {
      if (settled) return;
      settled = true;
      cursor.remove();
      if (!gotAny) textNode.appendData('（模型未返回内容）');
      setStreaming(false);
      scrollToBottom(true);
    }

    try {
      await streamChat(
        { conversation_id: conversationId, model: state.model, content: text },
        function (piece) {
          gotAny = true;
          textNode.appendData(piece);
          scrollToBottom(false);
        },
        finishDone,
        finishError
      );
    } catch (e) {
      finishError(e.message || '网络错误');
    }
    if (!settled) finishDone();
  }

  function openSidebar() {
    els.sidebar.classList.add('open');
    els.sidebarOverlay.classList.add('show');
  }

  function closeSidebar() {
    els.sidebar.classList.remove('open');
    els.sidebarOverlay.classList.remove('show');
  }

  els.messages.addEventListener('scroll', function () {
    var el = els.messages;
    stickBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
  });

  els.sidebarToggle.addEventListener('click', openSidebar);
  els.sidebarOverlay.addEventListener('click', closeSidebar);
  els.newChat.addEventListener('click', newChat);
  els.convFilter.addEventListener('input', function () {
    renderConvList();
  });

  els.composer.addEventListener('submit', function (ev) {
    ev.preventDefault();
    sendMessage();
  });

  els.input.addEventListener('input', autoGrow);
  els.input.addEventListener('keydown', function (ev) {
    if (ev.key === 'Enter' && !ev.shiftKey && !ev.isComposing) {
      ev.preventDefault();
      sendMessage();
    }
  });

  els.modelSelect.addEventListener('change', function () {
    state.model = els.modelSelect.value;
    try {
      localStorage.setItem('chat.model', state.model);
    } catch (e) {}
  });

  window.addEventListener('hashchange', function () {
    showView(viewFromHash());
  });

  async function init() {
    showView(viewFromHash());
    await Promise.all([loadModels(), loadConversations()]);
    autoGrow();
    els.input.focus();
  }

  init();
})();
