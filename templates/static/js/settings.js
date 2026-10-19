(function () {
  'use strict';

  var STATIC_VENDORS = [
    { id: 'deepseek', label: 'DeepSeek' },
    { id: 'openai', label: 'OpenAI' },
    { id: 'xai', label: 'Grok (xAI)' },
    { id: 'moonshot', label: 'Kimi (Moonshot)' },
    { id: 'qwen', label: '通义千问' },
    { id: 'zhipu', label: '智谱 GLM' },
    { id: 'anthropic', label: 'Claude' },
    { id: 'gemini', label: 'Gemini' },
    { id: 'ernie', label: '文心一言' }
  ];

  var vendorLabels = {};
  var vendorOrder = [];
  var vendorNotes = {
    deepseek: 'DeepSeek 官方兼容接口。若频繁出现 503，通常是服务端繁忙，可暂时切换其他厂商。',
    openai: 'OpenAI 官方接口，支持工具调用的模型可用于完整 Agent 流程。',
    xai: 'Grok 官方接口，使用 xAI API Key。接入 grok-4.7，可用于流式对话和 Agent 工具调用。',
    moonshot: 'Kimi 官方兼容接口。请选用支持 function calling 的模型。',
    qwen: '通义千问兼容接口。工具调用后会再次请求模型整理执行结果。',
    zhipu: '智谱 GLM 官方兼容接口，请确认所选模型支持工具调用。',
    anthropic: 'Claude 官方接口，使用 Anthropic API Key。',
    gemini: 'Gemini 官方接口，使用 Google AI API Key。',
    ernie: '文心一言接口，凭证格式为 API_KEY:SECRET_KEY。'
  };

  var els = {
    keyList: document.getElementById('key-list'),
    endpoint: document.getElementById('endpoint'),
    apiKey: document.getElementById('api-key'),
    form: document.getElementById('key-form'),
    submit: document.getElementById('key-submit'),
    toast: document.getElementById('toast'),
    soulPersonality: document.getElementById('soul-personality'),
    soulGrowth: document.getElementById('soul-growth'),
    soulVoice: document.getElementById('soul-voice'),
    soulWork: document.getElementById('soul-work'),
    soulSave: document.getElementById('soul-save'),
    soulNote: document.getElementById('soul-note'),
    keyVisibility: document.getElementById('key-visibility'),
    providerNote: document.getElementById('provider-note'),
    keyCount: document.getElementById('key-count')
  };

  var toastTimer = null;

  function toast(message) {
    els.toast.textContent = message;
    els.toast.classList.add('show');
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      els.toast.classList.remove('show');
    }, 2600);
  }

  async function api(path, options) {
    var res = await fetch(path, options);
    if (res.status === 401) {
      window.location.href = '/login';
      throw new Error('unauthorized');
    }
    return res;
  }

  async function apiJSON(path, options) {
    var res = await api(path, options);
    if (!res.ok) {
      var message = 'HTTP ' + res.status;
      try {
        var body = await res.json();
        if (body && body.error) message = body.error;
      } catch (e) {}
      throw new Error(message);
    }
    if (res.status === 204) return null;
    return res.json();
  }

  function labelFor(endpoint) {
    return vendorLabels[endpoint] || endpoint;
  }

  function initVendors() {
    STATIC_VENDORS.forEach(function (vendor) {
      if (!vendorLabels[vendor.id]) {
        vendorLabels[vendor.id] = vendor.label;
        vendorOrder.push(vendor.id);
      }
    });
  }

  function renderVendorOptions() {
    var selected = els.endpoint.value;
    els.endpoint.textContent = '';
    var placeholder = document.createElement('option');
    placeholder.value = '';
    placeholder.textContent = '请选择厂商';
    els.endpoint.appendChild(placeholder);

    vendorOrder.forEach(function (endpoint) {
      var opt = document.createElement('option');
      opt.value = endpoint;
      opt.textContent = vendorLabels[endpoint];
      els.endpoint.appendChild(opt);
    });
    if (selected && vendorLabels[selected]) els.endpoint.value = selected;
    updateProviderNote();
  }

  async function refreshVendors() {
    var models;
    try {
      models = await apiJSON('/api/models');
    } catch (e) {
      models = null;
    }
    if (Array.isArray(models)) {
      models.forEach(function (model) {
        var endpoint = model.endpoint;
        if (!endpoint || endpoint === 'mock') return;
        if (!vendorLabels[endpoint]) {
          var name = String(model.label || '').split('·')[0].trim() || endpoint;
          vendorLabels[endpoint] = name;
          vendorOrder.push(endpoint);
        }
      });
    }
    renderVendorOptions();
  }

  function renderKeys(keys) {
    els.keyList.textContent = '';
    if (els.keyCount) els.keyCount.textContent = Array.isArray(keys) ? String(keys.length) : '0';
    if (!Array.isArray(keys) || !keys.length) {
      var hint = document.createElement('div');
      hint.className = 'list-hint';
      hint.textContent = '还没有配置任何 Key，可在下方新增。';
      els.keyList.appendChild(hint);
      return;
    }

    keys.forEach(function (key) {
      var item = document.createElement('div');
      item.className = 'key-item';

      var vendor = document.createElement('span');
      vendor.className = 'key-vendor';
      vendor.textContent = labelFor(key.endpoint);
      item.appendChild(vendor);

      var masked = document.createElement('span');
      masked.className = 'key-masked';
      masked.textContent = key.masked || '****';
      item.appendChild(masked);

      var del = document.createElement('button');
      del.type = 'button';
      del.className = 'btn btn-danger';
      del.textContent = '删除';
      del.addEventListener('click', function () {
        removeKey(key.id, labelFor(key.endpoint));
      });
      item.appendChild(del);

      els.keyList.appendChild(item);
    });
  }

  async function loadKeys() {
    try {
      var keys = await apiJSON('/api/keys');
      renderKeys(keys);
    } catch (e) {
      renderKeys([]);
      toast('Key 列表加载失败：' + e.message);
    }
  }

  async function addKey(ev) {
    ev.preventDefault();
    var endpoint = els.endpoint.value;
    var apiKey = els.apiKey.value.trim();
    if (!endpoint) {
      toast('请选择厂商');
      return;
    }
    if (!apiKey) {
      toast('请填写 API Key');
      return;
    }

    els.submit.disabled = true;
    els.submit.textContent = '保存中…';
    try {
      await apiJSON('/api/keys', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ endpoint: endpoint, api_key: apiKey })
      });
      els.apiKey.value = '';
      toast('已保存');
      await loadKeys();
    } catch (e) {
      toast('保存失败：' + e.message);
    } finally {
      els.submit.disabled = false;
      els.submit.textContent = '保存连接';
    }
  }

  async function removeKey(id, name) {
    if (!window.confirm('删除「' + name + '」的 Key？')) return;
    try {
      await apiJSON('/api/keys/' + id, { method: 'DELETE' });
      toast('已删除');
      await loadKeys();
    } catch (e) {
      toast('删除失败：' + e.message);
    }
  }

  els.form.addEventListener('submit', addKey);

  function updateProviderNote() {
    if (!els.providerNote) return;
    var endpoint = els.endpoint.value;
    els.providerNote.textContent = endpoint
      ? (vendorNotes[endpoint] || (labelFor(endpoint) + ' 的连接信息将使用项目内置端点。'))
      : '选择厂商后，这里会显示对应的连接说明。';
  }

  els.endpoint.addEventListener('change', updateProviderNote);
  if (els.keyVisibility) {
    els.keyVisibility.addEventListener('click', function () {
      var showing = els.apiKey.type === 'text';
      els.apiKey.type = showing ? 'password' : 'text';
      els.keyVisibility.textContent = showing ? '显示' : '隐藏';
      els.keyVisibility.setAttribute('aria-label', showing ? '显示 API Key' : '隐藏 API Key');
      els.keyVisibility.setAttribute('aria-pressed', showing ? 'false' : 'true');
      els.apiKey.focus();
    });
  }

  // ---- 灵魂：人格 + 说话方式 + 做事方式（用户亲手写给助手的设定）----

  function soulNote(text, kind) {
    if (!els.soulNote) return;
    els.soulNote.textContent = text || '';
    els.soulNote.className = kind === 'error' ? 'kb-note kb-note-error' : (kind === 'ok' ? 'kb-note kb-note-ok' : 'kb-note');
  }

  async function loadSoul() {
    if (!els.soulVoice) return;
    try {
      var st = await apiJSON('/api/agent/settings');
      els.soulPersonality.value = st.soul_personality || '';
      if (els.soulGrowth) els.soulGrowth.value = st.soul_growth || '';
      els.soulVoice.value = st.soul_voice || '';
      els.soulWork.value = st.soul_work || '';
      soulNote(st.soul_personality || st.soul_voice || st.soul_work ? '当前已设置灵魂，每轮对话都会生效。' : '还没有设置灵魂。');
    } catch (e) {
      soulNote('读取失败：' + (e.message || '未知错误'), 'error');
    }
  }

  async function saveSoul() {
    if (!els.soulVoice) return;
    if (els.soulSave) els.soulSave.disabled = true;
    soulNote('保存中…');
    try {
      await apiJSON('/api/agent/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ soul_personality: els.soulPersonality.value, soul_growth: els.soulGrowth.value, soul_voice: els.soulVoice.value, soul_work: els.soulWork.value })
      });
      soulNote('已保存，下一轮对话开始生效。', 'ok');
      toast('灵魂已保存');
    } catch (e) {
      soulNote('保存失败：' + (e.message || '未知错误'), 'error');
    } finally {
      if (els.soulSave) els.soulSave.disabled = false;
    }
  }

  if (els.soulSave) els.soulSave.addEventListener('click', saveSoul);


  // 模式专属工作区（代码 / 学习）。留空 = 清空（回落通用工作区），所以空串要原样发出去。
  function rootsNote(text, kind) {
    var n = document.getElementById('roots-note');
    if (!n) return;
    n.textContent = text || '';
    n.className = kind === 'error' ? 'kb-note kb-note-error' : (kind === 'ok' ? 'kb-note kb-note-ok' : 'kb-note');
  }

  async function loadRoots() {
    var code = document.getElementById('code-root');
    var learn = document.getElementById('learn-root');
    if (!code || !learn) return;
    try {
      var st = await apiJSON('/api/agent/settings');
      code.value = st.code_root || '';
      learn.value = st.learn_root || '';
      rootsNote(st.code_root || st.learn_root ? '已设置模式专属目录。' : '两个目录都留空，全部走通用工作区。');
    } catch (e) {
      rootsNote('读取失败：' + (e.message || '未知错误'), 'error');
    }
  }

  async function saveRoots() {
    var code = document.getElementById('code-root');
    var learn = document.getElementById('learn-root');
    var btn = document.getElementById('roots-save');
    if (!code || !learn) return;
    if (btn) btn.disabled = true;
    rootsNote('保存中…');
    try {
      // 空串是有意义的（= 清空），所以不能像其它字段那样“没填就不传”。
      await apiJSON('/api/agent/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code_root: code.value, learn_root: learn.value })
      });
      rootsNote('已保存，切到对应模式后生效。', 'ok');
      toast('目录已保存');
    } catch (e) {
      rootsNote('保存失败：' + (e.message || '未知错误'), 'error');
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  var rootsBtn = document.getElementById('roots-save');
  if (rootsBtn) rootsBtn.addEventListener('click', saveRoots);

  initVendors();
  loadRoots();
  renderVendorOptions();
  loadKeys();
  loadSoul();
  refreshVendors();
  updateProviderNote();
})();
