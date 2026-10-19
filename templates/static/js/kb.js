// Day 23：本地知识库视图。
// 只依赖 templates/chat/index.html 里的固定元素 id，通过 window.KB.onShow() 被 chat.js 的
// hash 路由唤起（views 的显示/隐藏由 .app[data-view="kb"] 的 CSS 负责，这里不管样式切换）。
// 后端契约见 docs/api-contract.md 的 /api/kb/* 一节；本文件不发明任何新字段。
(function () {
  'use strict';

  var state = {
    docs: [],
    statsLoaded: false,
    firstShow: true,
    busy: false,
    askAbort: null,
    // 文档列表当前的来源筛选值（all / document / memory）与最近一次请求序号（防并发乱序）。
    docSource: 'all',
    docsSeq: 0
  };

  var els = {};
  var toastTimer = null;

  function byId(id) {
    return document.getElementById(id);
  }

  // 复用页面顶部的 toast（chat.js 也在用），没有就静默降级为 console
  function toast(message) {
    var el = byId('toast');
    if (!el) {
      if (window.console) window.console.log('[kb]', message);
      return;
    }
    el.textContent = message;
    el.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      el.classList.remove('show');
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
      throw new Error('登录已过期，请重新登录');
    }
    return res;
  }

  async function apiJSON(path, options) {
    var res = await api(path, options);
    var body = null;
    try {
      body = await res.json();
    } catch (e) {
      body = null;
    }
    if (!res.ok) {
      throw new Error((body && body.error) || ('HTTP ' + res.status));
    }
    return body;
  }

  function note(el, text, kind) {
    if (!el) return;
    el.textContent = text || '';
    el.className = 'kb-note' + (kind ? ' kb-note-' + kind : '');
  }

  function humanBytes(n) {
    if (typeof n !== 'number' || n < 0) return '—';
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1024 / 1024).toFixed(2) + ' MB';
  }

  function humanTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso);
    var pad = function (n) { return String(n).padStart(2, '0'); };
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  // ---------- 概览 ----------

  async function refreshStats() {
    if (!els.statDocs) return;
    els.statDocs.textContent = '…';
    els.statChunks.textContent = '…';
    try {
      var st = await apiJSON('/api/kb/stats');
      els.statDocs.textContent = st && st.documents !== undefined ? st.documents : '—';
      els.statChunks.textContent = st && st.chunks !== undefined ? st.chunks : '—';
      renderSourceCounts(st && st.by_source);
      state.statsLoaded = true;
    } catch (e) {
      els.statDocs.textContent = '—';
      els.statChunks.textContent = '—';
      renderSourceCounts(null);
      note(els.statNote, '统计加载失败：' + (e.message || '未知错误'), 'error');
    }
  }

  // ---------- 文档列表 ----------

  async function loadDocs() {
    var box = els.docList;
    if (!box) return;
    var filter = currentFilter();
    // 每次请求带一个递增序号：点击分段控件太快时，后到的旧响应不能覆盖新列表。
    var seq = ++state.docsSeq;
    box.textContent = '';
    box.appendChild(el('div', 'list-hint', '加载中…'));
    var body;
    try {
      body = await apiJSON('/api/kb/documents?source=' + encodeURIComponent(filter.value));
    } catch (e) {
      if (seq !== state.docsSeq) return;
      box.textContent = '';
      var state1 = el('div', 'list-state');
      state1.appendChild(el('div', 'list-state-text', '文档加载失败：' + (e.message || '未知错误')));
      var retry = el('button', 'btn btn-ghost list-state-btn', '重试');
      retry.type = 'button';
      retry.title = '重新加载文档列表';
      retry.setAttribute('aria-label', '重新加载文档列表');
      retry.addEventListener('click', loadDocs);
      state1.appendChild(retry);
      box.appendChild(state1);
      return;
    }
    if (seq !== state.docsSeq) return;
    var items = body && Array.isArray(body.data) ? body.data : [];
    var total = body && typeof body.total === 'number' ? body.total : items.length;
    state.docs = items;
    box.textContent = '';
    if (!items.length) {
      // 空态文案要跟当前筛选相符，不能一律说“知识库还是空的”。
      box.appendChild(el('div', 'list-hint', filter.empty));
      note(els.listNote, '');
      return;
    }
    items.forEach(function (doc) {
      box.appendChild(buildDocRow(doc));
    });
    note(els.listNote, '共 ' + total + ' 个条目（' + filter.label + '）。');
  }

  function buildDocRow(doc) {
    var row = el('div', 'kb-doc-row');
    // 来源徽标：AI 记忆 / 用户画像 / 资料（source 为空串时按「资料」算）。
    var src = sourceBadge(doc.source);
    row.appendChild(el('span', 'kb-src-badge kb-src-' + src.mod, src.label));

    var badge = el('span', 'kb-doc-kind' + (doc.kind === 'image' ? ' kb-doc-kind-image' : ''), doc.kind === 'image' ? '图片' : '文本');
    row.appendChild(badge);

    var main = el('div', 'kb-doc-main');
    main.appendChild(el('div', 'kb-doc-title', doc.title || '(无标题)'));
    var bits = [];
    bits.push('id=' + doc.id);
    bits.push('块 ' + (doc.chunk_count === undefined ? '?' : doc.chunk_count));
    bits.push(humanBytes(doc.bytes));
    if (doc.mime) bits.push(doc.mime);
    if (doc.created_at) bits.push(humanTime(doc.created_at));
    main.appendChild(el('div', 'kb-doc-meta', bits.join(' · ')));
    row.appendChild(main);

    var actions = el('div', 'kb-doc-actions');
    var delLabel = doc.source === 'memory' ? '删除这条记忆'
      : (doc.source === 'profile' ? '删除这张用户画像' : '删除文档');
    var del = el('button', 'btn btn-danger kb-doc-del', '删除');
    del.type = 'button';
    del.title = delLabel;
    del.setAttribute('aria-label', delLabel + '：' + (doc.title || doc.id));
    del.addEventListener('click', function () {
      deleteDoc(doc, row);
    });
    actions.appendChild(del);
    row.appendChild(actions);
    return row;
  }

  async function deleteDoc(doc, row) {
    if (state.busy) {
      toast('上一个操作还没结束，请稍候');
      return;
    }
    // AI 记忆 / 用户画像与普通资料走的是同一个删除接口，但确认文案要说得清楚。
    var isMemory = doc.source === 'memory' || doc.source === 'profile';
    var what = doc.source === 'memory' ? '这条 AI 记忆'
      : (doc.source === 'profile' ? '这张用户画像' : '文档「' + (doc.title || doc.id) + '」');
    var hint = doc.source === 'memory' ? '删除后 AI 不会再检索到它。' : '该文档的全部检索块会一起删除。';
    if (!window.confirm('删除' + what + '？' + hint + '不可恢复。')) return;
    state.busy = true;
    try {
      await apiJSON('/api/kb/documents/' + doc.id, { method: 'DELETE' });
    } catch (e) {
      toast('删除失败：' + (e.message || '未知错误'));
      state.busy = false;
      return;
    }
    state.busy = false;
    // 先从界面上移除这一项（立刻可见），再拉一次列表与计数跟后端对齐。
    if (row && row.parentNode) row.remove();
    toast(isMemory ? '已删除这条记忆' : '已删除文档 ' + doc.id);
    note(els.listNote, isMemory
      ? '已删除这条记忆（id=' + doc.id + '）。'
      : '已删除「' + (doc.title || doc.id) + '」（id=' + doc.id + '）。');
    await Promise.all([loadDocs(), refreshStats()]);
  }

  // ---------- 上传 / 粘贴文本 ----------

  async function onUpload(ev) {
    ev.preventDefault();
    if (state.busy) {
      toast('上一个操作还没结束，请稍候');
      return;
    }
    var file = els.file && els.file.files && els.file.files[0];
    if (!file) {
      note(els.uploadNote, '请先选择一个文件。', 'error');
      return;
    }
    var fd = new FormData();
    fd.append('file', file, file.name);

    state.busy = true;
    els.uploadBtn.disabled = true;
    note(els.uploadNote, '上传中…' + (file.name.match(/\.(png|jpe?g|webp|gif)$/i) ? '（图片要先经过视觉模型，可能要几十秒）' : ''));
    try {
      var body = await apiJSON('/api/kb/documents', { method: 'POST', body: fd });
      if (body && body.duplicated) {
        note(els.uploadNote, '内容已存在（sha256 去重命中），复用文档 id=' + body.id + '，未重复入库。', 'ok');
      } else {
        note(els.uploadNote, '已入库：id=' + body.id + '，kind=' + body.kind + '，切出 ' + body.chunk_count + ' 个块。', 'ok');
      }
      toast('上传成功');
      if (els.file) els.file.value = '';
    } catch (e) {
      note(els.uploadNote, '上传失败：' + (e.message || '未知错误'), 'error');
      state.busy = false;
      els.uploadBtn.disabled = false;
      return;
    }
    state.busy = false;
    els.uploadBtn.disabled = false;
    await Promise.all([loadDocs(), refreshStats()]);
  }

  async function onTextSubmit(ev) {
    ev.preventDefault();
    if (state.busy) {
      toast('上一个操作还没结束，请稍候');
      return;
    }
    var text = els.textBody.value.trim();
    var title = els.textTitle.value.trim();
    if (!text) {
      note(els.textNote, '正文不能为空。', 'error');
      return;
    }
    state.busy = true;
    els.textBtn.disabled = true;
    note(els.textNote, '入库中…（要把文本切块后逐块嵌入，稍等）');
    try {
      var body = await apiJSON('/api/kb/text', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: title, text: text })
      });
      if (body && body.duplicated) {
        note(els.textNote, '这段文本已在库里（去重命中），复用文档 id=' + body.id + '。', 'ok');
      } else {
        note(els.textNote, '已入库：id=' + body.id + '，标题「' + body.title + '」，切出 ' + body.chunk_count + ' 个块。', 'ok');
      }
      els.textBody.value = '';
      els.textTitle.value = '';
    } catch (e) {
      note(els.textNote, '入库失败：' + (e.message || '未知错误'), 'error');
      state.busy = false;
      els.textBtn.disabled = false;
      return;
    }
    state.busy = false;
    els.textBtn.disabled = false;
    await Promise.all([loadDocs(), refreshStats()]);
  }

  // ---------- 检索 ----------

  function hitCard(hit, index) {
    var card = el('div', 'kb-hit');
    var head = el('div', 'kb-hit-head');
    head.appendChild(el('span', 'kb-hit-rank', '[' + (index + 1) + ']'));
    head.appendChild(el('span', 'kb-hit-title', hit.title || '(无标题)'));
    head.appendChild(el('span', 'kb-hit-kind', hit.kind === 'image_caption' ? '图片块' : '文本块'));
    head.appendChild(el('span', 'kb-hit-score', '相似度 ' + (typeof hit.score === 'number' ? hit.score.toFixed(3) : '—')));
    card.appendChild(head);
    var text = el('div', 'kb-hit-text');
    text.textContent = hit.text || '';
    card.appendChild(text);
    var meta = el('div', 'kb-hit-meta', 'document_id=' + hit.document_id + '　chunk_id=' + hit.chunk_id + '　seq=' + hit.seq);
    card.appendChild(meta);
    return card;
  }

  function renderHits(container, hits, emptyText) {
    container.textContent = '';
    if (!hits.length) {
      container.appendChild(el('div', 'list-hint', emptyText || '没有命中任何块。'));
      return;
    }
    hits.forEach(function (hit, i) {
      container.appendChild(hitCard(hit, i));
    });
  }

  async function onSearch(ev) {
    ev.preventDefault();
    var query = els.searchInput.value.trim();
    if (!query) {
      els.searchResult.textContent = '';
      els.searchResult.appendChild(el('div', 'list-hint', '请输入检索内容。'));
      return;
    }
    els.searchBtn.disabled = true;
    els.searchResult.textContent = '';
    els.searchResult.appendChild(el('div', 'list-hint', '检索中…'));
    try {
      var body = await apiJSON('/api/kb/search', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ query: query, top_k: 5 })
      });
      renderHits(els.searchResult, (body && Array.isArray(body.hits)) ? body.hits : [], '没有命中任何块。');
    } catch (e) {
      els.searchResult.textContent = '';
      els.searchResult.appendChild(el('div', 'kb-note kb-note-error', '检索失败：' + (e.message || '未知错误')));
    }
    els.searchBtn.disabled = false;
  }

  // ---------- 知识库问答（SSE）----------

  // 事件口径与后端一致：先 sources（命中的资料），再若干 delta（正文增量），最后 done 或 error。
  function sourceList(hits) {
    var box = el('div', 'kb-sources');
    box.appendChild(el('div', 'kb-sources-title', '检索到的资料（' + hits.length + ' 条）'));
    hits.forEach(function (hit, i) {
      var row = el('button', 'kb-source');
      row.type = 'button';
      row.title = '点击查看该块全文';
      var head = el('div', 'kb-source-head');
      head.appendChild(el('span', 'kb-source-rank', '[' + (i + 1) + ']'));
      head.appendChild(el('span', 'kb-source-title', hit.title || '(无标题)'));
      head.appendChild(el('span', 'kb-source-score', typeof hit.score === 'number' ? hit.score.toFixed(3) : '—'));
      row.appendChild(head);
      var preview = el('div', 'kb-source-text', hit.text || '');
      row.appendChild(preview);
      row.addEventListener('click', function () {
        row.classList.toggle('open');
      });
      box.appendChild(row);
    });
    return box;
  }

  async function onAsk(ev) {
    ev.preventDefault();
    if (state.busy) {
      toast('上一个操作还没结束，请稍候');
      return;
    }
    var query = els.askInput.value.trim();
    if (!query) {
      els.askResult.textContent = '';
      els.askResult.appendChild(el('div', 'list-hint', '请输入问题。'));
      return;
    }
    var modelSel = byId('model-select');
    var model = modelSel ? modelSel.value : '';
    if (!model) {
      els.askResult.textContent = '';
      els.askResult.appendChild(el('div', 'kb-note kb-note-error', '没有可用模型：请先在「模型」视图确认已配置模型。'));
      return;
    }

    state.busy = true;
    els.askBtn.disabled = true;
    els.askResult.textContent = '';
    var running = el('div', 'list-hint', '检索并生成中…（本地模型首次加载可能较慢）');
    els.askResult.appendChild(running);

    var controller = null;
    try {
      controller = new AbortController();
    } catch (e) {
      controller = null;
    }
    state.askAbort = controller;

    var answerBox = null;
    var answerText = null;
    var answerMd = null; // Markdown 内容容器（光标是它的兄弟节点，不能被 innerHTML 冲掉）
    var answerAcc = '';
    var cursor = null;
    var sourcesBox = null;

    function ensureAnswer() {
      if (answerBox) return;
      running.remove();
      sourcesBox = sourcesBox || el('div', 'kb-sources kb-sources-empty');
      answerBox = el('div', 'kb-answer');
      answerBox.appendChild(el('div', 'kb-answer-role', '回答（模型：' + model + '）'));
      answerText = el('div', 'kb-answer-text');
      answerMd = el('div', 'md');
      cursor = el('span', 'cursor');
      answerText.appendChild(answerMd);
      answerText.appendChild(cursor);
      answerBox.appendChild(answerText);
      els.askResult.appendChild(answerBox);
    }

    var finished = false;
    var gotDelta = false;

    function onSources(hits) {
      running.remove();
      if (sourcesBox) sourcesBox.remove();
      sourcesBox = sourceList(hits || []);
      els.askResult.insertBefore(sourcesBox, answerBox);
    }

    function onDelta(content) {
      gotDelta = true;
      ensureAnswer();
      // 与聊天页同一套 Markdown 渲染（先转义 HTML 再套白名单语法）。
      answerAcc += content;
      if (window.MD && window.MD.render) {
        answerMd.innerHTML = window.MD.render(answerAcc);
      } else {
        answerMd.textContent = answerAcc;
      }
    }

    function onDone() {
      if (finished) return;
      finished = true;
      ensureAnswer();
      if (cursor) cursor.remove();
      if (!gotDelta) answerMd.textContent = '（模型没有返回内容）';
    }

    function onError(message) {
      if (finished) return;
      finished = true;
      if (answerBox) {
        if (cursor) cursor.remove();
        answerBox.appendChild(el('div', 'kb-note kb-note-error', '[出错] ' + message));
      } else {
        running.remove();
        els.askResult.appendChild(el('div', 'kb-note kb-note-error', '提问失败：' + message));
      }
    }

    try {
      await streamAsk({ query: query, model: model, top_k: 5 }, { onSources: onSources, onDelta: onDelta, onDone: onDone, onError: onError }, controller);
      if (!finished) onDone();
    } catch (e) {
      onError(e.message || '网络错误');
    }
    state.askAbort = null;
    state.busy = false;
    els.askBtn.disabled = false;
  }

  async function streamAsk(payload, handlers, controller) {
    var res = await api('/api/kb/ask', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body: JSON.stringify(payload),
      signal: controller ? controller.signal : undefined
    });
    if (!res.ok) {
      var message = 'HTTP ' + res.status;
      try {
        var body = await res.json();
        if (body && body.error) message = body.error;
      } catch (e) {}
      throw new Error(message);
    }
    if (!res.body || !res.body.getReader) throw new Error('当前浏览器不支持流式读取');

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
        if (field === 'event') event = value;
        else if (field === 'data') dataLines.push(value);
      }
      var data = safeParse(dataLines.join('\n'));
      if (event === 'sources') {
        handlers.onSources(data && Array.isArray(data.hits) ? data.hits : []);
      } else if (event === 'delta') {
        if (data && typeof data.content === 'string') handlers.onDelta(data.content);
      } else if (event === 'done') {
        finished = true;
        handlers.onDone();
      } else if (event === 'error') {
        finished = true;
        handlers.onError((data && data.error) ? data.error : '服务端返回错误');
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

  // ---------- 生命周期 ----------

  function bind() {
    els = {
      statDocs: byId('kb-stat-docs'),
      statChunks: byId('kb-stat-chunks'),
      statNote: byId('kb-stat-note'),
      uploadForm: byId('kb-upload-form'),
      file: byId('kb-file'),
      uploadBtn: byId('kb-upload-btn'),
      uploadNote: byId('kb-upload-note'),
      textForm: byId('kb-text-form'),
      textTitle: byId('kb-text-title'),
      textBody: byId('kb-text-body'),
      textBtn: byId('kb-text-btn'),
      textNote: byId('kb-text-note'),
      searchForm: byId('kb-search-form'),
      searchInput: byId('kb-search-input'),
      searchBtn: byId('kb-search-btn'),
      searchResult: byId('kb-search-result'),
      askForm: byId('kb-ask-form'),
      askInput: byId('kb-ask-input'),
      askBtn: byId('kb-ask-btn'),
      askResult: byId('kb-ask-result'),
      docList: byId('kb-doc-list'),
      listNote: byId('kb-list-note'),
      askModel: byId('kb-ask-model'),
      wsForm: byId('ws-form'),
      wsPath: byId('ws-path'),
      wsNote: byId('ws-note'),
      wsSave: byId('ws-save')
    };
    if (!els.uploadForm) return false;

    // 文档列表上方的来源分段控件（全部 / 我的文档 / AI 记忆）由 JS 动态插入。
    buildFilterBar();

    els.uploadForm.addEventListener('submit', onUpload);
    els.textForm.addEventListener('submit', onTextSubmit);
    els.searchForm.addEventListener('submit', onSearch);
    els.askForm.addEventListener('submit', onAsk);
    if (els.wsForm) els.wsForm.addEventListener('submit', onSaveWorkspace);
    loadWorkspace();

    // 顶栏的模型选择器同时决定知识库问答用哪个模型，这里把选择结果显式写出来，
    // 免得用户以为问答用的是"知识库内置模型"。
    var modelSel = byId('model-select');
    if (modelSel) modelSel.addEventListener('change', updateAskModel);
    return true;
  }

  // ---------- 工作区设置 ----------

  // 工作区 = AI 文件工具的沙箱根目录。用户在这里自己设，后端存到用户配置里。
  async function loadWorkspace() {
    if (!els.wsPath) return;
    try {
      var st = await apiJSON('/api/agent/settings');
      var paths = Array.isArray(st.workspaces) ? st.workspaces : (st.workspace ? [st.workspace] : []);
      els.wsPath.value = paths.join('\n');
      var src = st.source === 'user' ? '（你自己设置的）' : (st.source === 'env' ? '（服务端默认）' : '（还没设置）');
      var tip = st.tools_enabled ? '' : ' 注意：服务端未开启工具调用。';
      note(els.wsNote, '当前启用 ' + paths.length + ' 个工作目录：' + (paths.length ? paths.join('；') : '未设置') + src + tip);
    } catch (e) {
      note(els.wsNote, '读取工作区失败：' + (e.message || '未知错误'), 'error');
    }
  }

  async function onSaveWorkspace(ev) {
    ev.preventDefault();
    if (!els.wsPath) return;
    var paths = els.wsPath.value.split(/\r?\n/).map(function (p) { return p.trim(); }).filter(Boolean);
    if (paths.length > 20) { note(els.wsNote, '最多同时启用 20 个目录。', 'error'); return; }
    if (els.wsSave) els.wsSave.disabled = true;
    note(els.wsNote, '保存中…');
    try {
      var body = await apiJSON('/api/agent/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ workspaces: paths })
      });
      note(els.wsNote, '已保存 ' + (body.workspaces || []).length + ' 个活动工作目录。', 'ok');
      toast('工作区已更新');
    } catch (e) {
      note(els.wsNote, '保存失败：' + (e.message || '未知错误'), 'error');
    } finally {
      if (els.wsSave) els.wsSave.disabled = false;
    }
  }

  function updateAskModel() {
    if (!els.askModel) return;
    var modelSel = byId('model-select');
    var model = modelSel ? modelSel.value : '';
    els.askModel.textContent = model ? '将使用顶栏选中的模型「' + model + '」生成回答。' : '还没有可用模型：请先去「模型」视图确认已配置。';
  }

  // 每次切到知识库视图都刷新一次列表与统计：别的地方（脚本、别的标签页）也可能改过库。
  function onShow() {
    if (!els.uploadForm && !bind()) return;
    updateAskModel();
    refreshStats();
    loadWorkspace();
    loadDocs();
    state.firstShow = false;
  }

  // onShow 之外再暴露一个刷新入口：chat.js 在 init 里异步加载完模型后可能改了顶栏的值，
  // 此时若用户已经在知识库视图，提示语需要跟着更新。
  window.KB = { onShow: onShow, refreshAskModel: updateAskModel };

  // ---------- 来源分段筛选 ----------
  //
  // 后端契约（web/kb.go 的 kbSourceFilter / kbListDocuments）：
  //   all      全部
  //   document source <> 'memory'（用户上传/粘贴的资料 + 系统生成的「用户画像」）
  //   memory   source = 'memory'（对话里自动抽取的记忆）
  var SOURCE_FILTERS = [
    { value: 'all', label: '全部', empty: '知识库还是空的。上传一个 .txt/.md 文件，或粘贴一段文本试试。' },
    { value: 'document', label: '我的文档', empty: '还没有我的文档。上传一个 .txt/.md 文件，或粘贴一段文本试试。' },
    { value: 'memory', label: 'AI 记忆', empty: '还没有 AI 记忆。在对话里聊几句，AI 会自动记下要点。' }
  ];

  // 每项来源的徽标文案与样式后缀；source 为空串按普通「资料」处理。
  function sourceBadge(source) {
    if (source === 'memory') return { label: 'AI 记忆', mod: 'memory' };
    if (source === 'profile') return { label: '用户画像', mod: 'profile' };
    return { label: '资料', mod: 'document' };
  }

  function currentFilter() {
    for (var i = 0; i < SOURCE_FILTERS.length; i++) {
      if (SOURCE_FILTERS[i].value === state.docSource) return SOURCE_FILTERS[i];
    }
    return SOURCE_FILTERS[0];
  }

  // style.css 属于另外的 worker（本任务不能改），而分段控件与来源徽标需要一点新样式。
  // 这里动态注入一小段 kb-* 前缀的规则：只复用页面已有的 CSS 变量与胶囊样式尺度，
  // 不引入新的视觉体系，也不会影响其它视图。重复调用（多次 onShow）只会注入一次。
  function injectStyles() {
    if (document.getElementById('kb-source-style')) return;
    var style = document.createElement('style');
    style.id = 'kb-source-style';
    style.textContent = [
      '.kb-filter{display:flex;flex-wrap:wrap;align-items:center;gap:8px;margin:0 0 10px;}',
      '.kb-filter-group{display:inline-flex;flex-wrap:wrap;gap:6px;}',
      '.kb-filter-btn{min-height:30px;padding:4px 12px;font-size:12px;}',
      '.kb-filter-count{margin-left:auto;color:var(--text-muted);font-size:12px;line-height:1.5;}',
      '.kb-src-badge{flex:none;padding:4px 8px;border:1px solid var(--border);border-radius:6px;font-size:11px;line-height:1.4;color:var(--text-faint);white-space:nowrap;}',
      '.kb-src-memory{border-color:rgba(146,178,216,0.28);background:rgba(146,178,216,0.12);color:#a9c6e6;}',
      '.kb-src-profile{border-color:rgba(224,167,90,0.4);background:rgba(224,167,90,0.12);color:#e0a75a;}',
      '.kb-src-document{border-color:rgba(199,217,151,0.2);background:var(--accent-soft);color:var(--accent);}'
    ].join('');
    document.head.appendChild(style);
  }

  // 把分段控件插到文档列表上方（不能改 templates/chat/index.html，所以由 JS 动态插入）。
  function buildFilterBar() {
    if (els.filterBar || !els.docList || !els.docList.parentNode) return;
    injectStyles();
    var bar = el('div', 'kb-filter');
    var group = el('div', 'kb-filter-group');
    group.setAttribute('role', 'group');
    group.setAttribute('aria-label', '按来源筛选文档');
    els.filterBtns = {};
    SOURCE_FILTERS.forEach(function (f) {
      var btn = el('button', 'btn kb-filter-btn', f.label);
      btn.type = 'button';
      btn.title = '显示' + f.label;
      btn.setAttribute('aria-label', '显示' + f.label);
      btn.setAttribute('aria-pressed', 'false');
      btn.addEventListener('click', function () { setSourceFilter(f.value); });
      els.filterBtns[f.value] = btn;
      group.appendChild(btn);
    });
    bar.appendChild(group);
    els.filterCount = el('span', 'kb-filter-count');
    els.filterCount.setAttribute('aria-live', 'polite');
    bar.appendChild(els.filterCount);
    els.docList.parentNode.insertBefore(bar, els.docList);
    els.filterBar = bar;
    updateFilterButtons();
    renderSourceCounts(null);
  }

  function updateFilterButtons() {
    if (!els.filterBtns) return;
    var active = currentFilter().value;
    SOURCE_FILTERS.forEach(function (f) {
      var btn = els.filterBtns[f.value];
      if (!btn) return;
      var on = f.value === active;
      btn.className = 'btn kb-filter-btn' + (on ? '' : ' btn-ghost');
      btn.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
  }

  function setSourceFilter(value) {
    if (state.docSource === value) return;
    state.docSource = value;
    updateFilterButtons();
    loadDocs();
  }

  // 计数只来自后端 by_source，不在前端拿当前页的条数凑 —— 列表有 limit，数出来会对不上。
  function renderSourceCounts(by) {
    if (!els.filterCount) return;
    if (!by) {
      els.filterCount.textContent = '';
      return;
    }
    var doc = Number(by.document) || 0;
    var mem = Number(by.memory) || 0;
    var prof = Number(by.profile) || 0;
    var parts = ['资料 ' + doc + ' 篇', '记忆 ' + mem + ' 条'];
    if (prof > 0) parts.push('画像 ' + prof + ' 条');
    els.filterCount.textContent = parts.join(' · ');
  }

  // 启动：DOM 就绪后 bind()。这段放在文件末尾，确保上面的定义（含 SOURCE_FILTERS）都已初始化，
  // 否则 readyState 不是 loading 时 bind() 会立即执行，而 SOURCE_FILTERS 还没赋值。
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bind);
  } else {
    bind();
  }
})();
