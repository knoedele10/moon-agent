// 自定义助手（assistant）管理视图。
//
// 后端契约：/api/assistants 的四个接口（列表 / 新建 / 改 / 删）。
// 只依赖 templates/chat/index.html 里的两个固定元素 id：
//   #assistant-list  列表容器
//   #assistant-form  表单容器（本文件自己往里塞字段）
//
// 主 agent 在切到「助手」页签时调用 window.ASSISTANTS.init()。
// init 是幂等的：重复调用不会重复绑事件、不会重复建表单，但每次都会重新拉数据。
(function () {
  'use strict';

  var MODE_OPTIONS = [
    { value: '', label: '跟随全局' },
    { value: 'plan', label: 'plan（只列计划）' },
    { value: 'confirm', label: 'confirm（风险操作先确认）' },
    { value: 'auto', label: 'auto（自主执行）' }
  ];

  var state = { items: [], editingId: 0, loading: false, loaded: false, busy: false };
  var els = {};
  var built = false;
  var fieldSeq = 0;
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

  // 复用页面顶部的 toast（chat.js / skills.js / kb.js 也在用），没有就降级到 console。
  function toast(message) {
    var box = byId('toast');
    if (!box) {
      if (window.console) window.console.log('[assistants]', message);
      return;
    }
    box.textContent = message;
    box.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      box.classList.remove('show');
    }, 2600);
  }

  function note(text, kind) {
    if (!els.formNote) return;
    els.formNote.textContent = text || '';
    els.formNote.className = 'card-sub';
    els.formNote.style.color = kind === 'error' ? 'var(--danger)' : '';
  }

  async function apiJSON(path, options) {
    var res = await fetch(path, options);
    if (res.status === 401) {
      window.location.href = '/login';
      throw new Error('登录已过期，请重新登录');
    }
    var body = null;
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

  function modeLabel(v) {
    for (var i = 0; i < MODE_OPTIONS.length; i++) {
      if (MODE_OPTIONS[i].value === (v || '')) return MODE_OPTIONS[i].label;
    }
    return v || '跟随全局';
  }

  function truncate(text, max) {
    var s = String(text || '');
    return s.length > max ? s.slice(0, max) + '…' : s;
  }

  // ---------- 表单 ----------

  function addField(labelText, control) {
    var wrap = el('div', 'field');
    fieldSeq += 1;
    var id = 'assistant-field-' + fieldSeq;
    control.id = id;
    var label = el('label', null, labelText);
    label.setAttribute('for', id);
    wrap.appendChild(label);
    wrap.appendChild(control);
    return wrap;
  }

  function textInput(placeholder, maxLength) {
    var input = document.createElement('input');
    input.type = 'text';
    input.className = 'input';
    if (placeholder) input.placeholder = placeholder;
    if (maxLength) input.maxLength = maxLength;
    return input;
  }

  function buildForm(host) {
    host.textContent = '';
    els.form = host;

    els.name = textInput('例如：代码助手', 64);
    els.name.required = true;
    els.description = textInput('一句话说明它擅长什么', 512);
    els.model = textInput('留空则用系统默认模型');
    els.soulPersonality = document.createElement('textarea');
    els.soulPersonality.className = 'input';
    els.soulPersonality.rows = 3;
    els.soulPersonality.placeholder = '留空 = 跟随全局「人格」';
    els.soulGrowth = document.createElement('textarea');
    els.soulGrowth.className = 'input';
    els.soulGrowth.rows = 2;
    els.soulGrowth.readOnly = true;
    els.soulGrowth.placeholder = '助手的自我成长记录会显示在这里';
    els.soulVoice = document.createElement('textarea');
    els.soulVoice.className = 'input';
    els.soulVoice.rows = 3;
    els.soulVoice.placeholder = '留空 = 跟随全局「说话方式」';
    els.soulWork = document.createElement('textarea');
    els.soulWork.className = 'input';
    els.soulWork.rows = 3;
    els.soulWork.placeholder = '留空 = 跟随全局「做事方式」';

    els.mode = document.createElement('select');
    els.mode.className = 'input';
    MODE_OPTIONS.forEach(function (opt) {
      var o = document.createElement('option');
      o.value = opt.value;
      o.textContent = opt.label;
      els.mode.appendChild(o);
    });

    els.fsRoot = textInput('留空 = 跟随全局工作区，例如 D:\\my-workspace', 1024);

    host.appendChild(el('div', 'card-tag', '新建 / 编辑助手'));
    host.appendChild(addField('名字', els.name));
    host.appendChild(addField('简介', els.description));
    host.appendChild(addField('默认模型', els.model));
    host.appendChild(addField('人格', els.soulPersonality));
    host.appendChild(addField('自我成长记录（只读）', els.soulGrowth));
    host.appendChild(addField('说话方式', els.soulVoice));
    host.appendChild(addField('做事方式', els.soulWork));
    host.appendChild(addField('档位', els.mode));
    host.appendChild(addField('工作区', els.fsRoot));

    els.formNote = el('p', 'card-sub', '');
    els.formNote.setAttribute('role', 'status');
    els.formNote.setAttribute('aria-live', 'polite');
    host.appendChild(els.formNote);

    var actions = el('div', null);
    els.submit = el('button', 'btn', '新建助手');
    els.submit.type = 'submit';
    els.submit.title = '保存当前助手';
    els.submit.setAttribute('aria-label', '保存当前助手');
    actions.appendChild(els.submit);

    els.cancel = el('button', 'btn btn-ghost', '取消编辑');
    els.cancel.type = 'button';
    els.cancel.title = '取消编辑并清空表单';
    els.cancel.setAttribute('aria-label', '取消编辑并清空表单');
    els.cancel.hidden = true;
    actions.appendChild(els.cancel);
    host.appendChild(actions);

    host.addEventListener('submit', function (ev) {
      ev.preventDefault();
      save();
    });
    els.cancel.addEventListener('click', resetForm);
  }

  function resetForm() {
    state.editingId = 0;
    [els.name, els.description, els.model, els.soulPersonality, els.soulGrowth, els.soulVoice, els.soulWork, els.fsRoot].forEach(function (n) {
      n.value = '';
    });
    els.mode.value = '';
    els.submit.textContent = '新建助手';
    els.cancel.hidden = true;
    note('', '');
  }

  function fillForm(item) {
    state.editingId = item.id;
    els.name.value = item.name || '';
    els.description.value = item.description || '';
    els.model.value = item.model || '';
    els.soulPersonality.value = item.soul_personality || '';
    els.soulGrowth.value = item.soul_growth || '';
    els.soulVoice.value = item.soul_voice || '';
    els.soulWork.value = item.soul_work || '';
    els.mode.value = item.mode || '';
    els.fsRoot.value = item.fs_root || '';
    els.submit.textContent = '保存修改';
    els.cancel.hidden = false;
    note('编辑中：留空的覆盖项会回到「跟随全局」。', '');
    if (els.form && typeof els.form.scrollIntoView === 'function') {
      els.form.scrollIntoView({ block: 'nearest' });
    }
    els.name.focus();
  }

  async function save() {
    if (state.busy) return;
    var name = (els.name.value || '').trim();
    if (!name) {
      note('助手名字不能为空。', 'error');
      els.name.focus();
      return;
    }
    var payload = {
      name: name,
      description: (els.description.value || '').trim(),
      model: (els.model.value || '').trim(),
      soul_personality: els.soulPersonality.value || '',
      soul_voice: els.soulVoice.value || '',
      soul_work: els.soulWork.value || '',
      mode: els.mode.value || '',
      fs_root: (els.fsRoot.value || '').trim()
    };

    state.busy = true;
    els.submit.disabled = true;
    note('保存中…', '');
    try {
      if (state.editingId) {
        await apiJSON('/api/assistants/' + state.editingId, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        toast('已保存助手「' + name + '」');
      } else {
        await apiJSON('/api/assistants', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        toast('已新建助手「' + name + '」');
      }
      resetForm();
      await load();
    } catch (e) {
      note('保存失败：' + (e.message || '未知错误'), 'error');
      toast('保存助手失败：' + (e.message || '未知错误'));
    } finally {
      state.busy = false;
      els.submit.disabled = false;
    }
  }

  // ---------- 列表 ----------

  function chip(text) {
    var node = el('span', 'card-tag', text);
    return node;
  }

  function assistantRow(item) {
    var card = el('article', 'card');
    card.appendChild(el('div', 'card-tag', '助手'));
    card.appendChild(el('h2', null, item.name || ('#' + item.id)));
    if (item.description) {
      card.appendChild(el('p', 'card-sub', truncate(item.description, 160)));
    }

    var tags = el('div', null);
    tags.appendChild(chip('模型：' + (item.model || '默认')));
    tags.appendChild(document.createTextNode(' '));
    // 只标记「覆盖了哪些」：留空的项就是在跟随全局。
    if (item.soul_personality || item.soul_voice || item.soul_work) tags.appendChild(chip('灵魂已覆盖'));
    if (item.mode) tags.appendChild(chip('档位：' + modeLabel(item.mode)));
    if (item.fs_root) tags.appendChild(chip('独立工作区'));
    if (!item.soul_personality && !item.soul_voice && !item.soul_work && !item.mode && !item.fs_root) {
      tags.appendChild(chip('全部跟随全局'));
    }
    card.appendChild(tags);

    var actions = el('div', null);

    var toggleLabel = el('label', null);
    toggleLabel.title = '启用或停用助手';
    var toggle = document.createElement('input');
    toggle.type = 'checkbox';
    toggle.checked = item.enabled !== false;
    toggle.title = toggle.checked ? '停用该助手' : '启用该助手';
    toggle.setAttribute('aria-label', (toggle.checked ? '停用助手 ' : '启用助手 ') + (item.name || item.id));
    var toggleText = el('span', null, toggle.checked ? '已启用' : '已停用');
    toggle.addEventListener('change', function () {
      toggleEnabled(item, toggle, toggleText);
    });
    toggleLabel.appendChild(toggle);
    toggleLabel.appendChild(toggleText);
    actions.appendChild(toggleLabel);
    actions.appendChild(document.createTextNode(' '));

    var edit = el('button', 'btn btn-outline', '编辑');
    edit.type = 'button';
    edit.title = '编辑助手 ' + (item.name || item.id);
    edit.setAttribute('aria-label', '编辑助手 ' + (item.name || item.id));
    edit.addEventListener('click', function () {
      fillForm(item);
    });
    actions.appendChild(edit);

    var del = el('button', 'btn btn-danger', '删除');
    del.type = 'button';
    del.title = '删除助手 ' + (item.name || item.id);
    del.setAttribute('aria-label', '删除助手 ' + (item.name || item.id));
    del.addEventListener('click', function () {
      remove(item);
    });
    actions.appendChild(del);

    card.appendChild(actions);
    return card;
  }

  function renderList() {
    var box = els.list;
    if (!box) return;
    box.textContent = '';
    if (state.loading && !state.loaded) {
      box.appendChild(el('div', 'list-hint', '加载中…'));
      return;
    }
    if (!state.items.length) {
      box.appendChild(el('div', 'list-hint', '还没有助手。在上面填个名字就能新建一个。'));
      return;
    }
    state.items.forEach(function (item) {
      box.appendChild(assistantRow(item));
    });
  }

  async function load() {
    if (!els.list) return;
    state.loading = true;
    if (!state.loaded) renderList();
    try {
      var body = await apiJSON('/api/assistants');
      state.items = body && Array.isArray(body.assistants) ? body.assistants : [];
      state.loading = false;
      state.loaded = true;
      renderList();
      els.listNote.textContent = '共 ' + state.items.length + ' 个助手';
    } catch (e) {
      state.loading = false;
      if (!state.loaded) {
        state.items = [];
        renderList();
      }
      els.listNote.textContent = '助手列表加载失败：' + (e.message || '未知错误');
      toast('助手列表加载失败：' + (e.message || '未知错误'));
    }
  }

  async function toggleEnabled(item, box, textEl) {
    var wanted = !!box.checked;
    box.disabled = true;
    try {
      await apiJSON('/api/assistants/' + item.id, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: wanted })
      });
      item.enabled = wanted;
      textEl.textContent = wanted ? '已启用' : '已停用';
      box.title = wanted ? '停用该助手' : '启用该助手';
      toast(wanted ? '已启用「' + (item.name || item.id) + '」' : '已停用「' + (item.name || item.id) + '」');
    } catch (e) {
      box.checked = !wanted;
      toast('切换启用状态失败：' + (e.message || '未知错误'));
    } finally {
      box.disabled = false;
    }
  }

  async function remove(item) {
    var label = item.name || ('#' + item.id);
    if (!window.confirm('确定删除助手「' + label + '」吗？该操作不可撤销。')) return;
    try {
      await apiJSON('/api/assistants/' + item.id, { method: 'DELETE' });
      toast('已删除「' + label + '」');
      if (state.editingId === item.id) resetForm();
      await load();
    } catch (e) {
      toast('删除失败：' + (e.message || '未知错误'));
    }
  }

  // ---------- 初始化 ----------

  function build() {
    var listHost = byId('assistant-list');
    var formHost = byId('assistant-form');
    if (!listHost || !formHost) return false;

    listHost.textContent = '';
    listHost.appendChild(el('div', 'card-tag', '自定义助手'));
    listHost.appendChild(el('p', 'card-sub', '助手可以覆盖人格、说话方式、做事方式和档位；留空表示跟随全局设置。'));

    var toolbar = el('div', null);
    var refresh = el('button', 'btn btn-ghost', '刷新');
    refresh.type = 'button';
    refresh.title = '重新加载助手列表';
    refresh.setAttribute('aria-label', '重新加载助手列表');
    refresh.addEventListener('click', load);
    toolbar.appendChild(refresh);
    listHost.appendChild(toolbar);

    els.listNote = el('p', 'card-sub', '');
    els.listNote.setAttribute('role', 'status');
    els.listNote.setAttribute('aria-live', 'polite');
    listHost.appendChild(els.listNote);

    els.list = el('div', null);
    listHost.appendChild(els.list);

    buildForm(formHost);
    built = true;
    return true;
  }

  function init() {
    if (!built && !build()) return; // 这个页面没有助手视图就安静退出
    load();
  }

  window.ASSISTANTS = { init: init };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () {
      if (byId('assistant-list') && byId('assistant-form')) build();
    });
  } else if (byId('assistant-list') && byId('assistant-form')) {
    build();
  }
})();
