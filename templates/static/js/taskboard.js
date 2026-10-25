(function () {
  'use strict';

  var state = { initialized: false, timer: null, snapshot: null, request: 0 };
  var labels = { running: '执行中', inactive: '已结束', failed: '失败', pending: '待处理', in_progress: '进行中', completed: '已完成', deleted: '已删除' };

  function byId(id) { return document.getElementById(id); }
  function text(value, fallback) {
    var s = value == null ? '' : String(value).trim();
    return s || (fallback || '');
  }
  function clear(node) { if (node) node.textContent = ''; }
  function node(tag, cls, value) {
    var el = document.createElement(tag);
    if (cls) el.className = cls;
    if (value != null) el.textContent = value;
    return el;
  }
  function statusLabel(value) { return labels[String(value || '').toLowerCase()] || text(value, '未知'); }
  function escapeSelector(value) { return String(value || '').replace(/["\\]/g, '\\$&'); }

  function currentSnapshot() {
    if (window.CHAT && typeof window.CHAT.getTaskSnapshot === 'function') {
      try { return window.CHAT.getTaskSnapshot() || {}; } catch (e) {}
    }
    var active = document.querySelector('.conv-item.active');
    var messages = [];
    document.querySelectorAll('#thread .msg').forEach(function (item) {
      var role = item.getAttribute('data-role') || 'assistant';
      var content = item.getAttribute('data-raw-content') || item.querySelector('.bubble') && item.querySelector('.bubble').textContent || '';
      messages.push({ role: role, content: content });
    });
    var tools = [];
    document.querySelectorAll('#thread .tool-card').forEach(function (item) {
      tools.push({ name: item.querySelector('.tool-card-name') && item.querySelector('.tool-card-name').textContent, status: item.classList.contains('is-error') ? 'failed' : (item.classList.contains('is-running') ? 'running' : 'completed'), summary: item.querySelector('.tool-card-summary') && item.querySelector('.tool-card-summary').textContent });
    });
    return { id: active && active.dataset && active.dataset.id || null, title: active && active.textContent || '', messages: messages, tools: tools };
  }

  function renderContent(snapshot) {
    var box = byId('taskboard-content');
    if (!box) return;
    clear(box);
    var list = Array.isArray(snapshot.messages) ? snapshot.messages.filter(function (m) { return m && text(m.content); }) : [];
    if (!list.length) { box.appendChild(node('div', 'taskboard-empty', '当前会话还没有可展示的内容。')); return; }
    list.slice(-6).forEach(function (message, index) {
      var item = node('article', 'taskboard-message ' + (message.role === 'user' ? 'is-user' : 'is-assistant'));
      var head = node('div', 'taskboard-message-head');
      head.appendChild(node('span', 'taskboard-message-role', message.role === 'user' ? '我' : 'AI'));
      head.appendChild(node('span', 'taskboard-message-index', '#' + (list.length - list.slice(-6).length + index + 1)));
      item.appendChild(head);
      item.appendChild(node('p', 'taskboard-message-body', text(message.content).slice(0, 720)));
      box.appendChild(item);
    });
    var meta = byId('taskboard-content-meta');
    if (meta) meta.textContent = list.length + ' 条消息';
  }

  function renderPlan(team, snapshot) {
    var box = byId('taskboard-plan');
    if (!box) return;
    clear(box);
    var tasks = Array.isArray(team.tasks) ? team.tasks.filter(function (t) { return t && t.status !== 'deleted'; }) : [];
    if (!tasks.length) {
      var tools = Array.isArray(snapshot.tools) ? snapshot.tools : [];
      if (tools.length) {
        tasks = tools.map(function (tool, i) { return { id: 'step-' + (i + 1), subject: text(tool.name, '工具执行'), status: tool.status === 'failed' ? 'failed' : (tool.status === 'running' ? 'in_progress' : 'completed'), owner: '' }; });
      }
    }
    if (!tasks.length) { box.appendChild(node('div', 'taskboard-empty', '任务板会在 agent 创建计划后显示。')); return; }
    var done = tasks.filter(function (t) { return t.status === 'completed'; }).length;
    tasks.forEach(function (task, index) {
      var row = node('div', 'taskboard-plan-row is-' + (task.status || 'pending'));
      var rail = node('span', 'taskboard-plan-rail');
      rail.appendChild(node('i', 'taskboard-plan-dot'));
      row.appendChild(rail);
      var body = node('div', 'taskboard-plan-body');
      var title = node('div', 'taskboard-plan-title');
      title.appendChild(node('b', '', text(task.subject, '未命名任务')));
      title.appendChild(node('span', 'taskboard-status status-' + (task.status || 'pending'), statusLabel(task.status)));
      body.appendChild(title);
      var detail = text(task.description || task.owner, '步骤 ' + (index + 1));
      body.appendChild(node('p', 'taskboard-plan-detail', detail.slice(0, 180)));
      row.appendChild(body);
      box.appendChild(row);
    });
    var meta = byId('taskboard-plan-meta');
    if (meta) meta.textContent = done + ' / ' + tasks.length + ' 已完成';
    var progress = byId('taskboard-progress');
    if (progress) progress.textContent = Math.round(done / tasks.length * 100) + '%';
    var note = byId('taskboard-progress-note');
    if (note) note.textContent = done + ' / ' + tasks.length + ' 个步骤';
  }

  function renderTeam(team) {
    var box = byId('taskboard-team');
    if (!box) return;
    clear(box);
    var members = Array.isArray(team.members) ? team.members : [];
    if (!members.length) { box.appendChild(node('div', 'taskboard-empty', '当前会话尚未组建 agent 团队。')); return; }
    members.forEach(function (member) {
      var row = node('article', 'taskboard-member-row is-' + (member.status || 'inactive'));
      var avatar = node('span', 'taskboard-member-avatar', text(member.name, 'A').slice(0, 1).toUpperCase());
      var body = node('div', 'taskboard-member-body');
      var head = node('div', 'taskboard-member-head');
      head.appendChild(node('b', '', text(member.name, '未命名 agent')));
      head.appendChild(node('span', 'taskboard-status status-' + (member.status || 'inactive'), statusLabel(member.status)));
      body.appendChild(head);
      body.appendChild(node('p', 'taskboard-member-role', text(member.role === 'lead' ? 'Lead' : member.description || member.role, '团队成员')));
      if (text(member.activity || member.progress)) body.appendChild(node('p', 'taskboard-member-activity', text(member.activity || member.progress)));
      row.append(avatar, body);
      box.appendChild(row);
    });
    var count = byId('taskboard-member-count');
    if (count) count.textContent = members.length;
    var note = byId('taskboard-member-note');
    if (note) note.textContent = members.filter(function (m) { return m.status === 'running'; }).length + ' 位正在工作';
    var meta = byId('taskboard-team-meta');
    if (meta) meta.textContent = members.length + ' 位成员';
  }

  function renderActivity(snapshot) {
    var box = byId('taskboard-activity');
    if (!box) return;
    clear(box);
    var tools = Array.isArray(snapshot.tools) ? snapshot.tools : [];
    if (!tools.length) {
      document.querySelectorAll('#thread .tool-card').forEach(function (item) {
        tools.push({ name: item.querySelector('.tool-card-name') && item.querySelector('.tool-card-name').textContent, status: item.classList.contains('is-error') ? 'failed' : (item.classList.contains('is-running') ? 'running' : 'completed'), summary: item.querySelector('.tool-card-summary') && item.querySelector('.tool-card-summary').textContent });
      });
    }
    if (!tools.length) { box.appendChild(node('div', 'taskboard-empty', '工具调用和记忆写入会显示在这里。')); return; }
    tools.slice(-12).forEach(function (tool, index) {
      var row = node('div', 'taskboard-activity-row is-' + (tool.status || 'completed'));
      row.appendChild(node('span', 'taskboard-activity-index', String(index + 1).padStart(2, '0')));
      var body = node('div', 'taskboard-activity-body');
      body.appendChild(node('b', '', text(tool.name, '系统事件')));
      body.appendChild(node('p', '', text(tool.summary, statusLabel(tool.status))));
      row.appendChild(body);
      row.appendChild(node('span', 'taskboard-status status-' + (tool.status || 'completed'), statusLabel(tool.status)));
      box.appendChild(row);
    });
    var count = byId('taskboard-tool-count');
    if (count) count.textContent = tools.length;
    var note = byId('taskboard-tool-note');
    if (note) note.textContent = tools.filter(function (t) { return t.status === 'completed'; }).length + ' 次已完成';
    var meta = byId('taskboard-activity-meta');
    if (meta) meta.textContent = tools.length + ' 条记录';
  }

  async function refresh() {
    if (state.request) return;
    state.request = 1;
    var snapshot = currentSnapshot();
    var team = { members: [], tasks: [] };
    if (snapshot.id) {
      try {
        var response = await fetch('/api/conversations/' + encodeURIComponent(snapshot.id) + '/team');
        if (response.ok) team = await response.json();
      } catch (e) {}
    }
    state.snapshot = snapshot;
    renderContent(snapshot);
    renderPlan(team || {}, snapshot);
    renderTeam(team || {});
    renderActivity(snapshot);
    var stages = document.querySelectorAll('#view-taskboard .taskboard-stage-strip span');
    if (stages.length) {
      var stageIndex = snapshot.streaming ? (Array.isArray(snapshot.tools) && snapshot.tools.length ? 1 : 0) : (Array.isArray(snapshot.tools) && snapshot.tools.length ? 2 : 0);
      for (var si = 0; si < stages.length; si++) stages[si].classList.toggle('is-active', si === stageIndex);
    }
    var title = byId('taskboard-title');
    // 没有选中会话时不要拿顶栏标题冒充任务名：切到任务板时顶栏写的是「任务可视化」，
    // 直接显示会变成「当前任务：任务可视化」，像是任务就叫这个名字。
    if (title) title.textContent = snapshot.id ? text(snapshot.title, '暂无任务') : '暂无任务';
    var context = byId('taskboard-context');
    if (context) context.textContent = snapshot.id ? ('会话 #' + snapshot.id) : '选择一个会话开始执行';
    var live = byId('taskboard-live');
    if (live) {
      var running = (team.members || []).some(function (m) { return m.status === 'running'; }) || (snapshot.streaming === true);
      live.classList.toggle('is-live', running);
      live.innerHTML = '<i aria-hidden="true"></i>' + (running ? '正在执行' : (snapshot.id ? '已同步' : '等待任务'));
    }
    state.request = 0;
  }

  function startPolling() {
    if (state.timer) return;
    refresh();
    state.timer = setInterval(refresh, 3500);
  }
  function stopPolling() {
    if (state.timer) clearInterval(state.timer);
    state.timer = null;
  }
  function init() {
    if (state.initialized) return;
    state.initialized = true;
    var button = byId('taskboard-refresh');
    if (button) button.addEventListener('click', refresh);
    window.addEventListener('hashchange', function () {
      if ((location.hash || '') === '#/taskboard') startPolling(); else stopPolling();
    });
    if ((location.hash || '') === '#/taskboard') startPolling();
  }
  window.TASKBOARD = { init: init, onShow: startPolling, refresh: refresh, stop: stopPolling };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init); else init();
})();
