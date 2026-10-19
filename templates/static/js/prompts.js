// 提示词库（prompt）管理视图。
//
// 后端契约：/api/prompts 的四个接口（列表 / 新建 / 改 / 删）。
// 只依赖 templates/chat/index.html 里的两个固定元素 id：
//   #prompt-list  列表容器
//   #prompt-form  表单容器（本文件自己往里塞字段）
//
// 主 agent 在切到「提示词」页签时调用 window.PROMPTS.init()。
// init 幂等：重复调用不会重复绑事件、不会重复建表单，但每次都会重新拉数据。
(function () {
  'use strict';

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

  function toast(message) {
    var box = byId('toast');
    if (!box) {
      if (window.console) window.console.log('[prompts]', message);
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

  function truncate(text, max) {
    var s = String(text || '').replace(/\s+/g, ' ').trim();
    return s.length > max ? s.slice(0, max) + '…' : s;
  }

  // ---------- 复制 ----------

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    // 旧环境兜底：临时 textarea + execCommand。桌面 Electron 与部分内嵌浏览器走这条路。
    return new Promise(function (resolve, reject) {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', 'readonly');
      ta.style.position = 'fixed';
      ta.style.top = '-1000px';
      document.body.appendChild(ta);
      ta.select();
      try {
        var ok = document.execCommand('copy');
        document.body.removeChild(ta);
        ok ? resolve() : reject(new Error('当前环境不支持复制'));
      } catch (e) {
        document.body.removeChild(ta);
        reject(e);
      }
    });
  }

  // ---------- 表单 ----------

  function addField(labelText, control) {
    var wrap = el('div', 'field');
    fieldSeq += 1;
    var id = 'prompt-field-' + fieldSeq;
    control.id = id;
    var label = el('label', null, labelText);
    label.setAttribute('for', id);
    wrap.appendChild(label);
    wrap.appendChild(control);
    return wrap;
  }

  function buildForm(host) {
    host.textContent = '';
    els.form = host;

    els.title = document.createElement('input');
    els.title.type = 'text';
    els.title.className = 'input';
    els.title.maxLength = 60;
    els.title.required = true;
    els.title.placeholder = '例如：周报模板';

    els.content = document.createElement('textarea');
    els.content.className = 'input';
    els.content.rows = 6;
    els.content.placeholder = '提示词正文，可多行';
    els.content.required = true;

    host.appendChild(el('div', 'card-tag', '新建 / 编辑提示词'));
    host.appendChild(addField('标题', els.title));
    host.appendChild(addField('内容', els.content));

    els.formNote = el('p', 'card-sub', '');
    els.formNote.setAttribute('role', 'status');
    els.formNote.setAttribute('aria-live', 'polite');
    host.appendChild(els.formNote);

    var actions = el('div', null);
    els.submit = el('button', 'btn', '新建提示词');
    els.submit.type = 'submit';
    els.submit.title = '保存当前提示词';
    els.submit.setAttribute('aria-label', '保存当前提示词');
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
    els.title.value = '';
    els.content.value = '';
    els.submit.textContent = '新建提示词';
    els.cancel.hidden = true;
    note('', '');
  }

  function fillForm(item) {
    state.editingId = item.id;
    els.title.value = item.title || '';
    els.content.value = item.content || '';
    els.submit.textContent = '保存修改';
    els.cancel.hidden = false;
    note('编辑中。', '');
    if (els.form && typeof els.form.scrollIntoView === 'function') {
      els.form.scrollIntoView({ block: 'nearest' });
    }
    els.title.focus();
  }

  async function save() {
    if (state.busy) return;
    var title = (els.title.value || '').trim();
    var content = (els.content.value || '').trim();
    if (!title) {
      note('标题不能为空。', 'error');
      els.title.focus();
      return;
    }
    if (!content) {
      note('内容不能为空。', 'error');
      els.content.focus();
      return;
    }

    state.busy = true;
    els.submit.disabled = true;
    note('保存中…', '');
    try {
      if (state.editingId) {
        await apiJSON('/api/prompts/' + state.editingId, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ title: title, content: content })
        });
        toast('已保存提示词「' + title + '」');
      } else {
        await apiJSON('/api/prompts', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ title: title, content: content })
        });
        toast('已新建提示词「' + title + '」');
      }
      resetForm();
      await load();
    } catch (e) {
      note('保存失败：' + (e.message || '未知错误'), 'error');
      toast('保存提示词失败：' + (e.message || '未知错误'));
    } finally {
      state.busy = false;
      els.submit.disabled = false;
    }
  }

  // ---------- 列表 ----------

  function promptRow(item) {
    var card = el('article', 'card');
    card.appendChild(el('div', 'card-tag', '提示词'));
    card.appendChild(el('h2', null, item.title || ('#' + item.id)));
    var preview = truncate(item.content, 160);
    var sub = el('p', 'card-sub', preview || '（空）');
    sub.title = item.content || '';
    card.appendChild(sub);

    var actions = el('div', null);

    var copy = el('button', 'btn btn-ghost', '复制');
    copy.type = 'button';
    copy.title = '复制提示词 ' + (item.title || item.id);
    copy.setAttribute('aria-label', '复制提示词 ' + (item.title || item.id));
    copy.addEventListener('click', function () {
      copyText(item.content || '').then(function () {
        toast('已复制「' + (item.title || ('#' + item.id)) + '」');
      }).catch(function (e) {
        toast('复制失败：' + (e.message || '未知错误'));
      });
    });
    actions.appendChild(copy);

    var edit = el('button', 'btn btn-outline', '编辑');
    edit.type = 'button';
    edit.title = '编辑提示词 ' + (item.title || item.id);
    edit.setAttribute('aria-label', '编辑提示词 ' + (item.title || item.id));
    edit.addEventListener('click', function () {
      fillForm(item);
    });
    actions.appendChild(edit);

    var del = el('button', 'btn btn-danger', '删除');
    del.type = 'button';
    del.title = '删除提示词 ' + (item.title || item.id);
    del.setAttribute('aria-label', '删除提示词 ' + (item.title || item.id));
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
      box.appendChild(el('div', 'list-hint', '还没有提示词。在上面写一条就能存下来。'));
      return;
    }
    state.items.forEach(function (item) {
      box.appendChild(promptRow(item));
    });
  }

  async function load() {
    if (!els.list) return;
    state.loading = true;
    if (!state.loaded) renderList();
    try {
      var body = await apiJSON('/api/prompts');
      state.items = body && Array.isArray(body.prompts) ? body.prompts : [];
      state.loading = false;
      state.loaded = true;
      renderList();
      els.listNote.textContent = '共 ' + state.items.length + ' 条提示词';
    } catch (e) {
      state.loading = false;
      if (!state.loaded) {
        state.items = [];
        renderList();
      }
      els.listNote.textContent = '提示词加载失败：' + (e.message || '未知错误');
      toast('提示词加载失败：' + (e.message || '未知错误'));
    }
  }

  async function remove(item) {
    var label = item.title || ('#' + item.id);
    if (!window.confirm('确定删除提示词「' + label + '」吗？该操作不可撤销。')) return;
    try {
      await apiJSON('/api/prompts/' + item.id, { method: 'DELETE' });
      toast('已删除「' + label + '」');
      if (state.editingId === item.id) resetForm();
      await load();
    } catch (e) {
      toast('删除失败：' + (e.message || '未知错误'));
    }
  }

  // ---------- 初始化 ----------

  function build() {
    var listHost = byId('prompt-list');
    var formHost = byId('prompt-form');
    if (!listHost || !formHost) return false;

    listHost.textContent = '';
    listHost.appendChild(el('div', 'card-tag', '提示词库'));
    listHost.appendChild(el('p', 'card-sub', '把常用的提示词存下来，用的时候一键复制。'));

    var toolbar = el('div', null);
    var refresh = el('button', 'btn btn-ghost', '刷新');
    refresh.type = 'button';
    refresh.title = '重新加载提示词列表';
    refresh.setAttribute('aria-label', '重新加载提示词列表');
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
    if (!built && !build()) return; // 这个页面没有提示词视图就安静退出
    load();
  }

  window.PROMPTS = { init: init };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () {
      if (byId('prompt-list') && byId('prompt-form')) build();
    });
  } else if (byId('prompt-list') && byId('prompt-form')) {
    build();
  }
})();
