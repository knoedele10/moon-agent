// 技能（skill）管理视图 + 「本轮自动选中技能」的展示位。
// 只依赖 templates/chat/index.html 里的固定元素 id，通过 window.SKILLS.onShow() 被 chat.js 的
// hash 路由唤起；SSE 的 skills 事件由 chat.js 转成 window.SKILLS.onAutoPicked(data)。
// 后端契约：/api/skills 的六个接口（列表 / 详情 / 新建 / 改 / 删 / 导入 / 检索）。
// 本文件只渲染后端真的返回的字段，不发明任何新字段；失败一律给可见提示，不静默。
(function () {
  'use strict';

  var state = {
    skills: [],
    query: '',
    editingId: 0,      // 0 = 新建；>0 = 正在编辑 / 查看已有技能
    viewOnly: false,   // 弹层当前是不是「只看不改」
    original: null,    // 打开时的原始快照，用来判断哪些字段真的被改了
    loading: false,
    loaded: false,
    busy: false
  };

  var els = {};
  var toastTimer = null;
  var searchTimer = null;

  function byId(id) {
    return document.getElementById(id);
  }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  // 复用页面顶部的 toast（chat.js / kb.js 也在用），没有就降级到 console。
  function toast(message) {
    var box = byId('toast');
    if (!box) {
      if (window.console) window.console.log('[skills]', message);
      return;
    }
    box.textContent = message;
    box.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      box.classList.remove('show');
    }, 2600);
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
    // DELETE 成功是 204，没有响应体，别去 json() 硬解。
    if (res.status !== 204) {
      try {
        body = await res.json();
      } catch (e) {
        body = null;
      }
    }
    if (!res.ok) {
      throw new Error((body && body.error) || ('HTTP ' + res.status));
    }
    return body;
  }

  function note(text, kind) {
    if (!els.listNote) return;
    els.listNote.textContent = text || '';
    els.listNote.className = 'kb-note' + (kind ? ' kb-note-' + kind : '');
  }

  function noteDialog(text, kind) {
    if (!els.dialogNote) return;
    els.dialogNote.textContent = text || '';
    els.dialogNote.className = 'kb-note' + (kind ? ' kb-note-' + kind : '');
  }

  function num(v) {
    return typeof v === 'number' ? v : '—';
  }

  function truncate(text, max) {
    var s = String(text || '');
    return s.length > max ? s.slice(0, max) + '…' : s;
  }

  // 只留路径尾部两级：整条 C:\Users\... 会把一行铺满，反而看不清技能来自哪里。
  function shortSource(src) {
    if (!src) return '';
    var parts = String(src).split(/[\\/]+/).filter(Boolean);
    if (!parts.length) return String(src);
    if (parts.length <= 2) return parts.join('/');
    return '…/' + parts.slice(-2).join('/');
  }

  // ---------- 列表 ----------

  function renderList() {
    var box = els.list;
    if (!box) return;
    box.textContent = '';
    if (state.loading && !state.loaded) {
      box.appendChild(el('div', 'list-hint', '加载中…'));
      return;
    }
    if (!state.skills.length) {
      box.appendChild(el('div', 'list-hint', state.query
        ? '没有匹配「' + state.query + '」的技能。'
        : '还没有技能。点「从文件夹导入」批量收编，或「新建技能」写一个。'));
      return;
    }
    state.skills.forEach(function (skill) {
      box.appendChild(skillRow(skill));
    });
  }

  function skillRow(skill) {
    var row = el('div', 'skill-row');

    var main = el('div', 'skill-main');
    var name = el('div', 'skill-name', skill.name || ('#' + skill.id));
    if (skill.enabled === false) name.classList.add('skill-name-off');
    main.appendChild(name);

    if (skill.description) {
      var desc = el('p', 'skill-desc', truncate(skill.description, 120));
      desc.title = skill.description; // 截断了就靠 title 补全，别让用户点进弹层才知道内容
      main.appendChild(desc);
    }

    var metaBits = [];
    if (typeof skill.body_runes === 'number') metaBits.push(skill.body_runes + ' 字');
    var src = shortSource(skill.source);
    if (src) metaBits.push(src);
    if (metaBits.length) {
      var meta = el('p', 'skill-meta', metaBits.join(' · '));
      if (skill.source) meta.title = skill.source;
      main.appendChild(meta);
    }
    row.appendChild(main);

    var actions = el('div', 'skill-actions');

    // 开关直接 PATCH；失败要把勾选拨回去，否则界面就在骗人。
    var toggleLabel = el('label', 'skill-toggle');
    var toggle = document.createElement('input');
    toggle.type = 'checkbox';
    toggle.checked = skill.enabled !== false;
    toggle.setAttribute('aria-label', '启用技能 ' + (skill.name || skill.id));
    var toggleText = el('span', null, toggle.checked ? '已启用' : '已停用');
    toggle.addEventListener('change', function () {
      patchEnabled(skill, toggle, toggleText);
    });
    toggleLabel.appendChild(toggle);
    toggleLabel.appendChild(toggleText);
    actions.appendChild(toggleLabel);

    var view = el('button', 'btn btn-ghost skill-btn', '查看');
    view.type = 'button';
    view.setAttribute('aria-label', '查看技能 ' + (skill.name || skill.id));
    view.addEventListener('click', function () {
      openView(skill.id);
    });
    actions.appendChild(view);

    var del = el('button', 'btn btn-danger skill-btn', '删除');
    del.type = 'button';
    del.setAttribute('aria-label', '删除技能 ' + (skill.name || skill.id));
    del.addEventListener('click', function () {
      removeSkill(skill);
    });
    actions.appendChild(del);

    row.appendChild(actions);
    return row;
  }

  async function loadSkills() {
    if (!els.list) return;
    state.loading = true;
    if (!state.loaded) renderList();
    note('', '');
    var url = '/api/skills' + (state.query ? '?q=' + encodeURIComponent(state.query) : '');
    try {
      var body = await apiJSON(url);
      state.skills = body && Array.isArray(body.skills) ? body.skills : [];
      state.loading = false;
      state.loaded = true;
      renderList();
      var total = body && typeof body.total === 'number' ? body.total : state.skills.length;
      note('共 ' + total + ' 个技能' + (state.query ? '（搜索：' + state.query + '）' : ''), '');
    } catch (e) {
      state.loading = false;
      if (!state.loaded) {
        state.skills = [];
        renderList();
      }
      note('技能列表加载失败：' + (e.message || '未知错误'), 'error');
      toast('技能列表加载失败：' + (e.message || '未知错误'));
    }
  }

  async function patchEnabled(skill, box, textEl) {
    var wanted = !!box.checked;
    box.disabled = true;
    try {
      await apiJSON('/api/skills/' + skill.id, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: wanted })
      });
      skill.enabled = wanted;
      textEl.textContent = wanted ? '已启用' : '已停用';
      toast(wanted ? '已启用「' + (skill.name || skill.id) + '」' : '已停用「' + (skill.name || skill.id) + '」');
    } catch (e) {
      box.checked = !wanted;
      toast('切换启用状态失败：' + (e.message || '未知错误'));
    } finally {
      box.disabled = false;
    }
  }

  async function removeSkill(skill) {
    var label = skill.name || ('#' + skill.id);
    if (!window.confirm('确定删除技能「' + label + '」吗？该操作不可撤销。')) return;
    try {
      await apiJSON('/api/skills/' + skill.id, { method: 'DELETE' });
      toast('已删除「' + label + '」');
      await loadSkills();
    } catch (e) {
      toast('删除失败：' + (e.message || '未知错误'));
    }
  }

  // ---------- 新建 / 查看 / 编辑弹层 ----------

  function openDialog(dlg) {
    if (!dlg) return;
    if (typeof dlg.showModal === 'function') {
      if (!dlg.open) dlg.showModal();
    } else {
      dlg.setAttribute('open', 'open'); // 老浏览器兜底，至少能显示
    }
  }

  function closeDialog(dlg) {
    if (!dlg || !dlg.open) return;
    if (typeof dlg.close === 'function') dlg.close();
    else dlg.removeAttribute('open');
  }

  function openCreate() {
    fillDialog({ id: 0, name: '', description: '', body: '', enabled: true }, false);
    openDialog(els.dialog);
    if (els.name) els.name.focus();
  }

  async function openView(id) {
    try {
      var detail = await apiJSON('/api/skills/' + id);
      fillDialog(detail || {}, true);
      openDialog(els.dialog);
    } catch (e) {
      toast('读取技能失败：' + (e.message || '未知错误'));
    }
  }

  function fillDialog(skill, viewOnly) {
    state.editingId = skill.id || 0;
    state.viewOnly = !!viewOnly;
    state.original = {
      name: skill.name || '',
      description: skill.description || '',
      body: skill.body || '',
      enabled: skill.enabled !== false
    };
    if (els.dialogTitle) {
      els.dialogTitle.textContent = state.editingId
        ? (state.viewOnly ? '技能详情' : '编辑技能')
        : '新建技能';
    }
    els.name.value = state.original.name;
    els.description.value = state.original.description;
    els.body.value = state.original.body;
    els.enabled.checked = state.original.enabled;
    setDialogMode();
    noteDialog('', '');
  }

  function setDialogMode() {
    var ro = state.viewOnly;
    // 已有技能的 name 来自 SKILL.md 的 frontmatter，PATCH 也不接受改名，所以一律只读。
    var nameLocked = ro || state.editingId > 0;
    els.name.readOnly = nameLocked;
    els.description.readOnly = ro;
    els.body.readOnly = ro;
    els.name.classList.toggle('input-readonly', nameLocked);
    els.description.classList.toggle('input-readonly', ro);
    els.body.classList.toggle('input-readonly', ro);
    els.save.hidden = ro;
    els.edit.hidden = !(ro && state.editingId > 0);
  }

  async function saveDialog() {
    if (state.busy || state.viewOnly) return; // 查看模式下表单回车也会 submit，别让它走保存
    var name = (els.name.value || '').trim();
    var description = (els.description.value || '').trim();
    var body = (els.body.value || '').trim();
    var enabled = !!els.enabled.checked;

    if (!name) {
      noteDialog('技能名不能为空。', 'error');
      els.name.focus();
      return;
    }
    if (!body) {
      noteDialog('正文不能为空。', 'error');
      els.body.focus();
      return;
    }

    state.busy = true;
    els.save.disabled = true;
    noteDialog('保存中…', '');
    try {
      if (state.editingId) {
        // PATCH 只认 {enabled} 或 {description, body} 两种形状；拆成两次发，
        // 避免后端只读其中一段时另一半被默默丢掉。
        var orig = state.original || {};
        if (enabled !== !!orig.enabled) {
          await apiJSON('/api/skills/' + state.editingId, {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled: enabled })
          });
        }
        if (description !== (orig.description || '') || body !== (orig.body || '')) {
          await apiJSON('/api/skills/' + state.editingId, {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ description: description, body: body })
          });
        }
        toast('技能「' + name + '」已保存');
      } else {
        var res = await apiJSON('/api/skills', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: name, description: description, body: body, enabled: enabled })
        });
        toast(res && res.updated
          ? '已覆盖同名技能「' + name + '」'
          : '已新建技能「' + name + '」');
      }
      closeDialog(els.dialog);
      await loadSkills();
    } catch (e) {
      noteDialog('保存失败：' + (e.message || '未知错误'), 'error');
      toast('保存技能失败：' + (e.message || '未知错误'));
    } finally {
      state.busy = false;
      els.save.disabled = false;
    }
  }

  // ---------- 导入弹层 ----------

  async function pickImportDir() {
    if (window.desktop && typeof window.desktop.pickDirectory === 'function') {
      try {
        // 桌面端走原生目录选择框，和「工作区」按钮是同一套调用。
        var picked = await window.desktop.pickDirectory();
        if (picked) els.importDir.value = picked;
      } catch (e) {
        toast('打开文件夹选择框失败：' + (e.message || '未知错误'));
      }
      return;
    }
    // 网页端没有原生目录选择框：不假装能选，直接把光标送进输入框让用户手填绝对路径。
    els.importDir.focus();
    toast('网页端无法弹出系统文件夹选择框，请手动填写绝对路径');
  }

  function setImportResult(text, kind) {
    var box = els.importResult;
    if (!box) return;
    box.textContent = '';
    box.className = 'skill-import-result' + (kind ? ' skill-import-result-' + kind : '');
    if (text) box.appendChild(el('p', 'skill-import-result-text', text));
  }

  function renderImportResult(res) {
    var box = els.importResult;
    if (!box) return;
    box.textContent = '';
    box.className = 'skill-import-result';

    var stats = el('div', 'skill-import-stats');
    [['扫描文件', res.files], ['导入', res.imported], ['新建', res.created], ['覆盖更新', res.updated]]
      .forEach(function (pair) {
        var item = el('span', 'skill-import-stat');
        item.appendChild(el('b', null, num(pair[1])));
        item.appendChild(document.createTextNode(' ' + pair[0]));
        stats.appendChild(item);
      });
    box.appendChild(stats);

    if (res.root) box.appendChild(el('p', 'skill-import-path', '目录：' + res.root));

    var dup = Array.isArray(res.duplicates) ? res.duplicates : [];
    if (dup.length) {
      var d = el('details', 'skill-import-details');
      d.appendChild(el('summary', null, '跳过重复 ' + dup.length + ' 条'));
      d.appendChild(el('div', 'skill-import-lines', dup.slice(0, 5).join('\n') + (dup.length > 5 ? '\n…' : '')));
      box.appendChild(d);
    }

    var errs = Array.isArray(res.errors) ? res.errors : [];
    if (errs.length) {
      var ed = el('details', 'skill-import-details skill-import-details-error');
      ed.open = true; // 有失败项就必须默认展开，否则用户会以为整批都成功了
      ed.appendChild(el('summary', null, '失败 ' + errs.length + ' 条'));
      ed.appendChild(el('div', 'skill-import-lines', errs.slice(0, 5).join('\n') + (errs.length > 5 ? '\n…' : '')));
      box.appendChild(ed);
    }
  }

  async function runImport() {
    if (state.busy) return;
    var dir = (els.importDir.value || '').trim();
    if (!dir) {
      setImportResult('请先填写要导入的文件夹（绝对路径）。', 'error');
      els.importDir.focus();
      return;
    }
    state.busy = true;
    els.importRun.disabled = true;
    setImportResult('导入中，目录越大越慢，请稍等…', '');
    try {
      var res = await apiJSON('/api/skills/import', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          dir: dir,
          enabled: !!els.importEnabled.checked,
          include_hidden: false
        })
      });
      renderImportResult(res || {});
      toast('导入完成：新建 ' + num(res && res.created) + '，更新 ' + num(res && res.updated));
      await loadSkills();
    } catch (e) {
      setImportResult('导入失败：' + (e.message || '未知错误'), 'error');
      toast('导入失败：' + (e.message || '未知错误'));
    } finally {
      state.busy = false;
      els.importRun.disabled = false;
    }
  }

  // ---------- 自动技能面板（SSE 的 skills 事件） ----------

  function onAutoPicked(data) {
    var box = els.auto;
    if (!box) return;
    box.textContent = '';
    var picked = data && Array.isArray(data.picked) ? data.picked : [];
    if (!picked.length) {
      // 没命中就清空并隐藏，别让上一轮的名字赖在屏幕上。
      box.hidden = true;
      return;
    }
    box.hidden = false;
    box.appendChild(el('span', 'skill-auto-label', '本轮自动匹配技能'));
    box.appendChild(el('span', 'skill-auto-list', picked.map(function (p) {
      return p.name || ('#' + p.id);
    }).join('、')));
    var sourceText = data.source === 'llm' ? '大模型重排' : (data.source === 'hybrid' ? '混合检索' : '');
    if (sourceText) box.appendChild(el('span', 'skill-auto-src', '（' + sourceText + '）'));
    if (data.error) box.appendChild(el('span', 'skill-auto-src', '降级原因：' + data.error));
  }

  // ---------- 绑定与导出 ----------

  function bind() {
    if (els.search) {
      els.search.addEventListener('input', function () {
        // 防抖 250ms：每敲一个字都打后端会把列表刷成幻灯片。
        if (searchTimer) clearTimeout(searchTimer);
        searchTimer = setTimeout(function () {
          state.query = (els.search.value || '').trim();
          loadSkills();
        }, 250);
      });
    }
    if (els.newOpen) els.newOpen.addEventListener('click', openCreate);
    if (els.importOpen) {
      els.importOpen.addEventListener('click', function () {
        openDialog(els.importDialog);
      });
    }
    if (els.importPick) els.importPick.addEventListener('click', pickImportDir);
    if (els.importRun) els.importRun.addEventListener('click', runImport);
    if (els.importClose) els.importClose.addEventListener('click', function () { closeDialog(els.importDialog); });
    if (els.importCancel) els.importCancel.addEventListener('click', function () { closeDialog(els.importDialog); });
    if (els.dialogClose) els.dialogClose.addEventListener('click', function () { closeDialog(els.dialog); });
    if (els.cancel) els.cancel.addEventListener('click', function () { closeDialog(els.dialog); });
    if (els.edit) {
      els.edit.addEventListener('click', function () {
        state.viewOnly = false;
        setDialogMode();
        if (els.description) els.description.focus();
      });
    }
    if (els.form) {
      els.form.addEventListener('submit', function (ev) {
        ev.preventDefault();
        saveDialog();
      });
    }
    if (els.dialog) {
      els.dialog.addEventListener('close', function () {
        state.editingId = 0;
        state.original = null;
        state.viewOnly = false;
      });
    }
  }

  function ensureBound() {
    if (els.list) return true;
    els.list = byId('skill-list');
    els.listNote = byId('skill-list-note');
    els.search = byId('skill-search');
    els.importOpen = byId('skill-import-open');
    els.newOpen = byId('skill-new-open');
    els.dialog = byId('skill-dialog');
    els.dialogTitle = byId('skill-dialog-title');
    els.form = byId('skill-form');
    els.name = byId('skill-name');
    els.description = byId('skill-description');
    els.body = byId('skill-body');
    els.enabled = byId('skill-enabled');
    els.save = byId('skill-save');
    els.edit = byId('skill-edit');
    els.cancel = byId('skill-cancel');
    els.dialogClose = byId('skill-dialog-close');
    els.dialogNote = byId('skill-dialog-note');
    els.importDialog = byId('skill-import-dialog');
    els.importDir = byId('skill-import-dir');
    els.importPick = byId('skill-import-pick');
    els.importEnabled = byId('skill-import-enabled');
    els.importResult = byId('skill-import-result');
    els.importRun = byId('skill-import-run');
    els.importClose = byId('skill-import-close');
    els.importCancel = byId('skill-import-cancel');
    els.auto = byId('skill-auto');
    if (!els.list) return false; // 这个页面没有技能视图（别的模板复用本文件时）就安静退出
    bind();
    return true;
  }

  async function onShow() {
    if (!ensureBound()) return;
    await loadSkills();
  }

  window.SKILLS = { onShow: onShow, onAutoPicked: onAutoPicked };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', ensureBound);
  } else {
    ensureBound();
  }
})();
