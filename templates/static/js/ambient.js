// Low-density moving light and dust; one capped canvas behind the workspace.
(function () {
  'use strict';
  var host = document.querySelector('.main');
  if (!host) return;
  var canvas = document.createElement('canvas');
  canvas.className = 'ambient-canvas';
  canvas.setAttribute('aria-hidden', 'true');
  host.insertBefore(canvas, host.firstChild);
  var ctx = canvas.getContext('2d');
  if (!ctx) { canvas.remove(); return; }
  var motion = window.matchMedia('(prefers-reduced-motion: reduce)');
  var width = 0, height = 0, particles = [], frame = 0, last = 0, clock = 0;
  function resize() {
    width = host.clientWidth;
    height = host.clientHeight;
    var ratio = Math.min(window.devicePixelRatio || 1, 1.5);
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
    var count = Math.min(48, Math.max(18, Math.round(width * height / 22000)));
    particles = Array.from({ length: count }, function () {
      return { x: Math.random() * width, y: Math.random() * height,
        r: .9 + Math.random() * 1.6, speed: 9 + Math.random() * 17,
        phase: Math.random() * Math.PI * 2, color: Math.floor(Math.random() * 3) };
    });
    draw(0);
  }
  function draw(dt) {
    ctx.clearRect(0, 0, width, height);
    var day = document.documentElement.dataset.theme !== 'graphite';
    if (day) {
      // Warm light fans travel slowly across the blue atmosphere.
      for (var ray = 0; ray < 3; ray++) {
        var drift = Math.sin(clock / 5200 + ray * .8) * width * .06;
        var x = width * (.12 + ray * .18) + drift;
        var light = ctx.createLinearGradient(x, 0, x + width * .32, height);
        light.addColorStop(0, 'rgba(255,221,151,.13)');
        light.addColorStop(.65, 'rgba(255,242,203,.07)');
        light.addColorStop(1, 'rgba(255,242,203,0)');
        ctx.fillStyle = light;
        ctx.beginPath();
        ctx.moveTo(x - 70, 0); ctx.lineTo(x + 15, 0);
        ctx.lineTo(x + width * .56, height); ctx.lineTo(x + width * .31, height);
        ctx.closePath(); ctx.fill();
      }
    }
    var colors = day ? ['66,122,197', '143,112,185', '174,125,47'] : ['154,193,223', '170,167,226', '173,213,201'];
    particles.forEach(function (p) {
      p.y -= p.speed * dt / 1000;
      p.x += Math.sin(clock / 2200 + p.phase) * dt * .008;
      if (p.y < -12) { p.y = height + 12; p.x = Math.random() * width; }
      if (p.x < -12) p.x = width + 12;
      if (p.x > width + 12) p.x = -12;
      var alpha = (day ? .30 : .40) + .16 * Math.sin(clock / 1500 + p.phase);
      ctx.fillStyle = 'rgba(' + colors[p.color] + ',' + alpha + ')';
      ctx.beginPath(); ctx.arc(p.x, p.y, p.r, 0, Math.PI * 2); ctx.fill();
      if (!day && p.r > 2) {
        ctx.strokeStyle = 'rgba(' + colors[p.color] + ',' + alpha * .4 + ')';
        ctx.beginPath(); ctx.moveTo(p.x - 4, p.y); ctx.lineTo(p.x + 4, p.y);
        ctx.moveTo(p.x, p.y - 4); ctx.lineTo(p.x, p.y + 4); ctx.stroke();
      }
    });
  }
  function tick(time) {
    frame = 0;
    if (document.hidden || motion.matches) return;
    if (!last || time - last >= 33) {
      var dt = last ? Math.min(80, time - last) : 0;
      last = time; clock += dt; draw(dt);
    }
    frame = requestAnimationFrame(tick);
  }
  function resume() {
    if (frame) cancelAnimationFrame(frame);
    frame = 0; last = 0;
    if (!document.hidden && !motion.matches) frame = requestAnimationFrame(tick);
    else if (motion.matches) draw(0);
  }
  document.addEventListener('visibilitychange', resume);
  motion.addEventListener('change', resume);
  new MutationObserver(function () { if (!frame) draw(0); }).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  if (window.ResizeObserver) new ResizeObserver(resize).observe(host);
  else window.addEventListener('resize', resize);
  window.addEventListener('pagehide', function () { if (frame) cancelAnimationFrame(frame); });
  window.addEventListener('pageshow', resume);
  resize(); resume();
})();
