// 集成层黑盒探测（verifier 自写；按 docs/cua/06-integration-contract.md v1 逐条对齐）
// 用法：node docs/verify/integration-probe.mjs [--base http://127.0.0.1:8300] [--sse-timeout 30000]
// 退出码：0=全 PASS；1=有 FAIL；2=前置缺失（服务或接口未落地，不作判定）
// 安全红线：本脚本创建的所有 run 一律 dry_run=true，且结束时把仍在跑的 run 全部 stop。
// 契约条目号：C1=status / C3=runs 校验 / C4=201 / C5=409 / C6=SSE 形状 / C7=重连补发 /
//            C8=stop / C9=history / C10=代理 401 / C11=代理 data URI 映射（需上游抓包）
import { pathToFileURL } from "node:url";
import { readFileSync } from "node:fs";

export const EVENT_KINDS = ["run_start", "frame", "observation", "agent", "step",
  "action_result", "replan", "expect_timeout", "executor_retry", "guardrail", "run_end"];
export const EXTRA_KINDS = ["frame_image"];

export function makeIO() {
  const results = [];
  function rec(id, level, msg, detail) {
    results.push({ id: id, level: level, msg: msg, detail: detail });
    console.log("[" + level + "] " + id + " :: " + msg);
    if (detail !== undefined) {
      console.log("        证据: " + (typeof detail === "string" ? detail : JSON.stringify(detail)));
    }
  }
  return {
    results: results,
    PASS: function (id, m, d) { rec(id, "PASS", m, d); },
    FAIL: function (id, m, d) { rec(id, "FAIL", m, d); },
    WARN: function (id, m, d) { rec(id, "WARN", m, d); },
    SKIP: function (id, m, d) { rec(id, "SKIP", m, d); },
    INFO: function (id, m, d) { rec(id, "INFO", m, d); }
  };
}

function parseSSEBlock(block) {
  const ev = { event: "message", data: "", raw: block };
  const dataLines = [];
  for (const rawLine of block.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    if (line.startsWith("event:")) ev.event = line.slice(6).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).replace(/^ /, ""));
    else if (line.startsWith(":")) ev.comment = true;
  }
  if (dataLines.length) ev.data = dataLines.join("\n");
  try { ev.json = JSON.parse(ev.data); } catch (e) { ev.json = null; }
  return ev;
}

