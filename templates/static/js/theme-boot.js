// 主题要在首次绘制前落到 <html> 上，否则选了深色主题的人每次打开都会先闪一帧浅色。
// 所以这个脚本是**阻塞式**加载的（放在 head，不用 defer），而且只做一件事：
// 把 localStorage 里的选择写进 data-theme。它不碰 DOM、不绑事件。
(function () {
  'use strict';

  var THEME_KEY = 'blogmy:theme';
  var THEMES = ['azure', 'graphite'];
  var theme = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'graphite' : 'azure';

  try {
    var saved = window.localStorage.getItem(THEME_KEY);
    // 旧主题名迁移：这个浅色主题最初叫 sakura（粉的），改成蓝之后重命名为 azure。
    // 之前存过 sakura 的人不该被默默丢回默认值，所以在读取这一层直接等价掉。
    if (saved === 'sakura') saved = 'azure';
    if (THEMES.indexOf(saved) !== -1) theme = saved;
  } catch (e) {
    // 隐私模式下 localStorage 会抛异常，用默认主题继续，别让整页脚本挂掉。
  }

  document.documentElement.setAttribute('data-theme', theme);
})();
