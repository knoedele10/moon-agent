// A single visibility-aware scheduler and bounded, local-only performance samples.
(function () {
  'use strict';
  var jobs = new Map();
  var timer = 0;
  var samples = [];
  var sequence = 0;
  function record(kind, name, duration) {
    samples.push({ kind: kind, name: name, ms: Math.round(duration * 10) / 10, at: Date.now() });
    if (samples.length > 200) samples.shift();
  }
  function active(job) {
    return !document.hidden && (!job.visible || job.visible());
  }
  function tick() {
    timer = 0;
    var now = Date.now();
    jobs.forEach(function (job, id) {
      if (job.busy || !active(job) || now < job.next) return;
      job.busy = true;
      Promise.resolve().then(function () {
        if (jobs.get(id) === job && active(job)) return job.run();
      }).catch(function () { /* The next scheduled update retries. */ }).finally(function () {
        job.busy = false;
        job.next = Date.now() + job.interval;
      });
    });
    if (jobs.size && !document.hidden) timer = setTimeout(tick, 500);
  }
  function wake() {
    if (timer) clearTimeout(timer);
    timer = 0;
    if (!document.hidden && jobs.size) timer = setTimeout(tick, 0);
  }
  function every(run, interval, visible) {
    var id = ++sequence;
    jobs.set(id, { run: run, interval: interval, visible: visible, busy: false, next: Date.now() + interval });
    wake();
    return id;
  }
  function stop(id) { jobs.delete(id); if (!jobs.size && timer) { clearTimeout(timer); timer = 0; } }
  document.addEventListener('visibilitychange', function () {
    document.documentElement.classList.toggle('is-background', document.hidden);
    if (!document.hidden) jobs.forEach(function (job) { job.next = 0; });
    wake();
  });
  window.APP_RUNTIME = {
    every: every, stop: stop, wake: wake,
    idle: function (fn) {
      if (window.requestIdleCallback) window.requestIdleCallback(fn, { timeout: 1200 });
      else setTimeout(fn, 150);
    }
  };
  window.APP_PERF = {
    record: record,
    snapshot: function () {
      return { samples: samples.slice(), activePolls: jobs.size, hidden: document.hidden,
        messageNodes: document.querySelectorAll('.thread .msg').length };
    }
  };
  if (window.PerformanceObserver) {
    try {
      new PerformanceObserver(function (list) {
        list.getEntries().forEach(function (entry) { record('longtask', 'main-thread', entry.duration); });
      }).observe({ type: 'longtask', buffered: true });
    } catch (e) { /* Older browsers still expose request and render timings. */ }
  }
})();
