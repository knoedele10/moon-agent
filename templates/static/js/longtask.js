// 长任务面板（docs/agent/16-task-runtime.md §6）。
// 显示目标、状态、进度、执行对话与验收结果；提供暂停、继续、取消、重试、补充信息与人工审核。
// 只依赖 /api/tasks 接口和 window.CHAT 暴露的少量方法，不读 chat.js 内部状态。
(function () {
  'use strict';

  var STATUS_TONE = {
    running: 'run', verifying: 'run', review: 'warn', waiting_human: 'warn', paused: 'muted',
    done: 'ok', failed: 'bad', canceled: 'muted', budget_exhausted: 'bad'
  };
  var ACTION_LABEL = { pause: '暂停', resume: '继续', cancel: '取消', retry: '重试' };
  var STOP_EXPLAIN = {
    created: '刚创建，等待开始', submitted: '执行器已提交，正在逐项验收', auto_continue: '上一轮没有提交验收，系统已自动续跑',
    limit_continue: '上一轮触及单轮上限，系统已自动续跑', need_human: '需要人工处理后才能继续', verify_failed: '验收有未通过项，已交回执行对话修正',
    needs_review: '自动核对已通过，剩余条件需要人工审核', verified: '全部验收条件已通过', approved: '人工审核通过',
    paused_by_user: '你暂停了任务', resumed: '已继续', canceled_by_user: '你取消了任务', human_input: '已收到补充信息，继续执行',
    review_rejected: '人工审核未通过，已交回修正', retry: '已重试', repeated_failure: '连续失败后停止',
    verify_exhausted: '多次验收未通过后停止', budget_rounds: '轮数预算耗尽', budget_tool_calls: '工具调用预算耗尽',
    budget_tokens: 'token 预算耗尽', budget_deadline: '超过截止时间'
  };
  var VERDICT = { pass: ['通过', 'ok'], fail: ['未通过', 'bad'], review: ['待人工', 'warn'] };

  var panel, listEl, detailEl, formEl, openBtn;
  var state = { open: false, view: 'list', taskId: null, timer: null, busy: false };

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }

  async function api(path, opts) {
    var res = await fetch(path, Object.assign({ credentials: 'same-origin', headers: { 'Content-Type': 'application/json' } }, opts || {}));
    if (res.status === 401) { window.location.href = '/login'; throw new Error('unauthorized'); }
    var body = null;
    try { body = await res.json(); } catch (e) {}
    if (!res.ok) throw new Error((body && body.error) || ('HTTP ' + res.status));
    return body;
  }

  function convId() {
    return window.CHAT && window.CHAT.currentConversationId ? window.CHAT.currentConversationId() : null;
  }

  function fmtTime(s) {
    if (!s) return '';
    var d = new Date(s);
    return isNaN(d) ? '' : d.toLocaleString();
  }

  function badge(status, label) {
    return el('span', 'lt-badge lt-' + (STATUS_TONE[status] || 'muted'), label || status);
  }

  function section(title) {
    var s = el('section', 'lt-section');
    s.appendChild(el('h4', null, title));
    return s;
  }

  function pre(text) { return el('div', 'lt-pre', text || '—'); }

  // ---------------------------------------------------------------- 列表
  async function renderList() {
    state.view = 'list';
    formEl.hidden = true;
    detailEl.hidden = true;
    listEl.hidden = false;
    var cid = convId();
    var q = cid ? ('?conversation_id=' + encodeURIComponent(cid)) : '';
    var body;
    try { body = await api('/api/tasks' + q); } catch (e) { listEl.textContent = '读取失败：' + e.message; return; }
    listEl.textContent = '';
    var head = el('div', 'lt-row lt-head');
    head.appendChild(el('span', 'lt-hint', cid ? '本对话关联的任务' : '全部任务'));
    var add = el('button', 'btn btn-primary', '新建长任务');
    add.type = 'button';
    add.onclick = showForm;
    head.appendChild(add);
    listEl.appendChild(head);
    var tasks = (body && body.tasks) || [];
    if (!tasks.length) {
      listEl.appendChild(el('p', 'lt-empty', '还没有长任务。复杂、需要多轮执行且有明确验收条件的工作可以交给长任务：系统会自动续跑、逐项验收，直到通过或明确停下。'));
    }
    tasks.forEach(function (t) {
      var item = el('button', 'lt-item');
      item.type = 'button';
      var top = el('div', 'lt-row');
      top.appendChild(el('strong', null, '#' + t.id + ' ' + t.title));
      top.appendChild(badge(t.status, t.status_label));
      item.appendChild(top);
      var why = t.blocker || STOP_EXPLAIN[t.stop_reason] || '';
      if (why) item.appendChild(el('div', 'lt-sub', why));
      if (t.progress) item.appendChild(el('div', 'lt-sub lt-clip', '进度：' + t.progress));
      item.appendChild(el('div', 'lt-sub', '已用 ' + t.budget.rounds + '/' + t.budget.max_rounds + ' 轮 · ' + fmtTime(t.updated_at)));
      item.onclick = function () { showDetail(t.id); };
      listEl.appendChild(item);
    });
  }

  // ---------------------------------------------------------------- 新建
  function showForm() {
    state.view = 'form';
    listEl.hidden = true;
    detailEl.hidden = true;
    formEl.hidden = false;
    formEl.reset();
    formEl.querySelector('.lt-note').textContent = '';
  }

  // 验收条件每行一条；前缀 [人工] 表示人工审核，[命令] 后接命令表示宿主运行检查命令，其余由宿主核对证据。
  function parseCriteria(text) {
    return text.split(/\r?\n/).map(function (s) { return s.trim(); }).filter(Boolean).map(function (line) {
      var m = line.match(/^\[(人工|manual)\]\s*(.+)$/i);
      if (m) return { text: m[2], method: 'manual' };
      m = line.match(/^\[(命令|cmd|command)\]\s*(.+)$/i);
      if (m) {
        var parts = m[2].trim().split(/\s+/);
        return { text: '命令通过：' + m[2].trim(), method: 'command', check: { program: parts[0], args: parts.slice(1) } };
      }
      return { text: line, method: 'auto' };
    });
  }

  async function submitForm(ev) {
    ev.preventDefault();
    var note = formEl.querySelector('.lt-note');
    var f = formEl.elements;
    var payload = {
      title: f.title.value.trim(), goal: f.goal.value.trim(), constraints: f.constraints.value.trim(),
      criteria: parseCriteria(f.criteria.value), max_rounds: Number(f.max_rounds.value) || 0,
      deadline_minutes: Number(f.deadline.value) || 0,
      conversation_id: Number(convId()) || 0,
      model: window.CHAT && window.CHAT.currentModel ? window.CHAT.currentModel() : ''
    };
    if (!payload.goal) { note.textContent = '请填写目标'; return; }
    if (!payload.criteria.length) { note.textContent = '至少写一条验收条件：没有验收条件就无法判断是否完成'; return; }
    try {
      var body = await api('/api/tasks', { method: 'POST', body: JSON.stringify(payload) });
      showDetail(body.task.id);
    } catch (e) {
      note.textContent = '创建失败：' + e.message;
    }
  }

  // ---------------------------------------------------------------- 详情
  async function showDetail(id) {
    state.view = 'detail';
    state.taskId = id;
    listEl.hidden = true;
    formEl.hidden = true;
    detailEl.hidden = false;
    await refreshDetail();
  }

  async function act(t, action, extra) {
    if (state.busy) return;
    if (action === 'cancel' && !window.confirm('取消后任务不会再启动新步骤，确定取消？')) return;
    state.busy = true;
    try {
      var payload = Object.assign({ revision: t.revision }, extra || {});
      await api('/api/tasks/' + t.id + '/' + action, { method: 'POST', body: JSON.stringify(payload) });
    } catch (e) {
      window.alert('操作失败：' + e.message);
    } finally {
      state.busy = false;
      refreshDetail();
    }
  }

  async function refreshDetail() {
    if (state.view !== 'detail' || !state.taskId) return;
    var body;
    try { body = await api('/api/tasks/' + state.taskId); } catch (e) { detailEl.textContent = '读取失败：' + e.message; return; }
    var t = body.task;
    // 用户正在输入时不重绘，避免把草稿冲掉。
    var active = document.activeElement;
    if (active && detailEl.contains(active) && active.tagName === 'TEXTAREA' && active.value) return;
    detailEl.textContent = '';

    var nav = el('div', 'lt-row lt-head');
    var back = el('button', 'btn btn-ghost', '← 任务列表');
    back.type = 'button';
    back.onclick = renderList;
    nav.appendChild(back);
    nav.appendChild(badge(t.status, t.status_label));
    detailEl.appendChild(nav);
    detailEl.appendChild(el('h3', 'lt-title', '#' + t.id + ' ' + t.title));

    // 为什么停下 / 正在做什么
    var why = t.blocker || STOP_EXPLAIN[t.stop_reason] || t.stop_reason || '';
    if (t.status === 'running' && !t.blocker) why = (STOP_EXPLAIN[t.stop_reason] || '执行中') + (t.next_run_at ? '（下一轮：' + fmtTime(t.next_run_at) + '）' : '');
    var status = el('div', 'lt-callout lt-' + (STATUS_TONE[t.status] || 'muted'));
    status.appendChild(el('strong', null, t.status_label + '：'));
    status.appendChild(document.createTextNode(why));
    if (t.last_error) status.appendChild(el('div', 'lt-sub', '最近错误：' + t.last_error));
    if (t.needs_reconcile) status.appendChild(el('div', 'lt-sub', '服务曾重启：下一轮会先核对现场再继续'));
    detailEl.appendChild(status);

    // 操作按钮
    var bar = el('div', 'lt-actions');
    (t.actions || []).forEach(function (a) {
      if (ACTION_LABEL[a]) {
        var b = el('button', 'btn' + (a === 'cancel' ? ' btn-ghost lt-danger' : ''), ACTION_LABEL[a]);
        b.type = 'button';
        b.onclick = function () {
          if (a === 'retry' && t.status === 'budget_exhausted') {
            var n = window.prompt('追加多少轮预算？', '10');
            if (n === null) return;
            act(t, a, { extra_rounds: Number(n) || 10 });
          } else act(t, a);
        };
        bar.appendChild(b);
      }
    });
    if (t.exec_conversation_id) {
      var open = el('button', 'btn btn-ghost', '打开执行对话');
      open.type = 'button';
      open.onclick = function () {
        if (window.CHAT && window.CHAT.openConversation) window.CHAT.openConversation(t.exec_conversation_id, t.main_conversation_id || null, '任务：' + t.title);
        toggle(false);
      };
      bar.appendChild(open);
    }
    detailEl.appendChild(bar);

    // 补充信息 / 人工审核
    if ((t.actions || []).indexOf('input') >= 0 || (t.actions || []).indexOf('review') >= 0) {
      var box = el('div', 'lt-input');
      var ta = el('textarea', 'input');
      ta.rows = 3;
      ta.placeholder = (t.actions.indexOf('review') >= 0) ? '审核意见（驳回时必填，会交回执行对话修正）' : '补充信息：例如已完成登录、提供所需的参数或决定';
      box.appendChild(ta);
      var row = el('div', 'lt-actions');
      if (t.actions.indexOf('input') >= 0) {
        var send = el('button', 'btn btn-primary', '提交补充并继续');
        send.type = 'button';
        send.onclick = function () { if (ta.value.trim()) act(t, 'input', { message: ta.value.trim() }); };
        row.appendChild(send);
      }
      if (t.actions.indexOf('review') >= 0) {
        var ok = el('button', 'btn btn-primary', '审核通过');
        ok.type = 'button';
        ok.onclick = function () { act(t, 'review', { approve: true, message: ta.value.trim() }); };
        var no = el('button', 'btn btn-ghost', '驳回修正');
        no.type = 'button';
        no.onclick = function () { if (!ta.value.trim()) { ta.focus(); return; } act(t, 'review', { approve: false, message: ta.value.trim() }); };
        row.appendChild(ok);
        row.appendChild(no);
      }
      box.appendChild(row);
      detailEl.appendChild(box);
    }

    var g = section('目标');
    g.appendChild(pre(t.goal));
    detailEl.appendChild(g);
    if (t.constraints) {
      var c = section('约束');
      c.appendChild(pre(t.constraints));
      detailEl.appendChild(c);
    }

    // 验收条件 + 最近验收结果
    var v = section('验收条件');
    var verdicts = {};
    if (t.verification && t.verification.items) t.verification.items.forEach(function (it) { verdicts[it.criterion] = it; });
    (t.criteria || []).forEach(function (cr) {
      var row = el('div', 'lt-crit');
      var it = verdicts[cr.id];
      var vv = it ? VERDICT[it.verdict] : null;
      row.appendChild(el('span', 'lt-badge lt-' + (vv ? vv[1] : 'muted'), vv ? vv[0] : '未验收'));
      var txt = el('div');
      txt.appendChild(el('div', null, '[' + cr.id + '] ' + cr.text));
      var how = { auto: '宿主核对证据', command: '宿主运行命令', manual: '人工审核' }[cr.method] || cr.method;
      txt.appendChild(el('div', 'lt-sub', how + (it && it.reason ? ' · ' + it.reason : '')));
      row.appendChild(txt);
      v.appendChild(row);
    });
    if (t.verification) v.appendChild(el('div', 'lt-sub', '第 ' + t.verification.attempt + ' 次验收 · ' + fmtTime(t.verification.at)));
    detailEl.appendChild(v);

    var p = section('进度');
    p.appendChild(pre(t.progress || '尚未汇报'));
    if (t.done_steps && t.done_steps.length) {
      var ul = el('ul', 'lt-steps');
      t.done_steps.forEach(function (s) { ul.appendChild(el('li', null, s)); });
      p.appendChild(ul);
    }
    var b = t.budget;
    p.appendChild(el('div', 'lt-sub', '预算：' + b.rounds + '/' + b.max_rounds + ' 轮' +
      (b.max_tool_calls ? ' · 工具 ' + b.tool_calls + '/' + b.max_tool_calls : ' · 工具 ' + b.tool_calls) +
      (b.max_tokens ? ' · token ' + b.tokens + '/' + b.max_tokens : ' · token ≈' + b.tokens) +
      (b.deadline ? ' · 截止 ' + fmtTime(b.deadline) : '') + ' · 验收 ' + t.verify_attempts + ' 次'));
    detailEl.appendChild(p);

    if (t.submission) {
      var s = section('最近一次提交');
      s.appendChild(pre(t.submission.summary));
      detailEl.appendChild(s);
    }

    var r = section('执行轮次');
    (t.rounds || []).slice(0, 10).forEach(function (rd) {
      var line = '第 ' + rd.no + ' 轮 · ' + ({ start: '开始', continue: '续跑', reconcile: '重启后核对' }[rd.trigger] || rd.trigger) +
        ' · ' + rd.status + (rd.outcome ? ' / ' + rd.outcome : '') + ' · 工具 ' + rd.tool_calls + (rd.compacted ? ' · 已压缩' : '') +
        (rd.error ? ' · ' + rd.error : '');
      r.appendChild(el('div', 'lt-sub', line));
    });
    detailEl.appendChild(r);

    var e = section('事件');
    (t.events || []).slice(0, 15).forEach(function (ev) {
      e.appendChild(el('div', 'lt-sub', fmtTime(ev.at) + ' · ' + ev.message));
    });
    detailEl.appendChild(e);
  }

  // ---------------------------------------------------------------- 面板开关与轮询
  function poll() {
    if (!state.open) return;
    if (state.view === 'detail') refreshDetail();
    else if (state.view === 'list') renderList();
  }

  function toggle(force) {
    state.open = typeof force === 'boolean' ? force : !state.open;
    panel.hidden = !state.open;
    openBtn.setAttribute('aria-expanded', String(state.open));
    if (state.timer) { clearInterval(state.timer); state.timer = null; }
    if (state.open) {
      if (state.view === 'detail') refreshDetail(); else renderList();
      state.timer = setInterval(poll, 3000);
    }
  }

  function build() {
    var actions = document.querySelector('.topbar-actions') || document.querySelector('.topbar');
    if (!actions) return;
    openBtn = el('button', 'btn btn-ghost', '长任务');
    openBtn.type = 'button';
    openBtn.id = 'longtask-open';
    openBtn.setAttribute('aria-controls', 'longtask-panel');
    openBtn.setAttribute('aria-expanded', 'false');
    openBtn.onclick = function () { toggle(); };
    var keys = document.getElementById('keys-open');
    if (keys && keys.parentNode) keys.parentNode.insertBefore(openBtn, keys);
    else actions.appendChild(openBtn);

    panel = el('aside', 'lt-panel');
    panel.id = 'longtask-panel';
    panel.hidden = true;
    panel.setAttribute('aria-label', '长任务');
    var head = el('div', 'lt-row lt-panel-head');
    head.appendChild(el('h3', null, '长任务'));
    var close = el('button', 'icon-btn', '×');
    close.type = 'button';
    close.setAttribute('aria-label', '关闭');
    close.onclick = function () { toggle(false); };
    head.appendChild(close);
    panel.appendChild(head);

    listEl = el('div', 'lt-body');
    detailEl = el('div', 'lt-body');
    formEl = document.createElement('form');
    formEl.className = 'lt-body lt-form';
    formEl.hidden = true;
    formEl.innerHTML =
      '<label>标题<input class="input" name="title" maxlength="120" placeholder="可留空"></label>' +
      '<label>目标<textarea class="input" name="goal" rows="4" required placeholder="要完成什么（尽量写原话）"></textarea></label>' +
      '<label>约束<textarea class="input" name="constraints" rows="2" placeholder="必须遵守的限制，例如只改某个目录、必须用 Go"></textarea></label>' +
      '<label>验收条件（每行一条）<textarea class="input" name="criteria" rows="4" placeholder="报告包含三个章节\n[命令] go test ./...\n[人工] 页面视觉符合预期"></textarea></label>' +
      '<div class="lt-row"><label>最多轮数<input class="input" name="max_rounds" type="number" min="1" max="500" value="20"></label>' +
      '<label>截止（分钟，可空）<input class="input" name="deadline" type="number" min="0"></label></div>' +
      '<p class="lt-sub">没有前缀的条件由宿主根据证据逐项核对；[命令] 在工作区运行，退出码 0 才算通过；[人工] 进入待审。</p>' +
      '<div class="lt-actions"><button type="submit" class="btn btn-primary">创建并开始</button><button type="button" class="btn btn-ghost lt-cancel-form">返回</button></div>' +
      '<p class="lt-note" role="status"></p>';
    formEl.addEventListener('submit', submitForm);
    formEl.querySelector('.lt-cancel-form').onclick = renderList;
    panel.appendChild(listEl);
    panel.appendChild(formEl);
    panel.appendChild(detailEl);
    document.body.appendChild(panel);
    document.addEventListener('keydown', function (ev) { if (ev.key === 'Escape' && state.open) toggle(false); });
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', build);
  else build();
})();