export async function runProbe(BASE, io, opts) {
  const o = opts || {};
  const sseTimeoutMs = o.sseTimeoutMs || 30000;
  const jar = new Map();
  const created = [];
  function jarHeader() {
    return [...jar.entries()].map(function (kv) { return kv[0] + "=" + kv[1]; }).join("; ");
  }
  function absorb(res) {
    const set = res.headers.getSetCookie ? res.headers.getSetCookie() : [];
    for (const c of set) {
      const pair = c.split(";")[0];
      const i = pair.indexOf("=");
      if (i > 0) jar.set(pair.slice(0, i).trim(), pair.slice(i + 1).trim());
    }
  }
  async function req(path, o2) {
    const opt = o2 || {};
    const headers = Object.assign({}, opt.headers || {});
    if (jar.size) headers["Cookie"] = jarHeader();
    const t0 = Date.now();
    const res = await fetch(BASE + path, Object.assign({}, opt, { headers: headers, redirect: "manual" }));
    absorb(res);
    return { res: res, ms: Date.now() - t0 };
  }
  async function jsonReq(path, method, body, extra) {
    const r = await req(path, {
      method: method,
      headers: Object.assign({ "Content-Type": "application/json" }, extra || {}),
      body: body === undefined ? undefined : JSON.stringify(body)
    });
    const text = await r.res.text();
    let json = null;
    try { json = JSON.parse(text); } catch (e) { json = null; }
    return { status: r.res.status, json: json, text: text, ms: r.ms };
  }
  // SSE 读取：用 AbortController 做超时。绝不把 reader.read() 和定时器 Promise.race —— 
  // race 会遗留一个悬挂的 read()，它可能把最后一块数据（如 exit 事件）吃掉，导致对正确实现误判。
  async function readSSE(path, timeoutMs) {
    const headers = {};
    if (jar.size) headers["Cookie"] = jarHeader();
    const ac = new AbortController();
    let clipped = false;
    const timer = setTimeout(function () { clipped = true; try { ac.abort(); } catch (e) { /* ignore */ } }, timeoutMs);
    try {
      const res = await fetch(BASE + path, { headers: headers, signal: ac.signal });
      const ct = res.headers.get("content-type") || "";
      const events = [];
      const rawChunks = [];
      if (!res.body) return { status: res.status, contentType: ct, events: events, raw: Buffer.alloc(0), note: "no-body" };
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      let sawExit = false;
      while (!sawExit) {
        let chunk;
        try {
          chunk = await reader.read();
        } catch (e) {
          clipped = true;
          break;
        }
        if (chunk.done) break;
        rawChunks.push(Buffer.from(chunk.value));
        buf += dec.decode(chunk.value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const block = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          if (!block.trim()) continue;
          const ev = parseSSEBlock(block);
          events.push(ev);
          if (ev.event === "exit") { sawExit = true; break; }
        }
      }
      if (!sawExit && buf.trim()) events.push(parseSSEBlock(buf));
      try { await reader.cancel(); } catch (e) { /* ignore */ }
      return { status: res.status, contentType: ct, events: events, clipped: clipped, raw: Buffer.concat(rawChunks) };
    } finally {
      clearTimeout(timer);
    }
  }

  const TAG = "IT-" + Date.now().toString(36);
  const validTask = "集成探测 " + TAG + "（dry-run）";

  // ---------------- B0 前置：服务可达？ ----------------
  try {
    const r = await req("/api/health");
    if (r.res.status >= 500) {
      io.INFO("B0-server", "服务可达但 /api/health 返回 " + r.res.status, BASE);
    } else {
      io.PASS("B0-server", "服务可达：" + BASE + " /api/health=" + r.res.status, r.ms + "ms");
    }
  } catch (e) {
    io.INFO("B0-server", "无法连接 " + BASE + "：视为前置缺失（不算 FAIL）", String(e));
    return { prereqMissing: "服务不可达：" + BASE };
  }

  // ---------------- B1 未登录 401 ----------------
  {
    const r = await req("/api/cua/status");
    if (r.res.status === 401) io.PASS("B1-unauth-401", "未登录访问 /api/cua/status 返回 401");
    else io.FAIL("B1-unauth-401", "未登录访问 /api/cua/status 应 401，实际 " + r.res.status);
  }
  {
    const r = await jsonReq("/api/cua/runs", "POST", { task: "x", model: "y", dry_run: true });
    if (r.status === 401) io.PASS("B1b-unauth-runs-401", "未登录创建 run 返回 401");
    else io.FAIL("B1b-unauth-runs-401", "未登录创建 run 应 401，实际 " + r.status);
  }

  // ---------------- B2 登录（拿 session cookie） ----------------
  {
    const user = "itcua_" + TAG.toLowerCase().replace(/[^a-z0-9]/g, "");
    const pass = "Verify-" + TAG + "-2026";
    const body = new URLSearchParams({ username: user, password: pass }).toString();
    await req("/register", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: body });
    const r = await req("/login", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: body });
    // 表单登录成功在本项目是 302 跳转，不是 200：凡是 2xx/3xx 且拿到了 cookie 就算登录成功，
    // 真正的判据是有 cookie 后能访问需要登录的接口（C1 会验）。
    if (r.res.status >= 200 && r.res.status < 400 && jar.size > 0) {
      io.PASS("B2-login", "登录成功（HTTP " + r.res.status + "，cookie " + jar.size + " 个）", [...jar.keys()]);
    } else {
      io.FAIL("B2-login", "登录失败：HTTP " + r.res.status + "，cookie " + jar.size + " 个");
    }
  }

  // ---------------- C1 status 永远 200 ----------------
  let models = [];
  {
    const r = await req("/api/cua/status");
    if (r.res.status !== 200) {
      io.FAIL("C1-status", "GET /api/cua/status 必须永远 200，实际 " + r.res.status, r.res.status === 404 ? "接口未落地（404）" : undefined);
    } else {
      const t = await r.res.text();
      let j = null;
      try { j = JSON.parse(t); } catch (e) { j = null; }
      const missing = [];
      if (!j) missing.push("<非 JSON>");
      else {
        for (const k of ["available", "running", "run_id", "python", "cua", "env", "models", "notes"]) {
          if (!(k in j)) missing.push(k);
        }
        if (j && j.python && !("found" in j.python)) missing.push("python.found");
        if (j && j.cua && !("found" in j.cua)) missing.push("cua.found");
        if (j && j.env) {
          for (const k of ["screen_w", "screen_h", "dpi_scale"]) if (!(k in j.env)) missing.push("env." + k);
        }
        if (j && !Array.isArray(j.models)) missing.push("models[]");
        models = (j && Array.isArray(j.models)) ? j.models : [];
      }
      if (missing.length === 0) io.PASS("C1-status", "status 200 且字段齐全", { available: j.available, running: j.running, models: models.length });
      else io.FAIL("C1-status", "status 缺字段：" + missing.join(","), j);
    }
  }
  {
    const bad = models.filter(function (m) { return !m || m.vision !== true; });
    if (models.length === 0) io.WARN("C2-models-vision", "status.models 为空（探测不到视觉模型属允许行为，无法核对 vision 字段）");
    else if (bad.length === 0) io.PASS("C2-models-vision", "models 全为 vision:true（" + models.length + " 个）", models.map(function (m) { return m.name; }));
    else io.FAIL("C2-models-vision", "models 里混入非视觉模型（契约要求只列支持视觉的）", bad);
  }

  // ---------------- C12 status.models 的视觉集合（契约 §3.2，Lead 实测冻结） ----------------
  {
    const names = models.map(function (m) { return m.name; });
    const want = o.expectVisionModel || "deepseek-flash";
    const deny = o.denyModel || "deepseek-v4-pro";
    const wantEntry = models.filter(function (m) { return m.name === want; })[0];
    const why = [];
    if (names.indexOf(want) < 0) why.push("缺 " + want);
    if (names.indexOf(deny) >= 0) why.push("不应出现 " + deny);
    if (wantEntry && wantEntry.vision !== true) why.push(want + " 的 vision 必须为 true");
    if (why.length === 0) io.PASS("C12-vision-model-set", "models 含 " + want + "(vision:true) 且不含 " + deny, names);
    else io.FAIL("C12-vision-model-set", "契约 §3.2 视觉模型集合不符：" + why.join("；"), names);
  }

  // ---------------- C3 runs 入参校验（contract 2.2） ----------------
  const modelName = models.length ? models[0].name : "qwen2.5vl-16k";
  async function expect400(id, label, payload) {
    const r = await jsonReq("/api/cua/runs", "POST", payload);
    if (r.status === 400) io.PASS(id, label + " -> 400");
    else io.FAIL(id, label + " 应 400，实际 " + r.status, { body: r.text.slice(0, 200) });
  }
  await expect400("C3a-task-empty", "task 为空", { task: "", model: modelName, dry_run: true });
  await expect400("C3b-task-too-long", "task 长度 2001", { task: "x".repeat(2001), model: modelName, dry_run: true });
  await expect400("C3c-model-missing", "缺 model 字段", { task: validTask, dry_run: true });
  await expect400("C3d-model-empty", "model 为空串", { task: validTask, model: "", dry_run: true });
  await expect400("C3e-model-unknown", "model 不在 status.models", { task: validTask, model: "definitely-not-a-real-model", dry_run: true });
  await expect400("C3f-steps-low", "max_steps=0", { task: validTask, model: modelName, max_steps: 0, dry_run: true });
  await expect400("C3g-steps-high", "max_steps=201", { task: validTask, model: modelName, max_steps: 201, dry_run: true });
  await expect400("C3h-seconds-low", "max_seconds=9", { task: validTask, model: modelName, max_seconds: 9, dry_run: true });
  await expect400("C3i-seconds-high", "max_seconds=3601", { task: validTask, model: modelName, max_seconds: 3601, dry_run: true });

  // ---------------- C4 成功 201 ----------------
  let runId = null;
  {
    const r = await jsonReq("/api/cua/runs", "POST", {
      task: validTask, model: modelName, max_steps: 20, max_seconds: 600,
      dry_run: true, allow_dangerous_hotkeys: false
    });
    if (r.status === 201 && r.json) {
      runId = r.json.run_id;
      created.push(runId);
      const okId = typeof runId === "string" && /^[0-9a-f]{32}$/.test(runId);
      if (okId) io.PASS("C4a-run-201", "201，run_id 为 32 位十六进制", { run_id: runId, model: r.json.model });
      else io.FAIL("C4a-run-201", "run_id 不是 32 位十六进制：" + JSON.stringify(runId));
      if (r.json.model === modelName && r.json.task === validTask) io.PASS("C4b-run-echo", "响应回显 model/task 正确");
      else io.FAIL("C4b-run-echo", "响应回显不符", { model: r.json.model, task: r.json.task });
    } else {
      io.FAIL("C4a-run-201", "创建 run 应 201，实际 " + r.status, { body: r.text.slice(0, 300) });
    }
  }

  // ---------------- C5 并发 run -> 409 ----------------
  {
    if (!runId) {
      io.SKIP("C5-concurrent-409", "上一步没拿到 run_id，无法验证并发冲突");
    } else {
      const r = await jsonReq("/api/cua/runs", "POST", { task: validTask + " 第二个", model: modelName, dry_run: true });
      if (r.status === 409 && r.json && typeof r.json.error === "string" && r.json.error.length > 0) {
        io.PASS("C5-concurrent-409", "并发第二个 run -> 409 " + JSON.stringify(r.json.error));
      } else if (r.status === 409) {
        io.FAIL("C5-concurrent-409", "409 但缺少 error 文案", r.text.slice(0, 200));
      } else {
        io.FAIL("C5-concurrent-409", "同刻第二个 run 应 409，实际 " + r.status, { body: r.text.slice(0, 200) });
        if (r.json && r.json.run_id) created.push(r.json.run_id);
      }
    }
  }

  // ---------------- C6 SSE 事件形状（contract 2.3） ----------------
  let sse1 = null;
  if (!runId) {
    io.SKIP("C6-sse", "没有 run_id，跳过 SSE 探测");
  } else {
    sse1 = await readSSE("/api/cua/runs/" + runId + "/events", sseTimeoutMs);
    if (/text\/event-stream/i.test(sse1.contentType)) {
      io.PASS("C6a-content-type", "SSE Content-Type = " + sse1.contentType);
    } else {
      io.FAIL("C6a-content-type", "SSE Content-Type 应为 text/event-stream，实际 " + JSON.stringify(sse1.contentType));
    }
    const first = sse1.events[0];
    const firstOk = first && first.event === "start" && first.json && first.json.run_id === runId
      && typeof first.json.model === "string" && typeof first.json.task === "string";
    if (firstOk) io.PASS("C6b-start-first", "首个事件是 start 且 data 含 run_id/model/task", first.json);
    else io.FAIL("C6b-start-first", "首个事件应为 event: start + {run_id,model,task}", first || sse1.events.slice(0, 3));
    const names = sse1.events.map(function (e) { return e.event; });
    const unknown = names.filter(function (n) {
      return n !== "start" && n !== "exit" && EVENT_KINDS.indexOf(n) < 0 && EXTRA_KINDS.indexOf(n) < 0;
    });
    if (unknown.length === 0) io.PASS("C6c-kinds-known", "事件名全部落在契约 kind 集合内（" + names.length + " 个事件）", names);
    else io.FAIL("C6c-kinds-known", "出现契约外事件名：" + unknown.join(","), names);
    const nonJson = sse1.events.filter(function (e) {
      return e.event !== "start" && e.event !== "exit" && e.json === null;
    });
    if (nonJson.length === 0) io.PASS("C6d-data-json", "非 start/exit 事件的 data 都是可解析 JSON（原样透传 CUA JSON 行）");
    else io.FAIL("C6d-data-json", "有 data 不是 JSON 的事件", nonJson.slice(0, 2).map(function (e) { return e.raw.slice(0, 120); }));
    const last = sse1.events[sse1.events.length - 1];
    const lastOk = last && last.event === "exit" && last.json && typeof last.json.exit_code === "number"
      && typeof last.json.code === "string" && last.json.run_id === runId;
    if (lastOk) io.PASS("C6e-exit-last", "最后一个事件是 exit 且 data 含 exit_code/code", last.json);
    else if (sse1.clipped) io.FAIL("C6e-exit-last", "等待 " + sseTimeoutMs + "ms 仍未见 exit（流未收尾）", names);
    else io.FAIL("C6e-exit-last", "最后一个事件应为 event: exit + {run_id,exit_code,code}", last || names);
    if (names.indexOf("run_start") >= 0) io.PASS("C6f-run-start-kind", "事件流里出现 run_start（CUA JSONL 原样透传）");
    else io.WARN("C6f-run-start-kind", "事件流里没有 run_start（可能实现用别的 kind 起头，需人工确认）", names.slice(0, 5));
  }

  // ---------------- C7 已结束 run 的重连补发 ----------------
  if (!runId) {
    io.SKIP("C7-replay", "没有 run_id，跳过重连补发探测");
  } else {
    const sse2 = await readSSE("/api/cua/runs/" + runId + "/events", Math.min(sseTimeoutMs, 15000));
    const f2 = sse2.events[0];
    const l2 = sse2.events[sse2.events.length - 1];
    const names2 = sse2.events.map(function (e) { return e.event; });
    const replayOk = f2 && f2.event === "start" && l2 && l2.event === "exit";
    if (replayOk && names2.indexOf("run_start") >= 0) {
      io.PASS("C7-replay", "已结束 run 重连：立即补发 " + sse2.events.length + " 条历史并以 exit 收尾（支持刷新页面）", names2);
    } else if (replayOk) {
      io.PASS("C7a-replay-shape", "重连有 start..exit 形状（事件 " + sse2.events.length + " 条）", names2);
      io.WARN("C7b-replay-history", "重连未复现 run_start 历史事件（可能历史为空）", names2);
    } else {
      io.FAIL("C7-replay", "已结束 run 重连应补发全部历史再 exit", { status: sse2.status, names: names2.slice(0, 8) });
    }
  }

  // ---------------- C8 stop ----------------
  {
    const r = await jsonReq("/api/cua/runs/00000000000000000000000000000000/stop", "POST", {});
    if (r.status === 404) io.PASS("C8a-stop-unknown-404", "stop 不存在的 run -> 404");
    else io.FAIL("C8a-stop-unknown-404", "stop 不存在的 run 应 404，实际 " + r.status, r.text.slice(0, 200));
  }
  if (runId) {
    const r = await jsonReq("/api/cua/runs/" + runId + "/stop", "POST", {});
    if (r.status === 200 && r.json && r.json.stopped === false) {
      io.PASS("C8b-stop-finished", "stop 已结束的 run -> {stopped:false}");
    } else if (r.status === 200 && r.json && r.json.stopped === true) {
      io.WARN("C8b-stop-finished", "stop 已结束的 run 返回 stopped:true（契约写应 false，请人工确认时序）", r.json);
    } else {
      io.FAIL("C8b-stop-finished", "stop 已结束的 run 应 200 {stopped:false}，实际 " + r.status + " " + r.text.slice(0, 120));
    }
  }

  // ---------------- C9 历史 ----------------
  {
    const r = await req("/api/cua/runs");
    if (r.res.status !== 200) {
      io.FAIL("C9-history", "GET /api/cua/runs 应 200，实际 " + r.res.status);
    } else {
      const t = await r.res.text();
      let j = null;
      try { j = JSON.parse(t); } catch (e) { j = null; }
      if (!j || !Array.isArray(j.runs)) {
        io.FAIL("C9-history", "history 应为 {runs:[...]}", t.slice(0, 160));
      } else {
        const need = ["run_id", "task", "model", "status", "code", "steps", "started_at", "ended_at"];
        const miss = [];
        for (const item of j.runs) {
          for (const k of need) { if (!(k in item)) miss.push((item.run_id || "?") + "." + k); }
          if (["running", "done", "failed", "stopped"].indexOf(item.status) < 0) miss.push((item.run_id || "?") + ".status=" + item.status);
        }
        const mine = j.runs.filter(function (x) { return x.run_id === runId; })[0];
        if (miss.length === 0 && mine) io.PASS("C9-history", "history " + j.runs.length + " 条，字段与 status 枚举齐全，含本次 run", mine);
        else if (miss.length === 0) io.WARN("C9-history", "history 字段齐全，但没找到本次 run（可能内存已滚动）", j.runs.length);
        else io.FAIL("C9-history", "history 条目缺字段/枚举非法：" + miss.slice(0, 6).join(","), j.runs.slice(0, 2));
      }
    }
  }

  // ---------------- C10 代理必须校验 token ----------------
  {
    const r = await jsonReq("/internal/cua/llm/v1/chat/completions", "POST", {
      model: modelName, stream: false, messages: [{ role: "user", content: "ping" }]
    });
    if (r.status === 401) io.PASS("C10-proxy-401", "无 Bearer token 调模型代理 -> 401");
    else io.FAIL("C10-proxy-401", "无 token 调代理应 401，实际 " + r.status + "（代理不能裸奔）", r.text.slice(0, 200));
  }

  // ---------------- C13 上游空 content 必须报错（契约 §3.1，Lead 实测冻结） ----------------
  // 空 content 若被当成正常回复放行，就会变成"模型什么都没说但流程继续"的静默错误。
  if (!o.proxyToken) {
    io.SKIP("C13-empty-content", "需要代理 token 缝才能黑盒打到 /internal/cua/llm/v1/chat/completions" +
      "（契约是 Go 每轮生成随机 token）。待 cua-go 提供可注入的固定 token（例如已设 CUA_LLM_TOKEN 时不覆盖）后，" +
      "加 --proxy-token <token> --upstream-mode empty|normal 本条自动生效");
  } else {
    const PNG_1PX = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";
    const payload = {
      model: modelName, stream: false, messages: [{ role: "user", content: [
        { type: "text", text: "这张图里有什么？只回答颜色。" },
        { type: "image_url", image_url: { url: "data:image/png;base64," + PNG_1PX } }
      ] }]
    };
    const r = await jsonReq("/internal/cua/llm/v1/chat/completions", "POST", payload, { Authorization: "Bearer " + o.proxyToken });
    const mode = o.upstreamMode || "empty";
    if (mode === "normal") {
      const c = r.json && r.json.choices && r.json.choices[0] && r.json.choices[0].message && r.json.choices[0].message.content;
      if (r.status === 200 && typeof c === "string" && c.length > 0) {
        io.PASS("C13-empty-content", "正常上游：200 且 content 非空（对照组，证明链路本身通）", String(c).slice(0, 60));
      } else {
        io.FAIL("C13-empty-content", "正常上游下代理应 200 且 content 非空，实际 " + r.status, r.text.slice(0, 200));
      }
    } else {
      const msg = (r.json && r.json.error && (r.json.error.message || r.json.error)) || r.text;
      if (r.status >= 400) {
        if (String(msg).indexOf("E_MODEL_EMPTY") >= 0) {
          io.PASS("C13-empty-content", "上游空 content -> 代理报错并点名 E_MODEL_EMPTY（" + r.status + "）", String(msg).slice(0, 120));
        } else {
          io.WARN("C13-empty-content", "上游空 content 确实报错了（" + r.status + "），但错误文案没点名 E_MODEL_EMPTY", String(msg).slice(0, 160));
        }
      } else {
        io.FAIL("C13-empty-content", "上游空 content 被当成正常回复放行（" + r.status + "）—— 契约 §3.1 要求报 E_MODEL_EMPTY", r.text.slice(0, 200));
      }
    }
  }

  // ---------------- C11 data URI -> llm.Message.Images 映射（Lead 三条判据） ----------------
  {
    let ups = o.upstreamCapture;
    if (!ups && o.upstreamCapturePath) {
      // 惰性读取：抓包由本探针 C4 起的 run 产生，C11 执行时文件已写好。
      try { ups = JSON.parse(readFileSync(o.upstreamCapturePath, "utf8")); } catch (e) { ups = null; }
    }
    if (!ups) {
      io.SKIP("C11-proxy-images", "需要上游抓包：CUA_LLM_UPSTREAM 指向 docs/verify/it-upstream-capture.mjs，" +
        "并加 --upstream-capture <json>（抓包在本探针 C4 起 run 时产生，C11 执行时惰性读取）");
    } else {
      const urls = [];
      const partTypes = [];
      for (const m of (ups.messages || [])) {
        if (Array.isArray(m.content)) {
          partTypes.push(m.content.map(function (p) { return p && p.type; }));
          for (const part of m.content) {
            if (part && part.type === "image_url") urls.push(part.image_url && part.image_url.url);
          }
        } else if (Array.isArray(m.images)) {
          partTypes.push(["images[]"]);
          for (const i of m.images) urls.push(i);
        } else {
          partTypes.push(typeof m.content);
        }
      }
      let childB64 = null;
      if (created[0]) {
        const sse2 = await readSSE("/api/cua/runs/" + created[0] + "/events", Math.min(sseTimeoutMs, 20000));
        const fi = sse2.events.filter(function (e) { return e.event === "frame_image" && e.json && e.json.data_uri; })[0];
        if (fi) childB64 = String(fi.json.data_uri).split(",", 2)[1].replace(/\s+/g, "");
      }
      const upB64 = (urls.length && typeof urls[0] === "string" && urls[0].indexOf("base64,") > 0)
        ? urls[0].split("base64,", 2)[1].replace(/\s+/g, "") : null;
      if (urls.length > 0) io.PASS("C11a-image-payload", "上游收到 " + urls.length + " 个图片载荷（经 llm.Message.Images 未丢失）");
      else io.FAIL("C11a-image-payload", "上游请求里没有任何图片载荷（data URI 在代理层被丢掉）", { keys: Object.keys(ups) });
      // 逐字一致只能在**代理入口**验：子进程发出的模型输入图没有任何事件暴露它，
      // 而 frame_image 是给前端用的另一张图（640 长边 JPEG），拿它比对必然得出错误的 FAIL。
      if (!o.proxyToken) {
        io.SKIP("C11b-base64-identical", "需要 --proxy-token 才能直接打代理做已知载荷的逐字比对");
        io.SKIP("C11c-no-stream", "需要 --proxy-token");
        io.SKIP("C11d-content-parts", "需要 --proxy-token");
      } else {
        const knownB64 = "/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/wAALCAABAAEBAREA/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAD8AKp//2Q==";
        const marker = "c11-probe-" + Date.now().toString(36);
        const ingress = {
          model: "deepseek-flash",
          messages: [
            { role: "system", content: "你是测试桩" },
            { role: "user", content: [ { type: "text", text: marker }, { type: "image_url", image_url: { url: "data:image/jpeg;base64," + knownB64 } } ] }
          ]
        };
        const pr = await jsonReq("/internal/cua/llm/v1/chat/completions", "POST", ingress, { Authorization: "Bearer " + o.proxyToken });
        let cap2 = null;
        if (o.upstreamCapturePath) { try { cap2 = JSON.parse(readFileSync(o.upstreamCapturePath, "utf8")); } catch (e) { cap2 = null; } }
        if (!cap2) {
          io.FAIL("C11b-base64-identical", "代理直调后读不到上游抓包（是否上游没被指到桩上？）", { proxy_status: pr.status, path: o.upstreamCapturePath });
        } else {
          const urls2 = [];
          const types2 = [];
          for (const m of (cap2.messages || [])) {
            if (Array.isArray(m.content)) {
              types2.push(m.content.map(function (p) { return p && p.type; }));
              for (const part of m.content) if (part && part.type === "image_url") urls2.push(part.image_url && part.image_url.url);
            } else types2.push(typeof m.content);
          }
          const got2 = (urls2[0] && urls2[0].indexOf("base64,") > 0) ? urls2[0].split("base64,", 2)[1] : null;
          if (got2 === knownB64) io.PASS("C11b-base64-identical", "代理把已知 data URI 原样透传：上游收到的 base64 与提交的逐字一致（" + knownB64.length + " 字符）");
          else io.FAIL("C11b-base64-identical", "上游收到的 base64 与提交的不一致（代理可能重编码/丢包）",
            { submitted_len: knownB64.length, upstream_len: got2 ? got2.length : null, upstream_head: got2 ? got2.slice(0, 24) : null, proxy_status: pr.status });
          const hasStream2 = cap2.stream !== undefined && cap2.stream !== null && cap2.stream !== false && cap2.stream !== "false";
          if (!hasStream2) io.PASS("C11c-no-stream", "上游请求未带 stream（实测 stream=" + JSON.stringify(cap2.stream) + "）");
          else io.FAIL("C11c-no-stream", "上游请求带了 stream=" + JSON.stringify(cap2.stream));
          const flat2 = types2.length ? types2[types2.length - 1] : [];
          if (JSON.stringify(flat2) === JSON.stringify(["text", "image_url"])) io.PASS("C11d-content-parts", "多模态 content 只有 [\"text\",\"image_url\"] 且顺序正确", flat2);
          else io.FAIL("C11d-content-parts", "多模态 content 类型不符（期望 [\"text\",\"image_url\"]）", { got: flat2, all: types2.slice(0, 3) });
          const textKept = JSON.stringify(cap2.messages || []).indexOf(marker) >= 0;
          if (textKept) io.PASS("C11e-text-kept", "同一条消息里的文字部件未被图片顶掉（marker 仍在上游请求里）");
          else io.WARN("C11e-text-kept", "上游请求里找不到 marker，文字部件可能丢失");
        }
      }
      // C11a 的上下文：本条 run 的真实模型输入图（无法从事件里拿到，只能由代理入口证明）
      io.INFO("C11-note", "run 的模型输入图与 frame_image 是两张不同的图：抓包实测 " + (upB64 ? upB64.length + " 字符 PNG（原始截图）" : "n/a") + " vs 前端 frame_image（640 长边 JPEG）", { child_frame_image_len: childB64 ? childB64.length : null });
    }
  }

  // ---------------- C14 非 ASCII 往返保真（契约 §3.1 / Lead 三条验收判据） ----------------
  // 为什么必须有这一条：Go 的 encoding/json 遇到非法 UTF-8 不报错，而是静默替换成 U+FFFD，
  // 所以"能解析成 JSON"完全不代表字节是对的 —— 必须看原始字节。
  {
    // 测试向量同时含中文、全角符号与 emoji：emoji 在 GBK 里根本不存在，能额外压出"编码直接崩溃"这一类故障。
    const cnTask = "新建一个名为测试的文件夹🙂（含 emoji 与中文）";
    let idle = false;
    for (let i = 0; i < 40; i++) {
      const st = await jsonReq("/api/cua/status", "GET", undefined);
      if (st.status === 200 && st.json && st.json.running === false) { idle = true; break; }
      await new Promise(function (r) { setTimeout(r, 400); });
    }
    if (!idle) io.INFO("C14-pre", "上一个 run 迟迟不结束，C14 可能撞 409");
    const r = await jsonReq("/api/cua/runs", "POST", { task: cnTask, model: modelName, dry_run: true, max_steps: 2 });
    if (r.status !== 201) {
      io.FAIL("C14-cjk-roundtrip", "含中文的任务创建 run 应 201，实际 " + r.status, r.text.slice(0, 200));
    } else {
      const id = r.json.run_id;
      created.push(id);
      const sse = await readSSE("/api/cua/runs/" + id + "/events", Math.min(sseTimeoutMs, 20000));
      // (1) 原始字节必须能被严格 UTF-8 解码（GBK 字节也能侥幸解析成 JSON，所以要看字节）
      let utf8Ok = true;
      let utf8Err = "";
      try { new TextDecoder("utf-8", { fatal: true }).decode(sse.raw); }
      catch (e) { utf8Ok = false; utf8Err = String(e && e.message ? e.message : e).slice(0, 140); }
      if (utf8Ok) io.PASS("C14a-utf8-bytes", "SSE 原始字节可严格 UTF-8 解码（" + sse.raw.length + " 字节）");
      else io.FAIL("C14a-utf8-bytes", "SSE 原始字节不是合法 UTF-8（子进程 stdout 很可能退化成 gbk）", utf8Err);
      // (2) run_start 回显的 task 必须与提交的中文逐码点相同
      const rs = sse.events.filter(function (e) { return e.event === "run_start" && e.json; })[0];
      const got = rs ? rs.json.task : null;
      const cps = function (s) { return Array.from(String(s)).map(function (ch) { return ch.codePointAt(0); }); };
      if (got === cnTask) {
        io.PASS("C14b-task-fidelity", "run_start.task 与提交值逐码点相同（" + Array.from(cnTask).length + " 个码点），U+FFFD 计数 0");
      } else {
        io.FAIL("C14b-task-fidelity", "中文 task 经 Go->Python 子进程->SSE 往返后失真", {
          sent: cnTask,
          got: got,
          got_codepoints_head: got === null ? null : cps(got).slice(0, 16).map(function (c) { return "0x" + c.toString(16); }),
          ufffd: got === null ? null : (String(got).match(/\uFFFD/g) || []).length
        });
      }
      // (3) 全量事件里任何中文（thought 等）都不该被替换
      const mojibake = sse.events.filter(function (e) { return e.json && JSON.stringify(e.json).indexOf("\uFFFD") >= 0; });
      if (mojibake.length === 0) io.PASS("C14c-no-replacement-char", "事件流 " + sse.events.length + " 条里没有 U+FFFD 替换字符");
      else io.FAIL("C14c-no-replacement-char", "有 " + mojibake.length + " 条事件带 U+FFFD 替换字符", mojibake.slice(0, 3).map(function (e) { return e.event; }));
    }
  }
  // ---------------- 清理：绝不留下还在跑的 run ----------------
  for (const id of created) {
    if (!id) continue;
    try {
      const r = await jsonReq("/api/cua/runs/" + id + "/stop", "POST", {});
      io.INFO("Z-cleanup", "stop " + id + " -> " + r.status + " " + r.text.slice(0, 40));
    } catch (e) { io.INFO("Z-cleanup", "stop " + id + " 失败（可能已结束）", String(e)); }
  }
  return { prereqMissing: null };
}

