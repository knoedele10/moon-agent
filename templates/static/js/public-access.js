(function () {
  'use strict';

  function init() {
    var desktop = window.desktop;
    var control = document.getElementById('public-access-control');
    if (!desktop || !desktop.isDesktop || !control) return;

    var toggle = document.getElementById('public-access-toggle');
    var panel = document.getElementById('public-access-panel');
    var status = document.getElementById('public-access-status');
    var url = document.getElementById('public-access-url');
    var copy = document.getElementById('public-access-copy');
    var stop = document.getElementById('public-access-stop');
    var currentState = { status: 'stopped', url: '', error: '' };
    control.hidden = false;

    function setPanelOpen(open) {
      panel.hidden = !open;
      toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    }

    function render(next) {
      currentState = next || currentState;
      var connected = currentState.status === 'connected' && !!currentState.url;
      var connecting = currentState.status === 'connecting';
      toggle.disabled = connecting;
      toggle.textContent = connecting ? '连接中…' : connected ? '公网已开启' : '公网访问';
      status.dataset.state = currentState.status;
      status.textContent = connected
        ? '链接已就绪，复制后即可分享。'
        : connecting
          ? '正在连接公网隧道，请稍候…'
          : currentState.status === 'error'
            ? (currentState.error || '连接失败，请重试。')
            : '启动后会生成临时链接。';
      url.value = connected ? currentState.url : '';
      url.placeholder = connected ? '' : '尚未启动';
      copy.disabled = !connected;
      stop.hidden = !connected;
      if (currentState.status === 'error') setPanelOpen(true);
    }

    toggle.addEventListener('click', async function () {
      if (currentState.status === 'connected') {
        setPanelOpen(panel.hidden);
        return;
      }
      setPanelOpen(true);
      render({ status: 'connecting', url: '', error: '' });
      try {
        render(await desktop.startPublicAccess());
      } catch (err) {
        render({ status: 'error', url: '', error: err && err.message ? err.message : '连接失败，请重试。' });
      }
    });

    copy.addEventListener('click', async function () {
      if (!currentState.url) return;
      try {
        var copied = await desktop.copyPublicAccessUrl();
        status.textContent = copied ? '链接已复制到剪贴板。' : '复制失败，请重试。';
      } catch (_) {
        status.textContent = '复制失败，请重试。';
      }
    });

    stop.addEventListener('click', async function () {
      stop.disabled = true;
      try {
        render(await desktop.stopPublicAccess());
        setPanelOpen(false);
      } catch (err) {
        render({ status: 'error', url: '', error: err && err.message ? err.message : '停止失败。' });
      } finally {
        stop.disabled = false;
      }
    });

    if (typeof desktop.onPublicAccessState === 'function') {
      desktop.onPublicAccessState(render);
    }
    desktop.getPublicAccessState().then(render).catch(function () {});
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
