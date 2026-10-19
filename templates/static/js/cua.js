// Day 25：电脑操控（CUA）视图。
// 契约：docs/cua/06-integration-contract.md §2（/api/cua/* 五个接口）与 §4（本视图）。
// 只依赖 templates/chat/index.html 里的固定元素 id；SSE 用原生 EventSource，不引入任何前端依赖。
// 与 kb.js 一样先于 chat.js 加载：chat.js 的 hash 路由在 showView() 里调 window.CUA.onShow()。
// 本文件只渲染后端真的发过来的字段，不发明任何新字段（事件 schema 见契约 §1）。
(function () {
  'use strict';

  // Go 侧把 CUA 的 kind 原样当事件名透传（契约 §2.3）；frame_image 是可选事件（契约 §1）。
  var EVENT_KINDS = [
    'run_start', 'frame', 'frame_image', 'observation', 'agent', 'step',
    'action_result', 'terminal_action_result', 'replan', 'expect_timeout',
    'executor_retry', 'guardrail', 'model_retry', 'run_end',
    'popup_detected', 'popup_crop', 'popup_crop_failed', 'popup_esc'
  ];

  // EventSource 断开后浏览器会自动重连；连续失败这么多次就停手，避免 404 时无限重试刷日志。
  var MAX_STREAM_ERRORS = 3;

  var state = {
    status: null,
    models: [],
    runId: '',
    task: '',
    model: '',
    startedAt: 0,
    lastTs: 0,
    maxStep: 0,
    frames: 0,
    images: 0,
    steps: {},
    end: null,
    exit: null,
    shotImg: null,
    es: null,
    errCount: 0,
    exitSeen: false,
    active: false,
    busy: false,
    lastExitRunId: '',
    replay: false,
    runs: null
  };

  var els = {};
  var toastTimer = null;

  function byId(id) {
    return document.getElementById(id);
  }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  function toast(message) {
    var box = byId('toast');
    if (!box) {
      if (window.console) window.console.log('[cua]', message);
      return;
    }
    box.textContent = message;
    box.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      box.classList.remove('show');
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

  function note(node, text, kind) {
    if (!node) return;
    node.textContent = text || '';
    node.className = 'cua-note' + (kind ? ' cua-note-' + kind : '');
  }

  function ms(value) {
    if (typeof value !== 'number' || !isFinite(value) || value < 0) return '—';
    if (value < 1000) return Math.round(value) + ' ms';
    if (value < 60000) return (value / 1000).toFixed(1) + ' s';
    return Math.floor(value / 60000) + ' m ' + Math.round((value % 60000) / 1000) + ' s';
  }

  function compact(value) {
    var text;
    try {
      text = JSON.stringify(value);
    } catch (e) {
      text = String(value);
    }
    if (!text) return '';
    return text.length > 180 ? text.slice(0, 180) + '…' : text;
  }

  function point(p) {
    if (!p || typeof p !== 'object') return '?';
    return (p.x === undefined ? '?' : p.x) + ',' + (p.y === undefined ? '?' : p.y);
  }

  // 动作摘要：字段名严格照 docs/cua/03-action-protocol.md 第 3 节的 9 种动作，不猜未知字段。
  function actionText(action) {
    if (!action || typeof action !== 'object') return '（无动作）';
    var type = action.type || '?';
    if (type === 'click') {
      return 'click (' + point(action) + ')' +
        (action.clicks === 2 ? ' 双击' : '') +
        (action.button && action.button !== 'left' ? ' ' + action.button : '') +
        (Array.isArray(action.modifiers) && action.modifiers.length ? ' +' + action.modifiers.join('+') : '');
    }
    if (type === 'type') {
      var text = action.text === undefined || action.text === null ? '' : String(action.text);
      if (text.length > 60) text = text.slice(0, 60) + '…';
      return 'type "' + text.replace(/\n/g, '⏎') + '"' +
        (action.clear_first ? ' +clear' : '') +
        (action.submit ? ' +enter' : '');
    }
    if (type === 'hotkey') {
      return 'hotkey ' + (Array.isArray(action.keys) ? action.keys.join('+') : '?') +
        (action.repeat > 1 ? ' ×' + action.repeat : '');
    }
    if (type === 'drag') {
      return 'drag (' + point(action.from) + ')→(' + point(action.to) + ')';
    }
    if (type === 'scroll') {
      return 'scroll dx=' + (action.dx === undefined ? 0 : action.dx) +
        ' dy=' + (action.dy === undefined ? '?' : action.dy) +
        ' @(' + point(action) + ')';
    }
    if (type === 'move') return 'move (' + point(action) + ')';
    if (type === 'wait') return 'wait ' + (action.ms === undefined ? '?' : action.ms) + ' ms';
    if (type === 'screenshot') return 'screenshot ' + (action.region ? '局部' : '全屏');
    if (type === 'done') return 'done ' + (action.status || '');
    return type;
  }

  // ---------- 环境状态（契约 §2.1） ----------

  function setStat(node, text, kind) {
    if (!node) return;
    node.textContent = (text === undefined || text === null || text === '') ? '—' : String(text);
    node.className = kind ? 'cua-stat-' + kind : '';
  }

  function renderNotes(notes) {
    if (!els.statusNotes) return;
    els.statusNotes.textContent = notes.length ? notes.join(' · ') : '';
  }

  async function loadStatus() {
    if (!els.statAvailable) return;
    note(els.statNote, '');
    var st;
    try {
      st = await apiJSON('/api/cua/status');
    } catch (e) {
      state.status = null;
      setStat(els.statAvailable, '未知', 'bad');
      setStat(els.statRunning, '—', '');
      setStat(els.statPython, '—', '');
      setStat(els.statCua, '—', '');
      setStat(els.statEnv, '—', '');
      note(els.statNote, '环境状态加载失败：' + (e.message || '未知错误'), 'error');
      renderNotes([]);
      refreshButtons();
      return;
    }
    state.status = st || {};
    renderStatus(state.status);
    maybeResume(state.status);
  }

  function renderStatus(st) {
    var available = st.available === true;
    setStat(els.statAvailable, available ? '可用' : (st.available === false ? '不可用' : '未知'), available ? 'ok' : 'bad');
    setStat(els.statRunning, st.running ? '是' : '否', st.running ? 'live' : '');
    var py = st.python || {};
    setStat(els.statPython, py.found ? (py.version || '已找到') : '未找到', py.found ? 'ok' : 'bad');
    var cu = st.cua || {};
    setStat(els.statCua, cu.found ? (cu.version || '已找到') : '未找到', cu.found ? 'ok' : 'bad');
    var env = st.env || {};
    var screen = (env.screen_w && env.screen_h) ? (env.screen_w + '×' + env.screen_h) : '';
    if (screen && env.dpi_scale) screen += ' @' + env.dpi_scale + 'x';
    setStat(els.statEnv, screen || '—', '');
    fillModels(Array.isArray(st.models) ? st.models : []);
    renderNotes(Array.isArray(st.notes) ? st.notes : []);
    if (els.runId && !state.runId && st.run_id) els.runId.textContent = st.run_id;
    refreshButtons();
  }

  function readStoredModel() {
    try {
      return localStorage.getItem('cua.model') || '';
    } catch (e) {
      return '';
    }
  }

  function storeModel(name) {
    try {
      localStorage.setItem('cua.model', name || '');
    } catch (e) {}
  }

  // 契约 §2.1：models 只列支持视觉的模型；这里照单全收，不自己过滤、不自己造模型。
  function fillModels(models) {
    if (!els.model) return;
    state.models = models;
    var previous = els.model.value || readStoredModel();
    els.model.textContent = '';
    if (!models.length) {
      var none = el('option', '', '无可用视觉模型（去「模型」视图配置支持视觉的模型）');
      none.value = '';
      els.model.appendChild(none);
      state.model = '';
      refreshButtons();
      return;
    }
    models.forEach(function (m) {
      var name = m && m.name ? String(m.name) : '';
      if (!name) return;
      var opt = el('option', '', name + (m.backend ? ' · ' + m.backend : ''));
      opt.value = name;
      els.model.appendChild(opt);
    });
    var exists = models.some(function (m) {
      return m && String(m.name) === previous;
    });
    var chosen = exists ? previous : String(models[0].name || '');
    els.model.value = chosen;
    state.model = chosen;
    storeModel(chosen);
    refreshButtons();
  }

  // 刷新页面后接回正在跑的 run（契约 §2.3：run 已结束会补发历史事件）。
  function maybeResume(st) {
    if (!st || !st.running || !st.run_id) return;
    if (state.active) return;
    if (st.run_id === state.lastExitRunId) return;
    state.runId = st.run_id;
    state.replay = false;
    setReplayBadge(false);
    if (els.runId) els.runId.textContent = st.run_id;
    setRunState('运行中（已重连事件流）', 'live');
    note(els.formNote, '检测到正在运行的 run，已重新连接事件流。', '');
    openStream(st.run_id);
  }

  // ---------- 轨迹渲染 ----------

  function appendRow(node) {
    if (!els.trace) return;
    var hint = els.trace.querySelector('.list-hint');
    if (hint) hint.remove();
    var stick = els.trace.scrollHeight - els.trace.scrollTop - els.trace.clientHeight < 60;
    els.trace.appendChild(node);
    if (stick) els.trace.scrollTop = els.trace.scrollHeight;
  }

  function sysRow(text, bad) {
    appendRow(el('div', 'cua-trace-sys' + (bad ? ' cua-trace-sys-bad' : ''), text));
  }

  function metaRow(text) {
    appendRow(el('div', 'cua-trace-meta', text));
  }

  function ensureStepRow(stepNo) {
    var key = String(stepNo === undefined || stepNo === null ? '?' : stepNo);
    if (state.steps[key]) return state.steps[key];
    var row = el('div', 'cua-step');
    var head = el('div', 'cua-step-head');
    head.appendChild(el('span', 'cua-step-no', '#' + key));
    head.appendChild(el('span', 'cua-step-action', '…'));
    head.appendChild(el('span', 'cua-step-risk-tag', ''));
    head.appendChild(el('span', 'cua-step-badge cua-step-badge-wait', '待执行'));
    head.appendChild(el('span', 'cua-step-ms', ''));
    row.appendChild(head);
    var thought = el('div', 'cua-step-thought', '（等待模型输出 thought）');
    row.appendChild(thought);
    var error = el('div', 'cua-step-error', '');
    row.appendChild(error);
    appendRow(row);
    var rec = { row: row, action: head.children[1], risk: head.children[2], ok: head.children[3], dur: head.children[4], thought: thought, error: error };
    state.steps[key] = rec;
    return rec;
  }

  function updateRunSteps() {
    if (!els.runSteps) return;
    var bits = [];
    if (state.maxStep) bits.push('步 ' + state.maxStep);
    if (state.frames) bits.push('帧 ' + state.frames);
    if (state.images) bits.push('图 ' + state.images);
    els.runSteps.textContent = bits.join(' · ');
  }

  function renderShot(data) {
    if (!data || !data.data_uri || !els.shot) return;
    var img = state.shotImg;
    if (!img) {
      img = document.createElement('img');
      img.className = 'cua-shot-img';
      img.id = 'cua-shot-img';
      img.alt = 'CUA 实时截图';
      els.shot.textContent = '';
      els.shot.appendChild(img);
      state.shotImg = img;
    }
    img.src = data.data_uri;
    if (els.shotMeta) {
      var bits = [];
      if (data.step !== undefined && data.step !== null) bits.push('第 ' + data.step + ' 步');
      if (data.width && data.height) bits.push(data.width + '×' + data.height);
      if (data.media_type) bits.push(data.media_type);
      els.shotMeta.textContent = bits.join(' · ');
    }
  }

  function clearShot() {
    state.shotImg = null;
    if (els.shot) {
      els.shot.textContent = '';
      els.shot.appendChild(el('div', 'list-hint', '还没有画面。'));
    }
    if (els.shotMeta) els.shotMeta.textContent = '';
  }

  function clearTrace() {
    state.steps = {};
    if (els.trace) {
      els.trace.textContent = '';
      els.trace.appendChild(el('div', 'list-hint', '还没有轨迹。'));
    }
    note(els.traceNote, '');
  }

  // ---------- 最终报告 ----------

  function reportRow(label, value, valueClass) {
    var row = el('div', 'cua-report-row');
    row.appendChild(el('span', 'cua-report-label', label));
    var shown = (value === undefined || value === null || value === '') ? '—' : String(value);
    row.appendChild(el('span', 'cua-report-value' + (valueClass ? ' ' + valueClass : ''), shown));
    return row;
  }

  function renderReport() {
    if (!els.report) return;
    var end = state.end || {};
    var ex = state.exit || {};
    var code = end.code || ex.code || '';
    var statusText;
    var statusClass;
    if (code === 'E_CANCEL') {
      statusText = '已停止';
      statusClass = 'cua-report-warn';
    } else if (code) {
      statusText = '失败 / 已终止';
      statusClass = 'cua-report-bad';
    } else if (end.ok === true) {
      statusText = '成功';
      statusClass = 'cua-report-ok';
    } else if (end.ok === false) {
      statusText = '未成功';
      statusClass = 'cua-report-bad';
    } else {
      statusText = '已结束';
      statusClass = '';
    }
    var elapsed = (state.startedAt && state.lastTs && state.lastTs >= state.startedAt)
      ? ms((state.lastTs - state.startedAt) * 1000)
      : '—';
    els.report.textContent = '';
    els.report.appendChild(reportRow('状态', statusText, statusClass));
    els.report.appendChild(reportRow('终止码', code || '无（正常结束）', 'cua-report-mono'));
    if (end.message) els.report.appendChild(reportRow('说明', end.message));
    els.report.appendChild(reportRow('步数', end.steps === undefined ? '—' : end.steps, 'cua-report-mono'));
    els.report.appendChild(reportRow('重规划', end.replans === undefined ? '—' : end.replans, 'cua-report-mono'));
    els.report.appendChild(reportRow('耗时（首末事件）', elapsed, 'cua-report-mono'));
    els.report.appendChild(reportRow('exit_code', ex.exit_code === undefined ? '—' : ex.exit_code, 'cua-report-mono'));
    els.report.appendChild(reportRow('run_id', state.runId || '—', 'cua-report-mono'));
    els.report.appendChild(reportRow('模型', state.model || '—', 'cua-report-mono'));
    if (state.task) {
      var box = el('div', 'cua-report-task');
      box.appendChild(el('div', 'cua-report-label', '任务'));
      box.appendChild(el('div', 'cua-report-task-text', state.task));
      els.report.appendChild(box);
    }
  }

  function clearReport() {
    state.end = null;
    state.exit = null;
    if (els.report) {
      els.report.textContent = '';
      els.report.appendChild(el('div', 'list-hint', '还没有运行记录。'));
    }
  }

  function setRunState(text, kind) {
    if (!els.runStatus) return;
    els.runStatus.textContent = text;
    els.runStatus.className = 'cua-run-status' + (kind ? ' cua-run-status-' + kind : '');
  }

  // ---------- SSE（契约 §2.3） ----------

  function closeStream() {
    if (state.es) {
      try {
        state.es.close();
      } catch (e) {}
      state.es = null;
    }
  }

  // isReplay 省略时沿用当前模式：visibilitychange / maybeResume 的重连不会把回放误标成实时。
  function openStream(runId, isReplay) {
    if (isReplay === undefined) isReplay = state.replay;
    state.replay = !!isReplay;
    setReplayBadge(state.replay);
    closeStream();
    state.errCount = 0;
    state.exitSeen = false;
    state.active = true;
    var es;
    try {
      es = new EventSource('/api/cua/runs/' + encodeURIComponent(runId) + '/events');
    } catch (e) {
      state.active = false;
      note(els.traceNote, '当前浏览器不支持 EventSource，无法实时看轨迹。', 'error');
      refreshButtons();
      return;
    }
    state.es = es;
    es.addEventListener('start', function (ev) {
      onStreamEvent('start', safeParse(ev.data));
    });
    es.addEventListener('exit', function (ev) {
      onStreamEvent('exit', safeParse(ev.data));
    });
    EVENT_KINDS.forEach(function (kind) {
      es.addEventListener(kind, function (ev) {
        onStreamEvent(kind, safeParse(ev.data));
      });
    });
    es.addEventListener('error', onStreamError);
    refreshButtons();
  }

  function onStreamError() {
    if (state.exitSeen) {
      closeStream();
      return;
    }
    state.errCount += 1;
    if (state.errCount >= MAX_STREAM_ERRORS) {
      closeStream();
      state.active = false;
      setRunState('事件流断开', 'bad');
      note(els.traceNote, '事件流连续断开 ' + state.errCount + ' 次，已停止重连。CUA 进程不受影响；可点「刷新状态」或刷新页面重连。', 'error');
      refreshButtons();
    }
  }

  function onStreamEvent(kind, data) {
    state.errCount = 0;
    if (kind === 'start') {
      if (data && data.run_id) {
        state.runId = data.run_id;
        if (els.runId) els.runId.textContent = data.run_id;
      }
      if (data && data.model) state.model = data.model;
      if (data && data.task) state.task = data.task;
      return;
    }
    if (kind === 'exit') {
      state.exitSeen = true;
      state.exit = data || {};
      state.lastExitRunId = state.runId;
      renderReport();
      finishRun();
      return;
    }
    if (!data || typeof data !== 'object') return;
    if (typeof data.ts === 'number') {
      if (!state.startedAt) state.startedAt = data.ts;
      state.lastTs = data.ts;
    }
    handleKind(kind, data);
  }

  function handleKind(kind, data) {
    if (kind === 'run_start') {
      sysRow('运行开始 · 预算 ' + (data.step_budget === undefined ? '?' : data.step_budget) + ' 步 / ' +
        (data.wall_budget_s === undefined ? '?' : data.wall_budget_s) + 's · ' +
        (data.dry_run ? '演练模式 dry_run' : '真实操控') +
        (data.mock_screen ? ' · mock 屏幕' : ''));
      if (data.route) {
        metaRow('能力路由：' + (data.route.level || '?') + (data.route.reason ? ' · ' + data.route.reason : ''));
      }
      if (data.task) state.task = data.task;
      return;
    }
    if (kind === 'frame') {
      state.frames += 1;
      updateRunSteps();
      if (els.shotMeta && !state.shotImg) {
        var frameBits = ['第 ' + (data.step === undefined ? '?' : data.step) + ' 步帧'];
        if (data.label) frameBits.push(data.label);
        if (data.image && data.image.sha256) frameBits.push('sha256 ' + String(data.image.sha256).slice(0, 12));
        els.shotMeta.textContent = frameBits.join(' · ');
      }
      return;
    }
    if (kind === 'frame_image') {
      state.images += 1;
      updateRunSteps();
      renderShot(data);
      return;
    }
    if (kind === 'observation') {
      var ob = data.observation || {};
      var img = ob.image || {};
      var flags = ob.flags || {};
      metaRow('观测 #' + (data.step === undefined ? '?' : data.step) +
        ' · 图 ' + (img.width === undefined ? '?' : img.width) + '×' + (img.height === undefined ? '?' : img.height) +
        ' · 剩余 ' + (flags.steps_left === undefined ? '?' : flags.steps_left) + ' 步 / ' +
        (flags.seconds_left === undefined ? '?' : Math.round(flags.seconds_left)) + 's');
      return;
    }
    if (kind === 'step') {
      var rec = ensureStepRow(data.step);
      rec.thought.textContent = data.thought ? String(data.thought) : '（模型没有给出 thought）';
      rec.action.textContent = actionText(data.action);
      if (data.risk && data.risk !== 'low') {
        rec.risk.textContent = 'risk=' + data.risk;
        rec.row.classList.add('cua-step-risk');
      }
      // 收尾步（action.type=done）不会有 action_result（loop 只记 terminal_action_result，
      // 而 done 动作直接结束），实测会一直停在「待执行」——这里改标成终态，别让它看起来像卡住。
      if (data.action && data.action.type === 'done') {
        rec.ok.textContent = 'done';
        rec.ok.className = 'cua-step-badge cua-step-badge-done';
      }
      state.maxStep = Math.max(state.maxStep, Number(data.step) || 0);
      updateRunSteps();
      return;
    }
    if (kind === 'action_result' || kind === 'terminal_action_result') {
      var result = data.result || {};
      var rec2 = ensureStepRow(data.step);
      rec2.ok.textContent = result.ok ? 'ok' : 'error';
      rec2.ok.className = 'cua-step-badge ' + (result.ok ? 'cua-step-badge-ok' : 'cua-step-badge-bad');
      rec2.dur.textContent = ms(result.duration_ms);
      var err = result.error || null;
      if (err) {
        rec2.error.textContent = '[' + (err.code || 'E_INTERNAL') + '] ' + (err.message || '') +
          (err.hint ? '　提示：' + err.hint : '');
        rec2.error.className = 'cua-step-error';
      } else if (result.observed && result.observed.screen_changed === false) {
        rec2.error.textContent = '画面未变化（screen_changed=false）';
        rec2.error.className = 'cua-step-error cua-step-error-soft';
      }
      state.maxStep = Math.max(state.maxStep, Number(data.step) || 0);
      updateRunSteps();
      return;
    }
    if (kind === 'replan') {
      metaRow('重规划 · 第 ' + (data.step === undefined ? '?' : data.step) + ' 步 · code=' + (data.code || '?') +
        ' · 第 ' + (data.count === undefined ? '?' : data.count) + ' 次' + (data.note ? ' · ' + data.note : ''));
      return;
    }
    if (kind === 'expect_timeout') {
      metaRow('expect 未满足 · 第 ' + (data.step === undefined ? '?' : data.step) + ' 步 · ' +
        compact(data.expect));
      return;
    }
    if (kind === 'executor_retry') {
      metaRow('执行器重试 · 第 ' + (data.step === undefined ? '?' : data.step) + ' 步 · code=' + (data.code || '?') +
        (data.feedback ? ' · ' + data.feedback : ''));
      return;
    }
    if (kind === 'model_retry') {
      metaRow('模型重试 · 第 ' + (data.step === undefined ? '?' : data.step) + ' 步 · code=' + (data.code || '?') +
        ' · 退避 ' + (data.backoff_s === undefined ? '?' : data.backoff_s) + 's');
      return;
    }
    if (kind === 'guardrail') {
      metaRow('护栏拦截 · 第 ' + (data.step === undefined ? '?' : data.step) + ' 步 · ' + compact(data));
      return;
    }
    if (kind === 'agent') {
      metaRow('模型事件 · ' + (data.event || 'agent') + (data.step === undefined ? '' : ' · 第 ' + data.step + ' 步'));
      return;
    }
    if (kind === 'run_end') {
      state.end = data;
      state.lastExitRunId = state.runId;
      sysRow('运行结束 · ' + (data.code
        ? 'code=' + data.code + (data.message ? ' · ' + data.message : '')
        : 'ok=' + (data.ok === undefined ? '?' : data.ok)) +
        ' · 步数 ' + (data.steps === undefined ? '?' : data.steps) +
        ' · 重规划 ' + (data.replans === undefined ? '?' : data.replans), !!data.code);
      renderReport();
      updateRunSteps();
      return;
    }
    metaRow(kind + ' · ' + compact(data));
  }

  function finishRun() {
    state.active = false;
    closeStream();
    var code = (state.end && state.end.code) || (state.exit && state.exit.code) || '';
    if (state.replay) {
      // 回放不是实时执行：不显示「已完成」（会被读成刚跑完），也不重连；
      // 「回放」标识保留到下一次真实启动，否则用户会把历史轨迹当成正在跑的任务。
      setRunState(code ? '回放结束（' + code + '）' : '回放结束', 'replay');
      updateRunSteps();
      refreshButtons();
      loadRuns();
      return;
    }
    if (code === 'E_CANCEL') {
      setRunState('已停止', 'warn');
    } else if (code) {
      setRunState('已结束（' + code + '）', 'bad');
    } else {
      setRunState('已完成', 'ok');
    }
    updateRunSteps();
    refreshButtons();
    loadStatus();
    loadRuns();
  }

  // ---------- 启动 / 停止 ----------

  function refreshButtons() {
    var hasModel = !!(els.model && els.model.value);
    if (els.start) els.start.disabled = state.busy || state.active || !hasModel;
    // 回放的是已结束的 run：/stop 没有意义，按钮必须禁用。
    if (els.stop) els.stop.disabled = state.busy || !state.active || state.replay;
  }

  function resetRun(runId, payload) {
    state.replay = false;
    setReplayBadge(false);
    state.runId = runId || '';
    state.task = payload.task;
    state.model = payload.model;
    state.startedAt = 0;
    state.lastTs = 0;
    state.maxStep = 0;
    state.frames = 0;
    state.images = 0;
    state.exitSeen = false;
    state.errCount = 0;
    clearTrace();
    clearReport();
    clearShot();
    if (els.runId) els.runId.textContent = state.runId || '—';
    if (els.runSteps) els.runSteps.textContent = '';
    setRunState('已启动，等待事件…', 'live');
    refreshButtons();
  }

  async function onStart(ev) {
    ev.preventDefault();
    if (state.busy) {
      toast('上一个操作还没结束，请稍候');
      return;
    }
    if (state.active) {
      note(els.formNote, '已有一个 run 在跑（契约 §2.2：同一时刻只允许一个 run）。', 'error');
      return;
    }
    var task = els.task.value.trim();
    if (!task) {
      note(els.formNote, '任务不能为空。', 'error');
      els.task.focus();
      return;
    }
    if (task.length > 2000) {
      note(els.formNote, '任务不能超过 2000 字（当前 ' + task.length + ' 字）。', 'error');
      return;
    }
    var model = els.model.value;
    if (!model) {
      note(els.formNote, '没有可用的视觉模型：请先去「模型」视图配置一个支持视觉的模型。', 'error');
      return;
    }
    var maxSteps = parseInt(els.maxSteps.value, 10);
    if (!(maxSteps >= 1 && maxSteps <= 200)) {
      note(els.formNote, '最大步数必须是 1–200 的整数。', 'error');
      return;
    }
    var maxSeconds = parseInt(els.maxSeconds.value, 10);
    if (!(maxSeconds >= 10 && maxSeconds <= 3600)) {
      note(els.formNote, '最长秒数必须是 10–3600 的整数。', 'error');
      return;
    }
    var dryRun = !!(els.dryRun && els.dryRun.checked);
    var hotkeys = !!(els.hotkeys && els.hotkeys.checked);
    if (!dryRun && !window.confirm('真实操控会接管你的鼠标和键盘（dry_run=false）。确认现在开始？')) {
      note(els.formNote, '已取消：真实操控需要二次确认；勾选「演练模式」可只截屏不操作。', '');
      return;
    }
    var payload = {
      task: task,
      model: model,
      max_steps: maxSteps,
      max_seconds: maxSeconds,
      dry_run: dryRun,
      allow_dangerous_hotkeys: hotkeys
    };
    state.busy = true;
    refreshButtons();
    note(els.formNote, '启动中…');
    var body;
    try {
      body = await apiJSON('/api/cua/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
    } catch (e) {
      state.busy = false;
      refreshButtons();
      note(els.formNote, '启动失败：' + (e.message || '未知错误'), 'error');
      if (/已有任务在运行/.test(e.message || '')) loadStatus();
      return;
    }
    state.busy = false;
    var runId = body && body.run_id ? String(body.run_id) : '';
    resetRun(runId, payload);
    note(els.formNote, '已启动 run ' + (runId || '(后端未返回 run_id)') + '（' + (dryRun ? '演练模式' : '真实操控') + '）。', dryRun ? 'ok' : 'warn');
    if (runId) openStream(runId);
  }

  async function onStop() {
    if (!state.runId) {
      note(els.formNote, '当前没有在跑的 run。', 'error');
      return;
    }
    if (state.busy) return;
    if (!window.confirm('停止 run ' + state.runId + '？后端会杀掉 CUA 进程树。')) return;
    state.busy = true;
    refreshButtons();
    note(els.formNote, '正在请求停止…');
    try {
      var body = await apiJSON('/api/cua/runs/' + encodeURIComponent(state.runId) + '/stop', { method: 'POST' });
      if (body && body.stopped) {
        note(els.formNote, '已请求停止，等待 run_end / exit 事件。', 'ok');
      } else {
        note(els.formNote, '后端说该 run 已经结束了。', '');
      }
    } catch (e) {
      note(els.formNote, '停止失败：' + (e.message || '未知错误'), 'error');
    }
    state.busy = false;
    refreshButtons();
  }

  // ---------- 生命周期 ----------

  // ---------- 历史运行（契约 §2.5：GET /api/cua/runs，最近 50 条） ----------

  function pad2(n) {
    return (n < 10 ? '0' : '') + n;
  }

  function fmtClock(iso) {
    if (!iso) return '—';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso).replace('T', ' ').slice(0, 16);
    return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  }

  function fmtDuration(startIso, endIso) {
    if (!startIso || !endIso) return '';
    var a = new Date(startIso).getTime();
    var b = new Date(endIso).getTime();
    if (isNaN(a) || isNaN(b) || b < a) return '';
    var s = Math.round((b - a) / 1000);
    return s < 60 ? s + ' 秒' : Math.floor(s / 60) + ' 分 ' + (s % 60) + ' 秒';
  }

  // Go 侧只有 running / done / stopped / failed 四种（web/cua.go 的 finish）。
  function runStateMeta(status) {
    if (status === 'running') return { text: '进行中', cls: 'cua-history-state-live' };
    if (status === 'done') return { text: '成功', cls: 'cua-history-state-ok' };
    if (status === 'stopped') return { text: '已停止', cls: 'cua-history-state-warn' };
    if (status === 'failed') return { text: '失败', cls: 'cua-history-state-bad' };
    return { text: status ? String(status) : '—', cls: '' };
  }

  function setReplayBadge(on) {
    if (els.replayBadge) els.replayBadge.hidden = !on;
  }

  function renderRuns(list) {
    if (!els.historyList) return;
    var runs = Array.isArray(list) ? list : [];
    state.runs = runs;
    if (els.historyCount) els.historyCount.textContent = runs.length ? runs.length + ' 条' : '空';
    els.historyList.textContent = '';
    if (!runs.length) {
      // 契约 §2.5 的空列表就是 {"runs":[]}；历史只在内存，重启后必然为空 —— 这是正常态，不是错误。
      els.historyList.appendChild(el('div', 'list-hint', '还没有运行记录。历史只存在内存里，服务重启后会清空。'));
      return;
    }
    runs.forEach(function (r) {
      var runId = r && r.run_id ? String(r.run_id) : '';
      var st = runStateMeta(r ? r.status : '');
      var row = el('button', 'cua-history-row' + (runId && runId === state.runId ? ' cua-history-row-active' : ''));
      row.type = 'button';
      row.setAttribute('data-run-id', runId);
      var task = r && r.task ? String(r.task) : '';
      var tip = ['run_id ' + (runId || '—')];
      if (task) tip.push('任务：' + task);
      var dur = fmtDuration(r && r.started_at, r && r.ended_at);
      if (dur) tip.push('耗时 ' + dur);
      if (r && r.started_at) tip.push('开始 ' + fmtClock(r.started_at));
      if (r && r.ended_at) tip.push('结束 ' + fmtClock(r.ended_at));
      row.title = tip.join('\n');
      row.appendChild(el('span', 'cua-history-time', fmtClock(r && r.started_at)));
      row.appendChild(el('span', 'cua-history-task', task || '（无任务文本）'));
      row.appendChild(el('span', 'cua-history-model', (r && r.model) ? String(r.model) : '—'));
      row.appendChild(el('span', 'cua-history-state ' + st.cls, st.text));
      row.appendChild(el('span', 'cua-history-code', (r && r.code) ? String(r.code) : '—'));
      row.appendChild(el('span', 'cua-history-steps', (r && r.steps !== undefined && r.steps !== null) ? r.steps + ' 步' : '—'));
      els.historyList.appendChild(row);
    });
  }

  async function loadRuns() {
    if (!els.historyList) return;
    try {
      var body = await apiJSON('/api/cua/runs');
      renderRuns(body && body.runs);
      note(els.historyNote, '');
    } catch (e) {
      // 拿不到 ≠ 没有：加载失败时保留已有列表，不渲染成空态。
      note(els.historyNote, '历史加载失败：' + (e.message || '未知错误'), 'error');
    }
  }

  // 回放已结束的 run：Go 侧会先把历史事件全部补发再发 exit（契约 §2.3），
  // 所以这里复用 openStream，渲染路径与实时完全一致；截图会停在最后一帧。
  function startReplay(runId) {
    var meta = null;
    (state.runs || []).forEach(function (r) {
      if (r && String(r.run_id) === runId) meta = r;
    });
    closeStream();
    state.replay = true;
    state.runId = runId;
    state.task = meta && meta.task ? String(meta.task) : '';
    state.model = meta && meta.model ? String(meta.model) : '';
    state.startedAt = 0;
    state.lastTs = 0;
    state.maxStep = 0;
    state.frames = 0;
    state.images = 0;
    state.steps = {};
    state.end = null;
    state.exit = null;
    state.exitSeen = false;
    state.errCount = 0;
    clearTrace();
    clearReport();
    clearShot();
    if (els.runId) els.runId.textContent = runId;
    if (els.runSteps) els.runSteps.textContent = '';
    setReplayBadge(true);
    setRunState('回放中…', 'replay');
    note(els.formNote, '正在回放历史 run ' + runId + '（不是实时执行；画面停在最后一帧）。', '');
    renderRuns(state.runs);
    openStream(runId, true);
  }

  function onHistoryActivate(runId) {
    if (!runId) return;
    if (state.active && !state.replay) {
      note(els.historyNote, '当前有 run 在跑，先等它结束再回看历史。', 'error');
      return;
    }
    startReplay(runId);
  }

  function bind() {
    els = {
      statAvailable: byId('cua-stat-available'),
      statRunning: byId('cua-stat-running'),
      statPython: byId('cua-stat-python'),
      statCua: byId('cua-stat-cua'),
      statEnv: byId('cua-stat-env'),
      statNote: byId('cua-stat-note'),
      statusNotes: byId('cua-status-notes'),
      refresh: byId('cua-refresh'),
      runStatus: byId('cua-run-status'),
      runId: byId('cua-run-id'),
      runSteps: byId('cua-run-steps'),
      form: byId('cua-form'),
      task: byId('cua-task'),
      model: byId('cua-model'),
      maxSteps: byId('cua-max-steps'),
      maxSeconds: byId('cua-max-seconds'),
      dryRun: byId('cua-dry-run'),
      hotkeys: byId('cua-hotkeys'),
      start: byId('cua-start'),
      stop: byId('cua-stop'),
      formNote: byId('cua-form-note'),
      shot: byId('cua-shot'),
      shotMeta: byId('cua-shot-meta'),
      report: byId('cua-report'),
      trace: byId('cua-trace'),
      traceNote: byId('cua-trace-note'),
      replayBadge: byId('cua-replay-badge'),
      history: byId('cua-history'),
      historyList: byId('cua-history-list'),
      historyCount: byId('cua-history-count'),
      historyNote: byId('cua-history-note'),
      historyRefresh: byId('cua-history-refresh'),
      betaDialog: byId('cua-beta-dialog'),
      betaOk: byId('cua-beta-ok')
    };
    if (!els.form) return false;
    els.form.addEventListener('submit', onStart);
    if (els.stop) els.stop.addEventListener('click', onStop);
    if (els.refresh) els.refresh.addEventListener('click', function () { loadStatus(); });
    if (els.historyList) {
      // 事件委托：行是动态生成的，且行内元素被点中时要上溯到带 data-run-id 的那一行。
      els.historyList.addEventListener('click', function (ev) {
        var node = ev.target;
        while (node && node.nodeType === 1 && node !== els.historyList) {
          if (node.getAttribute && node.getAttribute('data-run-id')) {
            onHistoryActivate(node.getAttribute('data-run-id'));
            return;
          }
          node = node.parentElement;
        }
      });
    }
    if (els.historyRefresh) els.historyRefresh.addEventListener('click', function () { loadRuns(); });
    if (els.history) {
      els.history.addEventListener('toggle', function () {
        if (els.history.open) loadRuns();
      });
    }
    // 测试版弹窗：按钮关闭；close 事件覆盖按钮 / ESC / cancel 三条关闭路径。
    if (els.betaOk) els.betaOk.addEventListener('click', closeBetaDialog);
    if (els.betaDialog) els.betaDialog.addEventListener('close', onBetaClosed);
    if (els.model) {
      els.model.addEventListener('change', function () {
        state.model = els.model.value;
        storeModel(state.model);
        refreshButtons();
      });
    }
    refreshButtons();
    return true;
  }

  function ensureBound() {
    if (els.form) return true;
    return bind();
  }

  // ---- 测试版警告弹窗 ----
  // 用户要求「每次进电脑操控都弹」，所以不落 localStorage、不做「只弹一次」。
  // 触发点只有 onShow()，而 onShow() 只被 chat.js 的 showView() 在「进入视图」时调用；
  // 页签内的任何操作（刷新状态、起停 run、展开历史、回放）都不会再走 showView，因此不会重复弹。
  function openBetaDialog() {
    var dlg = els.betaDialog || byId('cua-beta-dialog');
    if (!dlg || dlg.open) return false;
    els.betaDialog = dlg;
    if (typeof dlg.showModal === 'function') {
      dlg.showModal(); // 原生模态：ESC 可关、背景 inert、焦点陷阱
    } else {
      dlg.setAttribute('open', 'open'); // 没有 showModal 的老内核兜底
    }
    if (els.betaOk) els.betaOk.focus(); // 打开时焦点移入弹窗
    return true;
  }

  function closeBetaDialog() {
    var dlg = els.betaDialog || byId('cua-beta-dialog');
    if (!dlg) return;
    if (typeof dlg.close === 'function' && dlg.open) {
      dlg.close(); // 触发 close 事件 → onBetaClosed()
    } else {
      dlg.removeAttribute('open');
      onBetaClosed();
    }
  }

  // 关闭后焦点回到页签（按钮、ESC、cancel 都汇到这里）。
  function onBetaClosed() {
    var tab = document.querySelector('.view-tab[data-view="cua"]');
    if (tab && typeof tab.focus === 'function') tab.focus();
  }

  // 每次切到电脑操控视图都重新取一次环境状态（Python/模型可能刚装好）。
  function onShow() {
    if (!ensureBound()) return;
    refreshButtons();
    loadStatus();
    loadRuns();
    openBetaDialog();
  }

  // 契约 §4：页面切走或关闭必须 close()。
  window.addEventListener('beforeunload', closeStream);
  window.addEventListener('pagehide', closeStream);

  // 切到别的视图（hash 路由）也要断开事件流。这里只做「离开 #/cua 即 close」，
  // 不参与视图分发——分发在 chat.js 的 showView()（本文件不在那里抢活）。
  // 本文件先于 chat.js 加载，所以这个监听先跑；先关流、再让 chat.js 切视图，顺序无关紧要。
  window.addEventListener('hashchange', function () {
    if (!/^#\/cua(\/|$|\?)/.test(location.hash || '')) closeStream();
  });

  // 切到别的浏览器标签页也先断开；回到本页时若 run 还在跑就重连（Go 侧会补发历史事件）。
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) {
      if (state.active && state.runId && !state.exitSeen) closeStream();
      return;
    }
    if (state.active && state.runId && !state.exitSeen && !state.es) openStream(state.runId);
  });

  window.CUA = { onShow: onShow, closeStream: closeStream };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', ensureBound);
  } else {
    ensureBound();
  }
})();
