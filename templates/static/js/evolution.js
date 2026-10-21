// 学习面板只展示后端证据；打开面板不会触发技能修改或启动外部工具。
(function () {
  'use strict';
  var panel = document.getElementById('evolution-panel');
  if (!panel) return;
  var status = document.getElementById('evolution-status');
  var tools = document.getElementById('evolution-tools');
  var report = document.getElementById('evolution-report');
  var refresh = document.getElementById('evolution-refresh');
  var review = document.getElementById('evolution-review');
  var apply = document.getElementById('evolution-apply');
  var rollback = document.getElementById('evolution-rollback');
  var busy = false;
  var learningEnabled = false;
  var labels = { off: '未启用', record: '记录经验', suggest: '生成建议', auto: '自动学习' };

  async function request(path, body) {
    var options = { credentials: 'same-origin' };
    if (body !== undefined) {
      options.method = 'POST';
      options.headers = { 'Content-Type': 'application/json' };
      options.body = JSON.stringify(body);
    }
    var response = await fetch('/api/agent/' + path, options);
    var data = await response.json();
    if (!response.ok) throw new Error(data.error || '记录暂时无法读取，请稍后重试。');
    return data;
  }

  function line(text) {
    var item = document.createElement('li');
    item.textContent = text;
    report.appendChild(item);
  }

  function showReport(data) {
    report.replaceChildren();
    if (!data) { line('还没有学习报告。完成任务后可在这里分析经验。'); return; }
    line('已回看 ' + (data.runs || 0) + ' 次任务，检查 ' + (data.skills || 0) + ' 条技能。');
    (data.candidates || []).slice(0, 12).forEach(function (candidate) {
      line('学习建议：' + (candidate.description || candidate.name) +
        (candidate.auto_applicable ? '' : '（需补充证据）'));
    });
    (data.applied || []).forEach(function (name) { line('已采用：' + name); });
    (data.at_risk || []).forEach(function (skill) {
      line('需要复查：' + skill.name + '，使用 ' + skill.uses + ' 次，任务成功率 ' +
        Math.round(skill.success_rate * 100) + '%。');
    });
  }

  async function load() {
    var data = await request('evolution');
    learningEnabled = !!data.enabled;
    status.textContent = '学习状态：' + (labels[data.mode] || data.mode) +
      '。已积累 ' + (data.memory_entries || 0) + ' 条长期记忆，' + (data.skill_stats || 0) + ' 条技能有使用记录。';
    apply.hidden = rollback.hidden = !data.can_apply;
    review.disabled = !data.enabled;
    showReport(data.report);
    var catalog = await request('tool-catalog');
    tools.textContent = '工具按需发现：' + (catalog.disclosure === 'search' ? '已开启' : '未开启') +
      '；大结果续读：' + (catalog.spill > 0 ? '已开启' : '未开启') +
      (catalog.mcp && catalog.mcp.summary ? '；' + catalog.mcp.summary : '') + '。';
  }

  async function run(action) {
    if (busy) return;
    busy = true;
    refresh.disabled = review.disabled = apply.disabled = rollback.disabled = true;
    try { await action(); } catch (err) { status.textContent = err.message; }
    finally {
      busy = false;
      refresh.disabled = apply.disabled = rollback.disabled = false;
      review.disabled = !learningEnabled;
    }
  }

  async function refreshSkills() {
    if (window.SKILLS && window.SKILLS.onShow) await window.SKILLS.onShow();
  }

  panel.addEventListener('toggle', function () { if (panel.open) run(load); });
  refresh.addEventListener('click', function () { run(load); });
  review.addEventListener('click', function () {
    run(async function () { await request('evolution/curate', { apply: false }); await load(); });
  });
  apply.addEventListener('click', function () {
    run(async function () { await request('evolution/curate', { apply: true }); await load(); await refreshSkills(); });
  });
  rollback.addEventListener('click', function () {
    run(async function () {
      await request('evolution/rollback', {});
      await load();
      await refreshSkills();
      status.textContent = '最近一次自动学习已撤销，技能列表已更新。';
    });
  });
})();
