// 深浅主题切换。首帧的主题由 head 里的 theme-boot.js 定好，这里只负责：
//   1. 点按钮时切换 data-theme 并记住选择；
//   2. 把按钮的 title / aria-label 同步成「当前是什么主题」。
// 两枚图标都在 DOM 里，显示哪一枚完全由 CSS 的 [data-theme] 选择器决定——
// 所以这里不换 innerHTML（换 innerHTML 会顺手把焦点和 hover 状态一起丢掉）。
(function () {
  'use strict';

  var THEME_KEY = 'blogmy:theme';
  // azure = 浅色（蓝），graphite = 深色（石墨）。名字跟着配色走，别再叫 sakura。
  var LABELS = { azure: '日间 · 晴空蓝', graphite: '夜间 · 深蓝' };

  function current() {
    var t = document.documentElement.getAttribute('data-theme');
    return LABELS[t] ? t : 'azure';
  }

  function apply(theme) {
    document.documentElement.setAttribute('data-theme', theme);
    try {
      window.localStorage.setItem(THEME_KEY, theme);
    } catch (e) {
      /* 写不了就算了，本次会话内的切换仍然有效 */
    }
    var btn = document.getElementById('theme-toggle');
    if (btn) {
      var text = '切换深浅主题（当前：' + LABELS[theme] + '）';
      btn.title = text;
      btn.setAttribute('aria-label', text);
    }
  }

  function init() {
    document.documentElement.classList.toggle('is-background', document.hidden);
    document.addEventListener('visibilitychange', function () {
      document.documentElement.classList.toggle('is-background', document.hidden);
    });
    var btn = document.getElementById('theme-toggle');
    if (!btn) return;
    // 把初始状态同步一遍：theme-boot.js 只写了属性，按钮上的文案还得补。
    apply(current());
    btn.addEventListener('click', function () {
      apply(current() === 'azure' ? 'graphite' : 'azure');
    });
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();

  window.THEME = { current: current, apply: apply, init: init };
})();
