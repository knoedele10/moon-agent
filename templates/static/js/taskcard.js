// 「当前任务」浮层卡片：把 templates/chat/index.html 里的 #task-card 空容器
// 渲染成任务卡片，并在数据变化时增量刷新（只改文本 / 宽度 / 显隐，不重建节点）。
//
// 容器与脚本标签由主 agent 提供：
//   <aside class="task-card" id="task-card" hidden aria-label="当前任务"></aside>
//   <script src="/static/js/taskcard.js"></script>
//
// 页面里没有 #task-card 时（其它页面 / 旧模板 / 测试环境）静默什么都不做。
//
// 卡片还支持拖动：按住 .task-card-head 拖，位置存 localStorage['blogmy:taskcard:pos']，
// 刷新后还原、拖出视口会夹回「至少 40px 可见」。拖动只改定位，不碰收起 / 渲染 / 轮询。
(function () {
  'use strict';

  var STORAGE_KEY = 'blogmy:taskcard:collapsed';
  var POS_KEY = 'blogmy:taskcard:pos';
  var KIND_LABELS = { chat: '聊天', work: '工作', code: '代码', learn: '学习' };

  var state = {
    card: null,        // #task-card 容器
    els: null,         // 内部节点引用，避免每次渲染重新查询
    built: false,      // 结构是否已建好
    hasData: false,    // 是否收到过第一份数据（决定卡片是否 hidden）
    collapsed: false,
    data: defaultData(),
    pos: null,         // 拖动后的视口坐标 {x,y}；null = 还在 CSS 默认位置（右上角）
    drag: null,        // 拖动中的临时状态（起点 / 基准坐标 / 是否真的动过）
    team: defaultTeam() // 团队区域数据，只由 setTeam 灌入（不混进 update 的 patch）
  };

  function defaultData() {
    return {
      kind: 'work',
      done: 0,
      total: 0,
      current: '',
      workspace: '',
      recalled: 0,
      busy: false
    };
  }

  function defaultTeam() {
    return { members: [], tasks: [], parent_id: null };
  }

  function el(tag, className) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    return node;
  }

  function trim(str) {
    return String(str).replace(/^\s+|\s+$/g, '');
  }

  // 计数统一收敛为非负整数，避免上游传来 undefined / 字符串 / NaN 时渲染出乱码。
  function toCount(value) {
    var n = Number(value);
    if (!isFinite(n) || n < 0) return 0;
    return Math.floor(n);
  }

  function toPositiveInt(value) {
    var n = Number(value);
    if (isFinite(n) && n > 0 && Math.floor(n) === n) return n;
    return 0;
  }

  // ---- 收起状态持久化 ----
  // 用户把卡片收起后，刷新 / 重进对话页仍应保持收起，否则浮层会反复弹出来打扰阅读。

  function readCollapsed() {
    try {
      return localStorage.getItem(STORAGE_KEY) === '1';
    } catch (e) {
      return false;
    }
  }

  function writeCollapsed(collapsed) {
    try {
      localStorage.setItem(STORAGE_KEY, collapsed ? '1' : '0');
    } catch (e) { /* 隐私模式等场景，静默失败 */ }
  }

  // ---- 位置持久化 + 拖动 ----
  // 拖过之后卡片切到 position: fixed（.is-positioned），坐标按视口 px 存；
  // 没拖过就没有记录，卡片继续走 CSS 里的默认位置（右上角），行为与拖动前一致。

  var MIN_VISIBLE = 40;   // 视口里至少留 40px，拖出去也还能抓回来
  var FALLBACK_W = 236;   // CSS 里的卡片宽度 / 大致高度，拿不到真实尺寸时兜底
  var FALLBACK_H = 160;

  function readPos() {
    try {
      var raw = localStorage.getItem(POS_KEY);
      if (!raw) return null;
      var parsed = JSON.parse(raw);
      if (!parsed || typeof parsed !== 'object') return null;
      var x = Number(parsed.x);
      var y = Number(parsed.y);
      if (!isFinite(x) || !isFinite(y)) return null;
      return { x: Math.round(x), y: Math.round(y) };
    } catch (e) {
      return null; // 坏数据（手改 / 旧版本）当作没存过，绝不抛异常
    }
  }

  function writePos(pos) {
    try {
      if (pos) {
        localStorage.setItem(POS_KEY, JSON.stringify({ x: Math.round(pos.x), y: Math.round(pos.y) }));
      } else {
        localStorage.removeItem(POS_KEY);
      }
    } catch (e) { /* 隐私模式等场景，静默失败 */ }
  }

  function cardSize() {
    var w = 0;
    var h = 0;
    if (state.card && state.card.getBoundingClientRect) {
      var rect = state.card.getBoundingClientRect();
      if (rect) {
        w = Number(rect.width) || 0;
        h = Number(rect.height) || 0;
      }
    }
    return { w: w > 0 ? w : FALLBACK_W, h: h > 0 ? h : FALLBACK_H };
  }

  // 夹回「至少 MIN_VISIBLE 可见」的范围：左右上下四条边都不许整张卡跑出去。
  function clampPos(pos) {
    var size = cardSize();
    var maxX = window.innerWidth - MIN_VISIBLE;
    var maxY = window.innerHeight - MIN_VISIBLE;
    var minX = MIN_VISIBLE - size.w;
    var minY = MIN_VISIBLE - size.h;
    if (maxX < minX) maxX = minX;
    if (maxY < minY) maxY = minY;
    return {
      x: Math.min(Math.max(pos.x, minX), maxX),
      y: Math.min(Math.max(pos.y, minY), maxY)
    };
  }

  function applyPos(pos, persist) {
    state.pos = pos ? { x: Math.round(pos.x), y: Math.round(pos.y) } : null;
    var card = state.card;
    if (card) {
      if (state.pos) {
        card.classList.add('is-positioned');
        card.style.left = state.pos.x + 'px';
        card.style.top = state.pos.y + 'px';
      } else {
        card.classList.remove('is-positioned');
        card.style.left = '';
        card.style.top = '';
      }
    }
    if (persist) writePos(state.pos);
  }

  // 有指针事件就用指针事件（顺带覆盖触摸），否则退回鼠标事件。
  var DRAG_EVENTS = typeof window.PointerEvent === 'function'
    ? { down: 'pointerdown', move: 'pointermove', up: 'pointerup' }
    : { down: 'mousedown', move: 'mousemove', up: 'mouseup' };

  function onDragMove(e) {
    var drag = state.drag;
    if (!drag) return;
    if (e.clientX !== drag.startX || e.clientY !== drag.startY) drag.moved = true;
    applyPos(clampPos({
      x: drag.baseX + (e.clientX - drag.startX),
      y: drag.baseY + (e.clientY - drag.startY)
    }), false);
    if (e.preventDefault) e.preventDefault();
  }

  function onDragEnd() {
    var drag = state.drag;
    if (!drag) return;
    state.drag = null;
    document.removeEventListener(DRAG_EVENTS.move, onDragMove, true);
    document.removeEventListener(DRAG_EVENTS.up, onDragEnd, true);
    if (state.card && state.card.classList) state.card.classList.remove('is-dragging');
    // 只有真的拖动过才落盘：单击手柄不算移动，别把卡片从默认位置挪走。
    if (drag.moved) writePos(state.pos);
  }

  function onDragStart(e) {
    if (state.drag || !state.card) return;
    if (e.button !== undefined && e.button !== 0) return; // 只认左键 / 单指
    var target = e.target;
    // 收起按钮是卡片头部里的独立交互：按它只收起 / 展开，绝不顺带拖动。
    if (target && typeof target.closest === 'function' && target.closest('.task-card-toggle')) return;

    var base = state.pos;
    if (!base && state.card.getBoundingClientRect) {
      var rect = state.card.getBoundingClientRect();
      base = { x: rect.left, y: rect.top };
    }
    if (!base) base = { x: 0, y: 0 };

    state.drag = {
      startX: e.clientX,
      startY: e.clientY,
      baseX: base.x,
      baseY: base.y,
      moved: false
    };
    // 监听挂在 document 上：指针拖出卡片范围（甚至拖出窗口）也收得到后续事件。
    // 能捕获指针就捕获（拖出窗口也不丢事件）；失败 / 不支持时靠 document 监听兜底。
    if (typeof state.card.setPointerCapture === 'function' && e.pointerId !== undefined) {
      try { state.card.setPointerCapture(e.pointerId); } catch (err) { /* 捕获失败不影响拖动 */ }
    }
    document.addEventListener(DRAG_EVENTS.move, onDragMove, true);
    document.addEventListener(DRAG_EVENTS.up, onDragEnd, true);
    if (state.card.classList) state.card.classList.add('is-dragging');
    if (e.preventDefault) e.preventDefault();
  }

  // 窗口变小后旧坐标可能整张卡都在视口外，resize 时再夹一次（不然就再也点不到了）。
  window.addEventListener('resize', function () {
    if (!state.pos) return;
    var next = clampPos(state.pos);
    if (next.x === state.pos.x && next.y === state.pos.y) return;
    applyPos(next, true);
  });

  // ---- 结构 ----

  function build(card) {
    while (card.firstChild) card.removeChild(card.firstChild);

    var head = el('div', 'task-card-head');
    var kind = el('span', 'task-card-kind');
    kind.setAttribute('data-kind', 'work');
    kind.textContent = '工作';

    var toggle = document.createElement('button');
    toggle.type = 'button';
    toggle.className = 'task-card-toggle';
    toggle.setAttribute('aria-expanded', 'true');
    toggle.setAttribute('aria-controls', 'task-card-body');
    toggle.title = '收起 / 展开';
    toggle.setAttribute('aria-label', '收起或展开当前任务卡片');
    toggle.textContent = '⌄';
    toggle.addEventListener('click', function () {
      setCollapsed(!state.collapsed);
    });

    // 头部即拖动手柄（收起按钮在 onDragStart 里被排除）。
    head.addEventListener(DRAG_EVENTS.down, onDragStart);

    head.appendChild(kind);
    head.appendChild(toggle);

    var body = el('div', 'task-card-body');
    body.id = 'task-card-body';

    var row = el('div', 'task-card-row');
    var avatar = el('span', 'task-card-avatar');
    avatar.setAttribute('aria-hidden', 'true');
    avatar.textContent = '月';

    var heading = el('span', 'task-card-heading');
    var headingTitle = document.createElement('b');
    headingTitle.textContent = '当前任务';
    var count = el('span', 'task-card-count');
    count.textContent = '0/0 已完成';
    heading.appendChild(headingTitle);
    heading.appendChild(count);

    var live = el('span', 'task-card-live');
    live.hidden = true;
    live.textContent = '执行中';

    row.appendChild(avatar);
    row.appendChild(heading);
    row.appendChild(live);

    var divider = el('div', 'task-card-divider');

    var current = el('p', 'task-card-current');
    current.textContent = '暂无任务';

    var bar = el('div', 'task-card-bar');
    var barFill = document.createElement('i');
    bar.appendChild(barFill);

    var foot = el('p', 'task-card-foot');
    foot.textContent = '工作区：未设置';

    var memory = el('p', 'task-card-foot task-card-memory');
    memory.hidden = true;

    body.appendChild(row);
    body.appendChild(divider);
    body.appendChild(current);
    body.appendChild(bar);
    body.appendChild(foot);
    body.appendChild(memory);

    // 团队区域（v2 团队层）：默认整段隐藏，只有 setTeam 收到非空数据才出现。
    // 两个数组都空时连标题一起藏掉，不能留一个空标题挂在卡片底部。
    var team = el('div', 'task-card-team');
    team.hidden = true;
    var teamTitle = el('p', 'task-card-team-title');
    teamTitle.textContent = '团队';
    var teamMembers = el('div', 'team-member-list');
    var teamTasks = el('div', 'team-task-list');
    team.appendChild(teamTitle);
    team.appendChild(teamMembers);
    team.appendChild(teamTasks);
    body.appendChild(team);

    card.appendChild(head);
    card.appendChild(body);

    state.els = {
      kind: kind,
      toggle: toggle,
      count: count,
      live: live,
      current: current,
      barFill: barFill,
      foot: foot,
      memory: memory,
      team: team,
      teamMembers: teamMembers,
      teamTasks: teamTasks
    };
  }

  // ---- 渲染 ----

  function render() {
    if (!state.els || !state.card) return;
    var els = state.els;
    var data = state.data;

    var kind = KIND_LABELS[data.kind] ? data.kind : 'work';
    els.kind.textContent = KIND_LABELS[kind];
    els.kind.setAttribute('data-kind', kind);

    var total = toCount(data.total);
    var done = toCount(data.done);
    els.count.textContent = done + '/' + total + ' 已完成';

    els.barFill.style.width = (total > 0 ? Math.round(done / total * 100) : 0) + '%';

    var current = data.current == null ? '' : String(data.current);
    els.current.textContent = trim(current) ? current : '暂无任务';

    var workspace = data.workspace == null ? '' : String(data.workspace);
    els.foot.textContent = '工作区：' + (trim(workspace) ? workspace : '未设置');

    var recalled = toPositiveInt(data.recalled);
    if (recalled > 0) {
      els.memory.textContent = '本轮回忆：' + recalled + ' 条';
      els.memory.hidden = false;
    } else {
      els.memory.textContent = '';
      els.memory.hidden = true;
    }

    els.live.hidden = data.busy !== true;
  }

  function applyCollapsed(collapsed, persist) {
    state.collapsed = collapsed === true;

    if (state.card) {
      if (state.collapsed) {
        state.card.classList.add('is-collapsed');
      } else {
        state.card.classList.remove('is-collapsed');
      }
    }

    var toggle = state.els && state.els.toggle;
    if (toggle) {
      toggle.setAttribute('aria-expanded', state.collapsed ? 'false' : 'true');
      toggle.textContent = state.collapsed ? '⌃' : '⌄';
    }

    if (persist) writeCollapsed(state.collapsed);
  }

  // ---- 对外 API ----

  // ---- 团队区域渲染 ----
  // 数据来自 GET /api/conversations/:id/team，字段见 docs/agent/07-team.md §1.3。
  // 全部用 createElement + textContent 拼装，绝不把上游数据塞进 innerHTML。

  var TEAM_MEMBER_STATUSES = { running: 'running', inactive: 'inactive', failed: 'failed' };
  var TEAM_TASK_LABELS = { pending: '待处理', in_progress: '进行中', completed: '完成', deleted: '已删除' };

  function clearChildren(node) {
    while (node && node.firstChild) node.removeChild(node.firstChild);
  }

  function normalizeMemberStatus(value) {
    var status = trim(value == null ? '' : String(value)).toLowerCase();
    return TEAM_MEMBER_STATUSES[status] ? status : 'inactive';
  }

  function memberRow(member) {
    member = member && typeof member === 'object' ? member : {};
    var row = el('div', 'team-member-row');
    // 状态只落在 data-status 上，颜色全部由 CSS 用主题变量决定（running / inactive / failed）。
    row.setAttribute('data-status', normalizeMemberStatus(member.status));
	var depth = toCount(member.depth);
	if (depth > 0) {
	  row.classList.add('is-nested');
	  row.style.marginLeft = Math.min(depth, 4) * 11 + 'px';
	}

    var dot = el('i', 'team-member-dot');
    dot.setAttribute('aria-hidden', 'true');

    var info = el('span', 'team-member-info');
    var main = el('span', 'team-member-main');
    var name = el('span', 'team-member-name');
    name.textContent = member.name == null ? '' : String(member.name);

    var role = el('span', 'team-member-role');
    var roleKey = trim(member.role == null ? '' : String(member.role)).toLowerCase();
    if (roleKey === 'lead') {
      role.classList.add('is-lead');
      role.textContent = 'Lead';
    } else if (roleKey === 'teammate') {
      role.textContent = '成员';
	} else if (roleKey === 'subagent') {
	  role.textContent = '子 agent';
    } else {
      role.textContent = roleKey;
    }

    main.appendChild(name);
    main.appendChild(role);
	var status = el('span', 'team-member-state');
	status.textContent = normalizeMemberStatus(member.status) === 'running' ? '执行中' :
	  (normalizeMemberStatus(member.status) === 'failed' ? '失败' : '已结束');
	main.appendChild(status);
    info.appendChild(main);
    var activityText = trim(member.activity || '');
    if (!activityText && member.progress) activityText = '正在生成回复';
    if (activityText) {
      var activity = el('span', 'team-member-activity');
      activity.textContent = activityText;
      activity.title = activityText;
      info.appendChild(activity);
    }
	var progressText = trim(member.progress || '');
	if (progressText) {
	  var progress = el('span', 'team-member-progress');
	  progress.textContent = progressText;
	  progress.title = progressText;
	  info.appendChild(progress);
	}
    row.appendChild(dot);
    row.appendChild(info);
    var childId = member.child_id == null ? '' : String(member.child_id);
    if (childId && window.CHAT && typeof window.CHAT.openConversation === 'function') {
      row.classList.add('is-openable');
      row.setAttribute('role', 'button');
      row.setAttribute('tabindex', '0');
      row.title = '打开 ' + (name.textContent || '子 agent') + ' 的会话';
      var openChild = function () {
        window.CHAT.openConversation(childId, member.parent_id || state.team.parent_id, name.textContent);
      };
      row.addEventListener('click', openChild);
      row.addEventListener('keydown', function (event) {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          openChild();
        }
      });
    }
    return row;
  }

  function taskRow(task) {
    task = task && typeof task === 'object' ? task : {};
    var status = trim(task.status == null ? '' : String(task.status)).toLowerCase() || 'pending';
    var row = el('div', 'team-task-row');
    row.setAttribute('data-status', status);

    var id = el('span', 'team-task-id');
    id.textContent = task.id == null ? '' : String(task.id);

    var subject = el('span', 'team-task-subject');
    subject.textContent = task.subject == null ? '' : String(task.subject);
    // 卡片只有 236px 宽，标题超长会截断，悬浮 title 里留全文。
    if (subject.textContent) subject.title = subject.textContent;

    var badge = el('span', 'team-task-status');
    badge.textContent = TEAM_TASK_LABELS[status] || status;

    row.appendChild(id);
    row.appendChild(subject);
    row.appendChild(badge);

    var owner = task.owner == null ? '' : trim(String(task.owner));
    if (owner) {
      var ownerEl = el('span', 'team-task-owner');
      ownerEl.textContent = owner;
      row.appendChild(ownerEl);
    }
    return row;
  }

  function renderTeam() {
    if (!state.els || !state.els.team) return;
    var members = state.team && Array.isArray(state.team.members) ? state.team.members : [];
    var tasks = state.team && Array.isArray(state.team.tasks) ? state.team.tasks : [];
    var box = state.els.team;
    var memberList = state.els.teamMembers;
    var taskList = state.els.teamTasks;

    clearChildren(memberList);
    clearChildren(taskList);

    // 两个数组都空：整段隐藏（标题也不留）。
    if (!members.length && !tasks.length) {
      box.hidden = true;
      return;
    }

    members.forEach(function (member) {
      memberList.appendChild(memberRow(member));
    });
    tasks.forEach(function (task) {
      taskList.appendChild(taskRow(task));
    });

    memberList.hidden = members.length === 0;
    taskList.hidden = tasks.length === 0;
    box.hidden = false;
  }

  function init() {
    var card = document.getElementById('task-card');
    if (!card) return; // 没有容器：静默退出，绝不抛异常

    // 幂等：已构建过同一个容器就只同步收起状态，不重建节点（否则会丢焦点和展开动画）。
    if (state.built && state.card === card && card.querySelector('.task-card-body')) {
      applyCollapsed(state.collapsed, false);
      return;
    }

    state.card = card;
    build(card);
    state.built = true;
    applyCollapsed(readCollapsed(), false);
    // 拖过的位置在重建（刷新 / 换会话）后还原；存过但当前窗口放不下就夹回来。
    var saved = readPos();
    applyPos(saved ? clampPos(saved) : null, false);
    render();
    renderTeam();
    card.hidden = !state.hasData;
  }

  function update(patch) {
    if (patch && typeof patch === 'object') {
      for (var key in patch) {
        if (Object.prototype.hasOwnProperty.call(patch, key)) {
          state.data[key] = patch[key];
        }
      }
    }
    state.hasData = true;

    // 容器可能比 update 晚出现（脚本先跑 / 主 agent 还没加 aside），这里再兜一次。
    init();
    if (state.card && state.built) {
      render();
      state.card.hidden = false;
    }
  }

  function reset() {
    state.data = defaultData();
    state.hasData = false;
    if (state.card && state.built) {
      render();
      state.card.hidden = true;
    }
  }

  // 团队面板：把 {members:[], tasks:[]} 渲染成卡片底部的一段区域。
  // 传空数据 / null 都会让整段隐藏；卡片本身是否可见仍由 update 的 hasData 决定。
  function setTeam(team) {
    var next = {
      members: team && Array.isArray(team.members) ? team.members : [],
      tasks: team && Array.isArray(team.tasks) ? team.tasks : [],
      parent_id: team && team.parent_id != null ? team.parent_id : null
    };
    if (state.built && JSON.stringify(next) === JSON.stringify(state.team)) return;
    state.team = next;
    // 容器可能比 setTeam 晚出现（脚本先跑 / 主 agent 还没加 aside），这里再兜一次。
    init();
    renderTeam();
  }

  function setCollapsed(collapsed) {
    applyCollapsed(collapsed === true, true);
  }

  function isCollapsed() {
    return state.collapsed === true;
  }

  window.TASKCARD = {
    init: init,
    update: update,
    reset: reset,
    setCollapsed: setCollapsed,
    isCollapsed: isCollapsed,
    setTeam: setTeam
  };

  // 脚本可能放在 <head> 里先于 #task-card 加载，等 DOM 就绪再建结构。
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