async function main() {
  const args = process.argv.slice(2);
  const BASE = (args.indexOf("--base") >= 0 ? args[args.indexOf("--base") + 1] : null) || "http://127.0.0.1:8300";
  const sseTimeout = Number(args.indexOf("--sse-timeout") >= 0 ? args[args.indexOf("--sse-timeout") + 1] : 30000);
  let upstream = null;
  if (args.indexOf("--upstream-capture") >= 0) {
    try {
      upstream = JSON.parse(readFileSync(args[args.indexOf("--upstream-capture") + 1], "utf8"));
    } catch (e) { upstream = null; }
  }
  const arg = function (name) { return args.indexOf(name) >= 0 ? args[args.indexOf(name) + 1] : null; };
  const io = makeIO();
  console.log("# 集成层黑盒探测  base=" + BASE + "  " + new Date().toISOString());
  const out = await runProbe(BASE, io, {
    sseTimeoutMs: sseTimeout, upstreamCapture: upstream, upstreamCapturePath: arg("--upstream-capture"),
    expectVisionModel: arg("--expect-vision-model") || "deepseek-flash",
    denyModel: arg("--deny-model") || "deepseek-v4-pro",
    proxyToken: arg("--proxy-token"),
    upstreamMode: arg("--upstream-mode") || "empty"
  });
  const pass = io.results.filter(function (r) { return r.level === "PASS"; }).length;
  const fail = io.results.filter(function (r) { return r.level === "FAIL"; }).length;
  const warn = io.results.filter(function (r) { return r.level === "WARN"; }).length;
  const skip = io.results.filter(function (r) { return r.level === "SKIP"; }).length;
  console.log("");
  console.log("---- 汇总：PASS=" + pass + " FAIL=" + fail + " WARN=" + warn + " SKIP=" + skip + " ----");
  io.results.filter(function (r) { return r.level === "FAIL"; }).forEach(function (r) {
    console.log("  FAIL " + r.id + " :: " + r.msg);
  });
  if (out && out.prereqMissing) { console.log("PREREQ_MISSING: " + out.prereqMissing); process.exit(2); }
  process.exit(fail ? 1 : 0);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) { main(); }
