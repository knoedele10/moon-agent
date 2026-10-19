// 顶栏的四模式分段控件 ↔ 隐藏的 #kind-select 双向同步。
//
// 为什么还留着那个 select：chat.js 读它取 kind、按会话记住模式也挂在它的 change 上。
// 把它当唯一状态源、这里只做「同一份状态的另一个视图」，就不用改后端协议、发送逻辑、
// 会话切换逻辑——新增的胶囊纯粹是表现层。
(function () {
  'use strict';

  var KINDS = ['chat', 'work', 'code', 'learn'];

  // 首屏那句副文案随模式变：这是唯一一处「换模式时界面语气也跟着换」的地方。
  // 文案故意写得具体（代码模式提目录、学习模式提材料），因为它同时是新手引导。
  var HERO_SUB = {
    chat: '随便聊聊，或者把心里的事说给我听。',
    work: '把要做的事交给我，我会一步步做完，并把进度写在右上角。',
    code: '给我一个目录和需求，我来读代码、改文件、跑验证。',
    learn: '把材料放进来，我们一起读、一起整理成自己的笔记。'
  };

  var switchEl = null;
  var selectEl = null;
  var heroSub = null;
  var bound = false;

  function kindValue() {
    var v = selectEl ? selectEl.value : 'work';
    return KINDS.indexOf(v) === -1 ? 'work' : v;
  }

  function paint() {
    if (!switchEl) return;
    var value = kindValue();
    var btns = switchEl.querySelectorAll('.kind-switch-btn');
    for (var i = 0; i < btns.length; i++) {
      var active = btns[i].getAttribute('data-kind') === value;
      btns[i].classList.toggle('is-active', active);
      // aria-pressed 而不是 aria-selected：这是一组切换按钮，不是 tablist——
      // tablist 语义要求有配套的 tabpanel，这里没有，硬套反而给读屏帮倒忙。
      btns[i].setAttribute('aria-pressed', active ? 'true' : 'false');
    }
    if (heroSub && HERO_SUB[value]) heroSub.textContent = HERO_SUB[value];
  }

  // chat.js 切会话后会调它：把胶囊刷成那个会话记住的模式。
  // 允许在 init 之前被调用，所以这里按需初始化（chat.js 的启动顺序不保证）。
  function sync() {
    if (!switchEl) init();
    paint();
  }

  function onSwitchClick(ev) {
    var el = ev.target;
    while (el && el !== switchEl && !(el.getAttribute && el.getAttribute('data-kind'))) {
      el = el.parentNode;
    }
    if (!el || el === switchEl) return;
    var kind = el.getAttribute('data-kind');
    if (KINDS.indexOf(kind) === -1) return;
    selectEl.value = kind;
    // 走真实的 change 事件而不是直接改值：chat.js 的 persistKind（按会话记住模式）
    // 就挂在 change 上，这样那条链路一行都不用重写。
    selectEl.dispatchEvent(new Event('change', { bubbles: true }));
    paint();
  }

  function init() {
    switchEl = document.getElementById('kind-switch');
    selectEl = document.getElementById('kind-select');
    heroSub = document.getElementById('hero-sub');
    if (!switchEl || !selectEl || bound) {
      paint();
      return;
    }
    bound = true;
    switchEl.addEventListener('click', onSwitchClick);
    paint();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();

  window.KIND_SWITCH = { init: init, sync: sync };
})();
