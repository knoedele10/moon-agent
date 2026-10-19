// 胶囊下拉：把 <select> 换成「按钮 + 页内浮层列表」。
//
// 为什么不直接用原生 select：它的下拉是**系统控件**，外观完全不受 CSS 控制
// （内嵌浏览器 / Electron 里尤其突兀，白色方框配系统箭头，和这套胶囊界面不搭）。
//
// 做法和顶栏的四模式胶囊一致：select 仍然是唯一状态源（chat.js 读它的 value、
// 按会话记住模式也挂在它的 change 上），只是把它藏起来，前面放一个能完全掌控外观的按钮。
// 因此后端协议、发送逻辑、任何读 select 的代码都不用改。
(function () {
  'use strict';

  var POP = null;      // 共享浮层：同一时刻只开一个
  var openState = null; // { handle, activeIndex }

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text != null) node.textContent = text;
    return node;
  }

  function chevron() {
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '2');
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    svg.setAttribute('aria-hidden', 'true');
    svg.classList.add('chipselect-chevron');
    var path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'm6 9 6 6 6-6');
    svg.appendChild(path);
    return svg;
  }

  function check() {
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('stroke-width', '2.2');
    svg.setAttribute('stroke-linecap', 'round');
    svg.setAttribute('stroke-linejoin', 'round');
    svg.setAttribute('aria-hidden', 'true');
    svg.classList.add('chipselect-check');
    var path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'M20 6 9 17l-5-5');
    svg.appendChild(path);
    return svg;
  }

  function ensurePop() {
    if (POP) return POP;
    POP = el('div', 'chipselect-pop');
    POP.setAttribute('role', 'listbox');
    POP.hidden = true;
    document.body.appendChild(POP);

    // 点到浮层和按钮以外就收起。用捕获阶段之前不行：按钮自己的 click 要先把开关做完。
    document.addEventListener('click', function (ev) {
      if (!openState) return;
      if (POP.contains(ev.target)) return;
      if (openState.handle.button.contains(ev.target)) return;
      close();
    });
    window.addEventListener('resize', close);
    // 捕获滚动：输入框那排工具条本身不滚动，但整页 / 消息区会滚。
    document.addEventListener('scroll', close, true);
    return POP;
  }

  function position(button) {
    var r = button.getBoundingClientRect();
    var popH = POP.offsetHeight || 240;
    var popW = POP.offsetWidth || 160;
    var left = Math.max(8, Math.min(r.left, window.innerWidth - popW - 8));
    POP.style.left = left + 'px';
    // 空间不够就往上翻（输入框贴着窗口底部，这是常态）。
    if (window.innerHeight - r.bottom < popH + 12 && r.top > popH + 12) {
      POP.style.top = Math.max(8, r.top - popH - 6) + 'px';
      POP.classList.add('is-above');
    } else {
      POP.style.top = Math.min(window.innerHeight - popH - 8, r.bottom + 6) + 'px';
      POP.classList.remove('is-above');
    }
  }

  function renderOptions() {
    var handle = openState.handle;
    var select = handle.select;
    POP.textContent = '';
    POP.style.minWidth = Math.max(150, handle.button.getBoundingClientRect().width) + 'px';
    for (var i = 0; i < select.options.length; i++) {
      var opt = select.options[i];
      var row = el('button', 'chipselect-opt');
      row.type = 'button';
      row.setAttribute('role', 'option');
      row.setAttribute('aria-selected', i === select.selectedIndex ? 'true' : 'false');
      row.dataset.index = String(i);
      if (i === openState.activeIndex) row.classList.add('is-active');
      var text = el('span', 'chipselect-opt-text', (opt.textContent || '').trim());
      row.appendChild(text);
      if (i === select.selectedIndex) row.appendChild(check());
      row.addEventListener('click', function () {
        choose(Number(this.dataset.index));
      });
      POP.appendChild(row);
    }
  }

  function highlight() {
    if (!openState) return;
    var rows = POP.querySelectorAll('.chipselect-opt');
    for (var i = 0; i < rows.length; i++) {
      rows[i].classList.toggle('is-active', i === openState.activeIndex);
    }
    var active = rows[openState.activeIndex];
    if (active && active.scrollIntoView) {
      try { active.scrollIntoView({ block: 'nearest' }); } catch (e) { /* 老浏览器忽略参数 */ }
    }
  }

  function open(handle) {
    ensurePop();
    if (openState && openState.handle === handle) return;
    close();
    openState = { handle: handle, activeIndex: Math.max(0, handle.select.selectedIndex) };
    renderOptions();
    POP.hidden = false;
    position(handle.button);
    handle.button.setAttribute('aria-expanded', 'true');
    handle.button.classList.add('is-open');
  }

  function close() {
    if (!openState) return;
    var handle = openState.handle;
    openState = null;
    if (handle.button) {
      handle.button.setAttribute('aria-expanded', 'false');
      handle.button.classList.remove('is-open');
    }
    if (POP) {
      POP.hidden = true;
      POP.textContent = '';
    }
  }

  function choose(index) {
    if (!openState) return;
    var handle = openState.handle;
    var opt = handle.select.options[index];
    if (!opt) return;
    handle.select.value = opt.value; // 会触发下面装的 value watcher，按钮文字随之更新
    // 走真实的 change 事件：chat.js 的 persistKind / 档位保存都挂在这上面。
    handle.select.dispatchEvent(new Event('change', { bubbles: true }));
    close();
    try { handle.button.focus(); } catch (e) { /* 忽略 */ }
  }

  function syncLabel(handle) {
    var opt = handle.select.options[handle.select.selectedIndex];
    var text = opt ? (opt.textContent || '').trim() : '';
    handle.label.textContent = text || '—';
    handle.button.title = text;
    handle.button.setAttribute('aria-label', (handle.select.getAttribute('title') || '选项') + '：' + (text || '未选择'));
  }

  // select.value 被代码直接赋值时没有任何事件可听，所以在这里劫持一次 setter：
  // 不这么做的话，模型列表异步加载完、或切会话恢复模型时，按钮文字会停在旧值上。
  function watchValue(handle) {
    try {
      var desc = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(handle.select), 'value');
      if (!desc || !desc.set) return;
      Object.defineProperty(handle.select, 'value', {
        configurable: true,
        get: function () { return desc.get.call(this); },
        set: function (v) {
          desc.set.call(this, v);
          syncLabel(handle);
        }
      });
    } catch (e) {
      /* 定义失败就退化：至少打开浮层时是准的 */
    }
  }

  function watchOptions(handle) {
    if (!window.MutationObserver) return;
    var mo = new MutationObserver(function () { syncLabel(handle); });
    mo.observe(handle.select, { childList: true, subtree: true, characterData: true });
  }

  function onKeydown(ev) {
    var handle = this.handle;
    if (!handle) return;
    var isOpen = openState && openState.handle === handle;
    if (ev.key === 'Escape' && isOpen) { ev.preventDefault(); close(); return; }
    if (ev.key === 'Tab' && isOpen) { close(); return; }
    if (ev.key === 'Enter' || ev.key === ' ') {
      ev.preventDefault();
      if (isOpen) choose(openState.activeIndex);
      else open(handle);
      return;
    }
    if (ev.key === 'ArrowDown' || ev.key === 'ArrowUp') {
      ev.preventDefault();
      if (!isOpen) { open(handle); return; }
      var delta = ev.key === 'ArrowDown' ? 1 : -1;
      var count = handle.select.options.length;
      openState.activeIndex = (openState.activeIndex + delta + count) % count;
      highlight();
    }
  }

  function enhance(select) {
    if (!select || select.dataset.chipEnhanced === '1') return null;
    select.dataset.chipEnhanced = '1';

    // 藏起来而不是 display:none：value 与 change 都还要正常用，读屏则由新按钮接管。
    select.classList.add('chipselect-native');
    select.setAttribute('tabindex', '-1');
    select.setAttribute('aria-hidden', 'true');

    var button = el('button', 'chipselect-btn');
    button.type = 'button';
    button.setAttribute('aria-haspopup', 'listbox');
    button.setAttribute('aria-expanded', 'false');
    if (select.id === 'model-select') button.classList.add('chipselect-btn-wide');
    var label = el('span', 'chipselect-label');
    button.appendChild(label);
    button.appendChild(chevron());

    select.parentNode.insertBefore(button, select.nextSibling);

    var handle = { select: select, button: button, label: label };
    button.handle = handle;
    watchValue(handle);
    watchOptions(handle);
    syncLabel(handle);

    button.addEventListener('click', function () {
      if (openState && openState.handle === handle) close();
      else open(handle);
    });
    button.addEventListener('keydown', onKeydown);

    return handle;
  }

  function init() {
    var list = document.querySelectorAll('select.composer-chip, #model-select');
    for (var i = 0; i < list.length; i++) enhance(list[i]);
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();

  window.CHIPSELECT = { init: init, enhance: enhance, close: close };
})();
