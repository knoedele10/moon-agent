/* fx.js — 宇宙级背景动效引擎
 * 包含：星云粒子星座连线、视差星野、极光呼吸、流星划破、点击粒子迸发+环形波纹
 * 所有效果在 prefers-reduced-motion 下自动静默 */
(function () {
  'use strict';

  var reduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  /* ===================== 工具函数 ===================== */
  function rand(min, max) { return min + Math.random() * (max - min); }
  function dist(a, b) { var dx = a.x - b.x, dy = a.y - b.y; return Math.sqrt(dx * dx + dy * dy); }

  var DPR = Math.min(window.devicePixelRatio || 1, 2);
  var W = 0, H = 0;

  /* ===================== 背景画布 ===================== */
  var bgCanvas = document.createElement('canvas');
  bgCanvas.className = 'fx-bg';
  bgCanvas.setAttribute('aria-hidden', 'true');
  document.body.appendChild(bgCanvas);
  var ctx = bgCanvas.getContext('2d');

  /* ---------- 星野层（三层视差 + 星座连线） ---------- */
  var LAYERS = [
    { count: 0.08,  rMin: 0.3, rMax: 0.7, speed: 0.004, alpha: 0.35, conn: false },
    { count: 0.035, rMin: 0.7, rMax: 1.3, speed: 0.012, alpha: 0.65, conn: true  },
    { count: 0.012, rMin: 1.2, rMax: 2.2, speed: 0.024, alpha: 1.0,  conn: true  }
  ];
  var stars = [];
  var meteors = [];
  var auroraBands = [];
  var pointer = { x: 0.5, y: 0.5, tx: 0.5, ty: 0.5 };

  /* ---------- 巨大行星系统 ---------- */
  var planets = [];
  function buildPlanets() {
    planets = [
      {
        // 紫色巨行星 — 左上方，带环
        x: W * 0.12, y: H * 0.18, r: Math.min(W, H) * 0.14,
        color: [139, 92, 246], darkColor: [60, 30, 120],
        glowColor: [139, 92, 246], glowAlpha: 0.12,
        hasRing: true, ringColor: [180, 140, 255],
        parallax: 3, phase: 0
      },
      {
        // 青色行星 — 右下方
        x: W * 0.88, y: H * 0.75, r: Math.min(W, H) * 0.09,
        color: [34, 211, 238], darkColor: [10, 80, 100],
        glowColor: [34, 211, 238], glowAlpha: 0.10,
        hasRing: false,
        parallax: 5, phase: 2.5
      },
      {
        // 琥珀色小行星 — 左下方远景
        x: W * 0.25, y: H * 0.82, r: Math.min(W, H) * 0.05,
        color: [226, 163, 102], darkColor: [100, 60, 30],
        glowColor: [226, 163, 102], glowAlpha: 0.08,
        hasRing: false,
        parallax: 2, phase: 5.0
      }
    ];
  }

  function drawPlanet(p, t) {
    var px = p.x + (pointer.x - 0.5) * p.parallax * -8;
    var py = p.y + (pointer.y - 0.5) * p.parallax * -6;
    var r = p.r;

    // 外发光光晕
    var outerGlow = ctx.createRadialGradient(px, py, r * 0.8, px, py, r * 2.2);
    outerGlow.addColorStop(0, 'rgba(' + p.glowColor[0] + ',' + p.glowColor[1] + ',' + p.glowColor[2] + ',' + p.glowAlpha + ')');
    outerGlow.addColorStop(0.5, 'rgba(' + p.glowColor[0] + ',' + p.glowColor[1] + ',' + p.glowColor[2] + ',' + (p.glowAlpha * 0.3).toFixed(3) + ')');
    outerGlow.addColorStop(1, 'rgba(' + p.glowColor[0] + ',' + p.glowColor[1] + ',' + p.glowColor[2] + ',0)');
    ctx.fillStyle = outerGlow;
    ctx.beginPath();
    ctx.arc(px, py, r * 2.2, 0, Math.PI * 2);
    ctx.fill();

    // 行星环（在行星本体之前绘制后半部分）
    if (p.hasRing) {
      var ringRx = r * 2.0;
      var ringRy = r * 0.5;
      var tilt = 0.25; // 倾斜角
      ctx.save();
      ctx.translate(px, py);
      ctx.rotate(tilt);
      // 环的后半部分（暗）
      ctx.strokeStyle = 'rgba(' + p.ringColor[0] + ',' + p.ringColor[1] + ',' + p.ringColor[2] + ',0.15)';
      ctx.lineWidth = r * 0.12;
      ctx.beginPath();
      ctx.ellipse(0, 0, ringRx, ringRy, 0, Math.PI * 0.1, Math.PI * 0.9);
      ctx.stroke();
      ctx.restore();
    }

    // 行星本体 — 暗面基底
    ctx.fillStyle = 'rgba(' + p.darkColor[0] + ',' + p.darkColor[1] + ',' + p.darkColor[2] + ',0.9)';
    ctx.beginPath();
    ctx.arc(px, py, r, 0, Math.PI * 2);
    ctx.fill();

    // 行星本体 — 亮面（偏移的径向渐变，营造3D球体感）
    var lightOffset = r * 0.35;
    var surfaceGrad = ctx.createRadialGradient(
      px - lightOffset, py - lightOffset, r * 0.1,
      px, py, r
    );
    surfaceGrad.addColorStop(0, 'rgba(' + p.color[0] + ',' + p.color[1] + ',' + p.color[2] + ',0.75)');
    surfaceGrad.addColorStop(0.4, 'rgba(' + p.color[0] + ',' + p.color[1] + ',' + p.color[2] + ',0.35)');
    surfaceGrad.addColorStop(0.75, 'rgba(' + p.darkColor[0] + ',' + p.darkColor[1] + ',' + p.darkColor[2] + ',0.2)');
    surfaceGrad.addColorStop(1, 'rgba(' + p.darkColor[0] + ',' + p.darkColor[1] + ',' + p.darkColor[2] + ',0.6)');
    ctx.fillStyle = surfaceGrad;
    ctx.beginPath();
    ctx.arc(px, py, r, 0, Math.PI * 2);
    ctx.fill();

    // 边缘高光
    var edgeGrad = ctx.createRadialGradient(px - lightOffset * 0.5, py - lightOffset * 0.5, r * 0.6, px, py, r);
    edgeGrad.addColorStop(0, 'rgba(255,255,255,0)');
    edgeGrad.addColorStop(0.85, 'rgba(255,255,255,0)');
    edgeGrad.addColorStop(1, 'rgba(255,255,255,0.08)');
    ctx.fillStyle = edgeGrad;
    ctx.beginPath();
    ctx.arc(px, py, r, 0, Math.PI * 2);
    ctx.fill();

    // 行星环的前半部分（亮）
    if (p.hasRing) {
      ctx.save();
      ctx.translate(px, py);
      ctx.rotate(tilt);
      ctx.strokeStyle = 'rgba(' + p.ringColor[0] + ',' + p.ringColor[1] + ',' + p.ringColor[2] + ',0.35)';
      ctx.lineWidth = r * 0.1;
      ctx.beginPath();
      ctx.ellipse(0, 0, ringRx, ringRy, 0, Math.PI * 1.15, Math.PI * 1.85);
      ctx.stroke();
      // 环的内边缘高亮
      ctx.strokeStyle = 'rgba(255,255,255,0.12)';
      ctx.lineWidth = r * 0.03;
      ctx.beginPath();
      ctx.ellipse(0, 0, ringRx * 0.92, ringRy * 0.92, 0, Math.PI * 1.2, Math.PI * 1.8);
      ctx.stroke();
      ctx.restore();
    }
  }

  var STAR_TINTS = [
    [232, 236, 246], // 星白
    [167, 139, 250], // 紫
    [103, 232, 249], // 青
    [226, 163, 102], // 琥珀
    [244, 114, 182], // 粉
    [139, 92, 246]   // 深紫
  ];

  function buildStars() {
    stars = [];
    LAYERS.forEach(function (layer, li) {
      var n = Math.round(W * H * layer.count / 10000) * 10;
      for (var i = 0; i < n; i++) {
        stars.push({
          x: Math.random() * W,
          y: Math.random() * H,
          r: rand(layer.rMin, layer.rMax),
          speed: layer.speed,
          alpha: layer.alpha * rand(0.3, 1),
          phase: Math.random() * Math.PI * 2,
          twinkle: rand(0.4, 2.6),
          layer: li,
          tint: STAR_TINTS[(Math.random() * STAR_TINTS.length) | 0],
          pulse: rand(0.8, 1.2)
        });
      }
    });
  }

  /* ---------- 星座连线：同层近邻星之间绘制微弱连线 ---------- */
  function drawConstellations(layerIndex) {
    var layerStars = stars.filter(function (s) { return s.layer === layerIndex; });
    if (layerStars.length < 2) return;
    var maxDist = layerIndex === 2 ? 90 : 70;
    var connAlpha = layerIndex === 2 ? 0.12 : 0.06;

    for (var i = 0; i < layerStars.length; i++) {
      var a = layerStars[i];
      // 只连后面的，避免重复
      for (var j = i + 1; j < layerStars.length; j++) {
        var b = layerStars[j];
        var d = dist(a, b);
        if (d < maxDist) {
          var alpha = connAlpha * (1 - d / maxDist);
          ctx.strokeStyle = 'rgba(167,139,250,' + alpha.toFixed(3) + ')';
          ctx.lineWidth = 0.5;
          ctx.beginPath();
          ctx.moveTo(a.x, a.y);
          ctx.lineTo(b.x, b.y);
          ctx.stroke();
        }
      }
    }
  }

  /* ---------- 极光层：缓慢呼吸漂移，绝不鬼畜 ----------
   * t 是 rAF 时间戳(ms)。speed 单位：弧度/毫秒。
   * 周期 = 2π / speed (ms)。例如 speed=0.0003 → 周期≈21s。
   * 取消旧的 *1000 乘数，那是鬼畜根源。 */
  function buildAurora() {
    auroraBands = [
      { hue: [124, 92, 255], baseY: 0.16, amp: 0.09, freq: 1.4, speed: 0.00025, drift: 0.00004, alpha: 0.07, phase: 0 },
      { hue: [34, 211, 238],  baseY: 0.26, amp: 0.07, freq: 1.9, speed: 0.00018, drift: 0.00006, alpha: 0.05, phase: 2.1 },
      { hue: [244, 114, 182], baseY: 0.10, amp: 0.05, freq: 2.4, speed: 0.00032, drift: 0.00003, alpha: 0.035, phase: 4.3 }
    ];
  }

  /* ---------- 流星系统（增强版：带尾迹光晕） ---------- */
  function spawnMeteor() {
    var x = rand(W * 0.1, W * 1.1);
    var y = rand(-50, H * 0.3);
    var len = rand(140, 320);
    var speed = rand(3.8, 6.5);
    var angle = rand(Math.PI * 0.55, Math.PI * 0.75);
    meteors.push({
      x: x, y: y,
      vx: -Math.cos(angle) * speed,
      vy: Math.sin(angle) * speed,
      len: len,
      life: 1,
      decay: rand(0.006, 0.013),
      width: rand(1.2, 2.2)
    });
    scheduleMeteor();
  }

  function scheduleMeteor() {
    setTimeout(function () { if (running) spawnMeteor(); }, rand(2200, 7000));
  }

  function resize() {
    W = window.innerWidth;
    H = window.innerHeight;
    bgCanvas.width = W * DPR;
    bgCanvas.height = H * DPR;
    bgCanvas.style.width = W + 'px';
    bgCanvas.style.height = H + 'px';
    ctx.setTransform(DPR, 0, 0, DPR, 0, 0);
    buildStars();
    buildPlanets();
  }

  /* ---------- 绘制函数 ---------- */
  /* ---------- 极光绘制：慢波漂移 + 纵向呼吸 ---------- */
  function drawAurora(t) {
    auroraBands.forEach(function (band) {
      var grad = ctx.createLinearGradient(0, 0, 0, H * 0.55);
      var c = band.hue;
      // 纵向呼吸：透明度随时间轻微脉动
      var breath = 0.85 + 0.15 * Math.sin(t * 0.00012 + band.phase);
      var a = band.alpha * breath;
      grad.addColorStop(0, 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + a.toFixed(3) + ')');
      grad.addColorStop(0.45, 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + (a * 0.5).toFixed(3) + ')');
      grad.addColorStop(1, 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',0)');
      ctx.fillStyle = grad;
      ctx.beginPath();
      ctx.moveTo(0, 0);
      var step = 24;
      // 基础纵向漂移
      var drift = Math.sin(t * band.drift + band.phase) * 0.02;
      for (var x = 0; x <= W + step; x += step) {
        var normX = x / W;
        // 主波 + 次级谐波，让波形更自然
        var wave = Math.sin(normX * Math.PI * band.freq + t * band.speed + band.phase);
        wave += 0.35 * Math.sin(normX * Math.PI * band.freq * 1.7 + t * band.speed * 1.3 + band.phase + 1.0);
        var y = H * (band.baseY + drift + band.amp * wave);
        ctx.lineTo(x, y);
      }
      ctx.lineTo(W, 0);
      ctx.closePath();
      ctx.fill();
    });
  }

  function drawStars(t) {
    pointer.x += (pointer.tx - pointer.x) * 0.03;
    pointer.y += (pointer.ty - pointer.y) * 0.03;

    // 按层绘制，先远后近
    for (var li = 0; li < LAYERS.length; li++) {
      if (LAYERS[li].conn) drawConstellations(li);

      stars.forEach(function (s) {
        if (s.layer !== li) return;
        s.x -= s.speed;
        if (s.x < -6) { s.x = W + 6; s.y = Math.random() * H; }

        var par = (s.layer + 1) * 8;
        var dx = (pointer.x - 0.5) * par;
        var dy = (pointer.y - 0.5) * par;
        var tw = 0.55 + 0.45 * Math.sin(t * 0.0008 * s.twinkle + s.phase);
        var pulse = s.pulse + 0.15 * Math.sin(t * 0.001 + s.phase);
        var a = s.alpha * tw;
        var c = s.tint;
        var px = s.x + dx;
        var py = s.y + dy;
        var pr = s.r * pulse;

        // 星点本体
        ctx.fillStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + a.toFixed(3) + ')';
        ctx.beginPath();
        ctx.arc(px, py, pr, 0, Math.PI * 2);
        ctx.fill();

        // 亮星加十字光芒 + 光晕
        if (s.layer >= 1 && s.r > 1.0 && tw > 0.85) {
          var glow = pr * 4;
          var gAlpha = a * 0.25;
          ctx.fillStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + gAlpha.toFixed(3) + ')';
          ctx.beginPath();
          ctx.arc(px, py, glow, 0, Math.PI * 2);
          ctx.fill();

          ctx.strokeStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + (a * 0.35).toFixed(3) + ')';
          ctx.lineWidth = 0.5;
          var gLen = pr * 3.5;
          ctx.beginPath();
          ctx.moveTo(px - gLen, py); ctx.lineTo(px + gLen, py);
          ctx.moveTo(px, py - gLen); ctx.lineTo(px, py + gLen);
          ctx.stroke();
        }
      });
    }
  }

  function drawMeteors() {
    for (var i = meteors.length - 1; i >= 0; i--) {
      var m = meteors[i];
      m.x += m.vx; m.y += m.vy; m.life -= m.decay;
      if (m.life <= 0 || m.x < -200 || m.y > H + 100) { meteors.splice(i, 1); continue; }

      var tailX = m.x - m.vx * (m.len / 5);
      var tailY = m.y - m.vy * (m.len / 5);

      // 尾迹光晕
      var glowGrad = ctx.createRadialGradient(m.x, m.y, 0, m.x, m.y, m.len * 0.3);
      glowGrad.addColorStop(0, 'rgba(232,236,246,' + (0.15 * m.life).toFixed(3) + ')');
      glowGrad.addColorStop(1, 'rgba(232,236,246,0)');
      ctx.fillStyle = glowGrad;
      ctx.beginPath();
      ctx.arc(m.x, m.y, m.len * 0.3, 0, Math.PI * 2);
      ctx.fill();

      // 尾迹线
      var grad = ctx.createLinearGradient(m.x, m.y, tailX, tailY);
      grad.addColorStop(0, 'rgba(255,255,255,' + (0.95 * m.life).toFixed(3) + ')');
      grad.addColorStop(0.25, 'rgba(167,139,250,' + (0.45 * m.life).toFixed(3) + ')');
      grad.addColorStop(0.6, 'rgba(103,232,249,' + (0.2 * m.life).toFixed(3) + ')');
      grad.addColorStop(1, 'rgba(103,232,249,0)');
      ctx.strokeStyle = grad;
      ctx.lineWidth = m.width * m.life;
      ctx.lineCap = 'round';
      ctx.beginPath();
      ctx.moveTo(m.x, m.y);
      ctx.lineTo(tailX, tailY);
      ctx.stroke();

      // 头部亮点
      ctx.fillStyle = 'rgba(255,255,255,' + m.life.toFixed(3) + ')';
      ctx.beginPath();
      ctx.arc(m.x, m.y, m.width * 0.8, 0, Math.PI * 2);
      ctx.fill();
    }
  }

  /* ---------- 鼠标跟随光晕 ---------- */
  function drawPointerGlow() {
    var mx = pointer.tx * W;
    var my = pointer.ty * H;
    var glow = ctx.createRadialGradient(mx, my, 0, mx, my, 180);
    glow.addColorStop(0, 'rgba(124,92,255,0.035)');
    glow.addColorStop(0.5, 'rgba(34,211,238,0.015)');
    glow.addColorStop(1, 'rgba(0,0,0,0)');
    ctx.fillStyle = glow;
    ctx.beginPath();
    ctx.arc(mx, my, 180, 0, Math.PI * 2);
    ctx.fill();
  }

  /* ---------- 主循环 ---------- */
  var running = !reduced;
  function frame(t) {
    if (!running) return;
    ctx.clearRect(0, 0, W, H);
    // 绘制顺序：行星（最远）→ 极光 → 鼠标光晕 → 星点 → 流星（最近）
    planets.forEach(function (p) { drawPlanet(p, t); });
    drawAurora(t);
    drawPointerGlow();
    drawStars(t);
    drawMeteors();
    requestAnimationFrame(frame);
  }

  window.addEventListener('resize', resize);
  window.addEventListener('pointermove', function (e) {
    pointer.tx = e.clientX / W;
    pointer.ty = e.clientY / H;
  }, { passive: true });

  resize();
  buildAurora();
  if (running) {
    requestAnimationFrame(frame);
    scheduleMeteor();
  }

  /* ===================== 点击特效层 ===================== */
  var fxCanvas = document.createElement('canvas');
  fxCanvas.className = 'fx-click';
  fxCanvas.setAttribute('aria-hidden', 'true');
  document.body.appendChild(fxCanvas);
  var fctx = fxCanvas.getContext('2d');

  var particles = [];
  var ripples = [];
  var shockwaves = [];

  function sizeFx() {
    fxCanvas.width = W * DPR;
    fxCanvas.height = H * DPR;
    fxCanvas.style.width = W + 'px';
    fxCanvas.style.height = H + 'px';
    fctx.setTransform(DPR, 0, 0, DPR, 0, 0);
  }
  sizeFx();
  window.addEventListener('resize', sizeFx);

  var FX_COLORS = [
    [167, 139, 250], // 紫
    [103, 232, 249], // 青
    [226, 163, 102], // 琥珀
    [244, 114, 182], // 粉
    [232, 236, 246], // 白
    [139, 92, 246]   // 深紫
  ];

  /* ---------- 点击爆发：能量迸发系统 ----------
   * 1. 核心闪光：点击瞬间白色亮斑
   * 2. 拖尾粒子：菱形/十字星/圆点，带发光拖尾
   * 3. 三道涟漪：逐步减速扩散
   * 4. 冲击波：彩色光环快速外扩 */
  var flares = [];

  function burst(x, y) {
    // 核心闪光
    flares.push({ x: x, y: y, r: 2, max: rand(12, 22), life: 1, decay: 0.08 });

    // 粒子迸发：18-26颗
    var n = 18 + (Math.random() * 9 | 0);
    for (var i = 0; i < n; i++) {
      var ang = Math.random() * Math.PI * 2;
      var v = rand(2.2, 7.5);
      var shapeRoll = Math.random();
      var shape = shapeRoll < 0.25 ? 'diamond' : (shapeRoll < 0.5 ? 'cross' : 'dot');
      particles.push({
        x: x, y: y,
        vx: Math.cos(ang) * v,
        vy: Math.sin(ang) * v - rand(1.0, 2.8),
        r: rand(1.2, 3.6),
        life: 1,
        decay: rand(0.015, 0.032),
        tint: FX_COLORS[(Math.random() * FX_COLORS.length) | 0],
        shape: shape,
        rotation: Math.random() * Math.PI * 2,
        rotSpeed: rand(-0.2, 0.2),
        // 拖尾历史：记录最近6个位置
        trail: [],
        trailMax: 6,
        flicker: rand(3, 8), // 闪烁频率
        flickPhase: Math.random() * Math.PI * 2
      });
    }

    // 三道涟漪：大→中→小，逐步延迟
    ripples.push({ x: x, y: y, r: 4, max: rand(60, 90), life: 1, width: 2.2, speed: 0.14 });
    ripples.push({ x: x, y: y, r: 2, max: rand(36, 52), life: 1, width: 1.6, speed: 0.10 });
    ripples.push({ x: x, y: y, r: 1, max: rand(18, 28), life: 1, width: 1.0, speed: 0.07 });

    // 冲击波：快速彩色光环
    shockwaves.push({ x: x, y: y, r: 2, max: rand(100, 160), life: 1, width: 3.5, speed: 0.18 });
  }

  function drawShape(p, a) {
    var c = p.tint;
    var r = p.r * a;
    fctx.save();
    fctx.translate(p.x, p.y);
    fctx.rotate(p.rotation);

    if (p.shape === 'diamond') {
      // 菱形 + 光晕
      fctx.fillStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + (a * 0.2).toFixed(3) + ')';
      fctx.beginPath();
      fctx.moveTo(0, -r * 2.5); fctx.lineTo(r * 1.2, 0);
      fctx.lineTo(0, r * 2.5); fctx.lineTo(-r * 1.2, 0);
      fctx.closePath();
      fctx.fill();
      fctx.fillStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + a.toFixed(3) + ')';
      fctx.beginPath();
      fctx.moveTo(0, -r * 1.2); fctx.lineTo(r * 0.6, 0);
      fctx.lineTo(0, r * 1.2); fctx.lineTo(-r * 0.6, 0);
      fctx.closePath();
      fctx.fill();
    } else if (p.shape === 'cross') {
      // 十字星
      fctx.strokeStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + a.toFixed(3) + ')';
      fctx.lineWidth = r * 0.4;
      fctx.lineCap = 'round';
      var cl = r * 2.2;
      fctx.beginPath();
      fctx.moveTo(-cl, 0); fctx.lineTo(cl, 0);
      fctx.moveTo(0, -cl * 0.5); fctx.lineTo(0, cl * 0.5);
      fctx.stroke();
      // 中心亮点
      fctx.fillStyle = 'rgba(255,255,255,' + a.toFixed(3) + ')';
      fctx.beginPath();
      fctx.arc(0, 0, r * 0.35, 0, Math.PI * 2);
      fctx.fill();
    } else {
      // 圆点 + 光晕
      var glow = r * 2.8;
      fctx.fillStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + (a * 0.25).toFixed(3) + ')';
      fctx.beginPath();
      fctx.arc(0, 0, glow, 0, Math.PI * 2);
      fctx.fill();
      fctx.fillStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + a.toFixed(3) + ')';
      fctx.beginPath();
      fctx.arc(0, 0, r, 0, Math.PI * 2);
      fctx.fill();
    }
    fctx.restore();
  }

  function drawTrail(p, a) {
    if (p.trail.length < 2) return;
    var c = p.tint;
    fctx.lineCap = 'round';
    fctx.lineJoin = 'round';
    for (var i = 1; i < p.trail.length; i++) {
      var t1 = p.trail[i - 1];
      var t2 = p.trail[i];
      var ta = a * (i / p.trail.length) * 0.6;
      fctx.strokeStyle = 'rgba(' + c[0] + ',' + c[1] + ',' + c[2] + ',' + ta.toFixed(3) + ')';
      fctx.lineWidth = p.r * 0.5 * (i / p.trail.length);
      fctx.beginPath();
      fctx.moveTo(t1.x, t1.y);
      fctx.lineTo(t2.x, t2.y);
      fctx.stroke();
    }
  }

  function fxFrame() {
    fctx.clearRect(0, 0, W, H);
    var alive = false;

    // 核心闪光
    for (var f = flares.length - 1; f >= 0; f--) {
      var fl = flares[f];
      fl.r += (fl.max - fl.r) * 0.25;
      fl.life -= fl.decay;
      if (fl.life <= 0) { flares.splice(f, 1); continue; }
      alive = true;
      var fa = fl.life;
      var fg = fctx.createRadialGradient(fl.x, fl.y, 0, fl.x, fl.y, fl.r);
      fg.addColorStop(0, 'rgba(255,255,255,' + (fa * 0.9).toFixed(3) + ')');
      fg.addColorStop(0.3, 'rgba(232,236,246,' + (fa * 0.4).toFixed(3) + ')');
      fg.addColorStop(1, 'rgba(232,236,246,0)');
      fctx.fillStyle = fg;
      fctx.beginPath();
      fctx.arc(fl.x, fl.y, fl.r, 0, Math.PI * 2);
      fctx.fill();
    }

    // 粒子
    for (var i = particles.length - 1; i >= 0; i--) {
      var p = particles[i];

      // 记录拖尾
      p.trail.push({ x: p.x, y: p.y });
      if (p.trail.length > p.trailMax) p.trail.shift();

      p.x += p.vx; p.y += p.vy;
      p.vy += 0.07; // 微重力
      p.vx *= 0.985; p.vy *= 0.985;
      p.life -= p.decay;
      p.rotation += p.rotSpeed;
      if (p.life <= 0) { particles.splice(i, 1); continue; }
      alive = true;

      // 闪烁调制
      var flick = 0.75 + 0.25 * Math.sin(p.life * p.flicker * 6 + p.flickPhase);
      var a = p.life * flick;

      // 先画拖尾
      drawTrail(p, a);
      // 再画形状
      drawShape(p, a);
    }

    // 涟漪
    for (var j = ripples.length - 1; j >= 0; j--) {
      var rp = ripples[j];
      rp.r += (rp.max - rp.r) * rp.speed;
      rp.life -= 0.022;
      if (rp.life <= 0) { ripples.splice(j, 1); continue; }
      alive = true;
      var ra = rp.life * 0.55;
      fctx.strokeStyle = 'rgba(167,139,250,' + ra.toFixed(3) + ')';
      fctx.lineWidth = rp.width * rp.life;
      fctx.beginPath();
      fctx.arc(rp.x, rp.y, rp.r, 0, Math.PI * 2);
      fctx.stroke();
    }

    // 冲击波
    for (var k = shockwaves.length - 1; k >= 0; k--) {
      var sw = shockwaves[k];
      sw.r += (sw.max - sw.r) * sw.speed;
      sw.life -= 0.022;
      if (sw.life <= 0) { shockwaves.splice(k, 1); continue; }
      alive = true;
      var swA = sw.life;
      // 外环：紫→透明
      fctx.strokeStyle = 'rgba(139,92,246,' + (swA * 0.35).toFixed(3) + ')';
      fctx.lineWidth = sw.width * swA;
      fctx.beginPath();
      fctx.arc(sw.x, sw.y, sw.r, 0, Math.PI * 2);
      fctx.stroke();
      // 中环：青
      if (sw.r > sw.max * 0.25) {
        fctx.strokeStyle = 'rgba(34,211,238,' + (swA * 0.25).toFixed(3) + ')';
        fctx.lineWidth = (sw.width * 0.6) * swA;
        fctx.beginPath();
        fctx.arc(sw.x, sw.y, sw.r * 0.75, 0, Math.PI * 2);
        fctx.stroke();
      }
      // 内环：白
      if (sw.r > sw.max * 0.5) {
        fctx.strokeStyle = 'rgba(255,255,255,' + (swA * 0.2).toFixed(3) + ')';
        fctx.lineWidth = (sw.width * 0.35) * swA;
        fctx.beginPath();
        fctx.arc(sw.x, sw.y, sw.r * 0.5, 0, Math.PI * 2);
        fctx.stroke();
      }
    }

    if (alive || particles.length || ripples.length || shockwaves.length || flares.length) {
      requestAnimationFrame(fxFrame);
    } else {
      fxActive = false;
    }
  }

  var fxActive = false;
  function ensureLoop() {
    if (!fxActive) { fxActive = true; requestAnimationFrame(fxFrame); }
  }

  if (!reduced) {
    window.addEventListener('pointerdown', function (e) {
      burst(e.clientX, e.clientY);
      ensureLoop();
    }, { passive: true });
  }

  /* ===================== 扫描线叠加层 ===================== */
  if (!reduced) {
    var scanCanvas = document.createElement('canvas');
    scanCanvas.className = 'fx-scan';
    scanCanvas.setAttribute('aria-hidden', 'true');
    document.body.appendChild(scanCanvas);
    var sctx = scanCanvas.getContext('2d');

    function sizeScan() {
      scanCanvas.width = W * DPR;
      scanCanvas.height = H * DPR;
      scanCanvas.style.width = W + 'px';
      scanCanvas.style.height = H + 'px';
      sctx.setTransform(DPR, 0, 0, DPR, 0, 0);
    }
    sizeScan();
    window.addEventListener('resize', sizeScan);

    var scanY = -20;
    function scanFrame() {
      sctx.clearRect(0, 0, W, H);
      scanY += 1.5;
      if (scanY > H + 20) scanY = -20;

      // 扫描线
      var grad = sctx.createLinearGradient(0, scanY - 8, 0, scanY + 8);
      grad.addColorStop(0, 'rgba(124,92,255,0)');
      grad.addColorStop(0.5, 'rgba(124,92,255,0.018)');
      grad.addColorStop(1, 'rgba(124,92,255,0)');
      sctx.fillStyle = grad;
      sctx.fillRect(0, scanY - 8, W, 16);

      // 随机噪点
      if (Math.random() < 0.08) {
        var nx = Math.random() * W;
        var ny = Math.random() * H;
        sctx.fillStyle = 'rgba(255,255,255,' + (Math.random() * 0.04).toFixed(3) + ')';
        sctx.fillRect(nx, ny, 1, 1);
      }

      requestAnimationFrame(scanFrame);
    }
    requestAnimationFrame(scanFrame);
  }

  /* 标记 fx 已激活 */
  document.body.classList.add('fx-on');
})();
