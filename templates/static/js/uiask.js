// 页内对话框：替掉 window.prompt / window.confirm。
//
// 为什么必须替掉 prompt：**Electron 从来没实现过它**（调用只会往控制台打
// "prompt() is and will not be supported" 然后返回 null），所以桌面端和内嵌浏览器里
// 「改工作目录」「重命名会话」点下去就什么都不发生——用户看到的就是卡住。
// confirm 在 Electron 里能用，但它是系统弹窗，和这套界面完全不搭，顺手一起收进来。
//
// 用法：
//   var v = await UIASK.prompt({ title: '工作目录', label: '绝对路径', value: '/x', placeholder: 'D:\\work' });
//   if (v === null) return;            // 取消
//   var ok = await UIASK.confirm({ title: '删除会话？', message: '不可撤销', danger: true });
(function () {
  'use strict';

  var dlg = null;
  var parts = null;
  var pending = null; // { resolve, mode, settled }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text != null) node.textContent = text;
    return node;
  }

  function build() {
    if (dlg) return;
    dlg = el('dialog', 'uiask-dialog');
    dlg.setAttribute('role', 'dialog');
    dlg.setAttribute('aria-modal', 'true');

    var form = el('form', 'uiask-form');
    form.setAttribute('method', 'dialog');

    var tag = el('p', 'card-tag', 'CONFIRM');
    var title = el('h2', 'uiask-title');
    var message = el('p', 'uiask-message');

    var field = el('label', 'uiask-field');
    var label = el('span', 'uiask-label');
    var input = el('input', 'input uiask-input');
    input.type = 'text';
    input.setAttribute('autocomplete', 'off');
    input.setAttribute('spellcheck', 'false');
    field.appendChild(label);
    field.appendChild(input);

    var note = el('p', 'kb-note uiask-note');

    var actions = el('div', 'uiask-actions');
    var cancel = el('button', 'btn btn-ghost uiask-cancel', '取消');
    cancel.type = 'button';
    var ok = el('button', 'btn uiask-ok', '确定');
    ok.type = 'submit';
    actions.appendChild(cancel);
    actions.appendChild(ok);

    form.appendChild(tag);
    form.appendChild(title);
    form.appendChild(message);
    form.appendChild(field);
    form.appendChild(note);
    form.appendChild(actions);
    dlg.appendChild(form);
    document.body.appendChild(dlg);

    parts = { tag: tag, title: title, message: message, field: field, label: label, input: input, note: note, cancel: cancel, ok: ok };

    // 取消：按钮 / ESC / 点击背景 都走同一条结算路径。
    cancel.addEventListener('click', function () { settle(null); });
    dlg.addEventListener('cancel', function (ev) {
      ev.preventDefault(); // 不让浏览器自己关，统一由 settle() 关，避免漏掉 resolve
      settle(null);
    });
    dlg.addEventListener('close', function () { settle(null); });
    // 点 dialog 自身的空白区（不是表单内部）也算取消。
    dlg.addEventListener('click', function (ev) {
      if (ev.target === dlg) settle(null);
    });
    form.addEventListener('submit', function () {
      if (pending && pending.mode === 'prompt') {
        settle(parts.input.value);
      } else {
        settle(true);
      }
    });
  }

  // jsdom 没实现 showModal/close（会直接抛），所以两条路都留：抛了就退化成 open 属性。
  function open() {
    if (typeof dlg.showModal === 'function') {
      try {
        dlg.showModal();
        return;
      } catch (e) {
        /* 落到下面 */
      }
    }
    dlg.setAttribute('open', '');
  }

  function close() {
    if (typeof dlg.close === 'function') {
      try {
        dlg.close();
        return;
      } catch (e) {
        /* 落到下面 */
      }
    }
    dlg.removeAttribute('open');
  }

  function settle(value) {
    if (!pending || pending.settled) return;
    pending.settled = true;
    var resolve = pending.resolve;
    var mode = pending.mode;
    pending = null;
    close();
    resolve(mode === 'prompt' ? (value == null ? null : String(value)) : value === true);
  }

  function ask(mode, opts) {
    build();
    var o = opts || {};
    parts.tag.textContent = o.tag || (mode === 'prompt' ? 'INPUT' : 'CONFIRM');
    parts.title.textContent = o.title || (mode === 'prompt' ? '请输入' : '确认');
    parts.message.textContent = o.message || '';
    parts.message.hidden = !o.message;

    var isPrompt = mode === 'prompt';
    parts.field.hidden = !isPrompt;
    if (isPrompt) {
      parts.label.textContent = o.label || '';
      parts.label.hidden = !o.label;
      parts.input.value = o.value == null ? '' : String(o.value);
      parts.input.placeholder = o.placeholder || '';
    }

    parts.note.textContent = o.note || '';
    parts.note.hidden = !o.note;
    parts.ok.textContent = o.okText || (isPrompt ? '保存' : '确定');
    parts.cancel.textContent = o.cancelText || '取消';
    parts.ok.className = 'btn uiask-ok' + (o.danger ? ' btn-danger' : '');

    return new Promise(function (resolve) {
      pending = { resolve: resolve, mode: mode, settled: false };
      open();
      if (isPrompt) {
        // 聚焦并选中原值：改路径时最常见的操作是整段替换。
        try {
          parts.input.focus();
          parts.input.select();
        } catch (e) { /* 忽略 */ }
      } else {
        try { parts.ok.focus(); } catch (e) { /* 忽略 */ }
      }
    });
  }

  window.UIASK = {
    prompt: function (opts) { return ask('prompt', opts); },
    confirm: function (opts) { return ask('confirm', opts); }
  };
})();
