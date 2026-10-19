// 集成层探测的负向对照（verifier 自写）：用一个"故意违规"的桩服务验证 integration-probe.mjs 真的能抓到问题。
// 用法：node docs/verify/integration-negative-control.mjs
// 退出码：0=每个变异都被抓到且干净桩全 PASS；1=有变异没被抓到（或干净桩出现误报）
import { createServer } from "node:http";
import { pathToFileURL } from "node:url";
import { runProbe } from "./integration-probe.mjs";

const FINISH_AFTER_MS = 1200;

function readBody(req) {
  return new Promise(function (resolve) {
    let b = "";
    req.on("data", function (c) { b += c; });
    req.on("end", function () { resolve(b); });
  });
}

export function startStub(mut) {
  const state = { runs: new Map(), runningId: null, seq: 0 };
  const server = createServer(function (req, res) {
    handle(req, res, mut, state).catch(function (e) {
      try { res.writeHead(500); res.end(JSON.stringify({ error: String(e) })); } catch (e2) { /* ignore */ }
    });
  });
  return new Promise(function (resolve) {
    server.listen(0, "127.0.0.1", function () { resolve({ server: server, port: server.address().port }); });
  });
}

async function handle(req, res, mut, state) {
  const url = new URL(req.url, "http://127.0.0.1");
  const p = url.pathname;
  function json(code, obj, extra) {
    res.writeHead(code, Object.assign({ "Content-Type": "application/json" }, extra || {}));
    res.end(JSON.stringify(obj));
  }
  if (p === "/api/health") { json(200, { status: "ok" }); return; }
  if (p === "/register" || p === "/login") {
    await readBody(req);
    res.writeHead(200, { "Set-Cookie": "session=stub; Path=/" });
    res.end("ok");
    return;
  }
  if (p === "/internal/cua/llm/v1/chat/completions") {
    await readBody(req);
    if (mut === "proxy_no_token_ok") { json(200, { choices: [{ message: { role: "assistant", content: "ok" } }] }); return; }
    json(401, { error: { message: "缺少 Bearer token" } });
    return;
  }
  if (p.indexOf("/api/cua/") !== 0) { json(404, { error: "not found" }); return; }
  const cookie = req.headers.cookie || "";
  if (!/(^|;\s*)session=/.test(cookie)) { json(401, { error: "unauthorized" }); return; }

  // 干净桩按契约 §3.2 给出：含 deepseek-flash(视觉)、不含 deepseek-v4-pro
  const models = (mut === "models_no_vision")
    ? [{ name: "text-only-model", backend: "ollama", vision: false }]
    : [{ name: "deepseek-flash", backend: "deepseek", vision: true },
       { name: "qwen2.5vl-16k", backend: "ollama", vision: true }];
  if (mut === "models_include_v4pro") models.push({ name: "deepseek-v4-pro", backend: "deepseek", vision: false });
  if (mut === "models_missing_expected") models.shift();

  if (p === "/api/cua/status") {
    if (mut === "status_500") { json(503, { error: "stub 故意 503" }); return; }
    json(200, {
      available: true, running: !!state.runningId, run_id: state.runningId || "",
      python: { found: true, version: "3.13.13" },
      cua: { found: true, version: "0.1.0" },
      env: { screen_w: 2560, screen_h: 1600, dpi_scale: 1.5 },
      models: models, notes: ["stub"]
    });
    return;
  }

  if (p === "/api/cua/runs" && req.method === "GET") {
    const runs = [...state.runs.values()].map(function (r) {
      const o = { run_id: r.run_id, task: r.task, model: r.model, status: r.status, code: r.code, steps: r.steps, started_at: r.started_at, ended_at: r.ended_at };
      if (mut === "history_no_started_at") delete o.started_at;
      return o;
    });
    json(200, { runs: runs });
    return;
  }

  if (p === "/api/cua/runs" && req.method === "POST") {
    let body = {};
    try { body = JSON.parse(await readBody(req)); } catch (e) { body = {}; }
    if (typeof body.task !== "string" || body.task.length === 0) { json(400, { error: "task 必填" }); return; }
    if (body.task.length > 2000) { json(400, { error: "task 过长" }); return; }
    if (mut !== "accept_empty_model") {
      if (typeof body.model !== "string" || body.model.length === 0) { json(400, { error: "model 必填" }); return; }
    }
    if (mut !== "accept_unknown_model" && mut !== "accept_empty_model" && typeof body.model === "string") {
      const known = models.some(function (m) { return m.name === body.model; });
      if (!known) { json(400, { error: "model 不存在" }); return; }
    }
    if (mut !== "accept_bad_budget") {
      const ms = body.max_steps === undefined ? 20 : body.max_steps;
      if (typeof ms !== "number" || ms < 1 || ms > 200) { json(400, { error: "max_steps 越界" }); return; }
      const sec = body.max_seconds === undefined ? 600 : body.max_seconds;
      if (typeof sec !== "number" || sec < 10 || sec > 3600) { json(400, { error: "max_seconds 越界" }); return; }
    }
    if (state.runningId && mut !== "concurrent_201") { json(409, { error: "已有任务在运行" }); return; }
    state.seq += 1;
    const id = String(state.seq).padStart(32, "0");
    const run = {
      run_id: id, task: body.task, model: body.model, status: "running", code: "", steps: 1,
      started_at: new Date().toISOString(), ended_at: null, waiters: [],
      events: [
        { event: "run_start", data: { kind: "run_start", protocol: "cua/1", task: mut === "cua_task_mojibake" ? Array.from(String(body.task)).map(function (c) { return c.codePointAt(0) > 127 ? "\uFFFD" : c; }).join("") : body.task } },
        { event: "frame", data: { kind: "frame", step: 0, image: { width: 1472, height: 828 } } },
        { event: "run_end", data: { kind: "run_end", ok: true, steps: 1 } }
      ]
    };
    state.runs.set(id, run);
    state.runningId = id;
    setTimeout(function () {
      if (run.status === "running") run.status = "done";
      run.ended_at = new Date().toISOString();
      if (state.runningId === id) state.runningId = null;
      const ws = run.waiters;
      run.waiters = [];
      ws.forEach(function (w) { w(); });
    }, FINISH_AFTER_MS);
    json(201, { run_id: id, model: body.model, task: body.task });
    return;
  }

  const mEvents = p.match(/^\/api\/cua\/runs\/([0-9a-f]+)\/events$/);
  if (mEvents) {
    const run = state.runs.get(mEvents[1]);
    if (!run) { json(404, { error: "run 不存在" }); return; }
    if (mut === "no_replay" && run.status !== "running") {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.end();
      return;
    }
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
    function send(ev, data) { res.write("event: " + ev + "\ndata: " + JSON.stringify(data) + "\n\n"); }
    if (mut === "cua_task_gbk_bytes") {
      // 复现真 bug 的字节形态：run_start 的 data 按 GBK 落字节（0xb2 0xe2 0xca 0xd4 = "测试"）。
      // 注意：整条事件仍然是"能解析的 JSON"（坏字节被替换成 U+FFFD 后照样 parse），
      // 所以只有看原始字节的 C14a/C14b 能抓住它 —— 这正是 C14 存在的理由。
      const head = Buffer.from("event: run_start\ndata: {\"kind\":\"run_start\",\"protocol\":\"cua/1\",\"task\":\"", "utf8");
      const gbk = Buffer.from([0xb2, 0xe2, 0xca, 0xd4]);
      const tail = Buffer.from("\"}\n\n", "utf8");
      res.write(Buffer.concat([head, gbk, tail]));
    }
    if (mut !== "sse_omit_start") send("start", { run_id: run.run_id, model: run.model, task: run.task });
    for (const e of run.events) send(e.event, e.data);
    if (run.status === "running") {
      await new Promise(function (r) { run.waiters.push(r); });
    }
    send("exit", mut === "sse_no_exit_code"
      ? { run_id: run.run_id }
      : { run_id: run.run_id, exit_code: 0, code: "" });
    if (process.env.IT_STUB_DEBUG) console.error("[stub] after-exit-write writableEnded=" + res.writableEnded);
    res.end();
    return;
  }

  const mStop = p.match(/^\/api\/cua\/runs\/([0-9a-f]+)\/stop$/);
  if (mStop && req.method === "POST") {
    await readBody(req);
    const run = state.runs.get(mStop[1]);
    if (!run) {
      if (mut === "stop_200_for_unknown") { json(200, { stopped: false }); return; }
      json(404, { error: "run 不存在" });
      return;
    }
    if (run.status === "running") {
      run.status = "stopped";
      run.ended_at = new Date().toISOString();
      if (state.runningId === run.run_id) state.runningId = null;
      const ws = run.waiters;
      run.waiters = [];
      ws.forEach(function (w) { w(); });
      json(200, { stopped: true });
      return;
    }
    json(200, { stopped: false });
    return;
  }

  json(404, { error: "not found" });
}

const MUTATIONS = [
  { name: "status_500", expect: ["C1-status"] },
  { name: "models_no_vision", expect: ["C2-models-vision"] },
  { name: "models_include_v4pro", expect: ["C12-vision-model-set"] },
  { name: "models_missing_expected", expect: ["C12-vision-model-set"] },
  { name: "accept_empty_model", expect: ["C3d-model-empty"] },
  { name: "accept_unknown_model", expect: ["C3e-model-unknown"] },
  { name: "accept_bad_budget", expect: ["C3f-steps-low", "C3g-steps-high", "C3h-seconds-low", "C3i-seconds-high"] },
  { name: "concurrent_201", expect: ["C5-concurrent-409"] },
  { name: "sse_omit_start", expect: ["C6b-start-first"] },
  { name: "sse_no_exit_code", expect: ["C6e-exit-last"] },
  { name: "no_replay", expect: ["C7-replay"] },
  { name: "stop_200_for_unknown", expect: ["C8a-stop-unknown-404"] },
  { name: "history_no_started_at", expect: ["C9-history"] },
  { name: "proxy_no_token_ok", expect: ["C10-proxy-401"] },
  { name: "cua_task_mojibake", expect: ["C14b-task-fidelity"] },
  { name: "cua_task_gbk_bytes", expect: ["C14a-utf8-bytes"] }
];

function silentIO() {
  const results = [];
  function rec(id, level, msg, detail) { results.push({ id: id, level: level, msg: msg, detail: detail }); }
  return {
    results: results,
    PASS: function (id, m, d) { rec(id, "PASS", m, d); },
    FAIL: function (id, m, d) { rec(id, "FAIL", m, d); },
    WARN: function (id, m, d) { rec(id, "WARN", m, d); },
    SKIP: function (id, m, d) { rec(id, "SKIP", m, d); },
    INFO: function (id, m, d) { rec(id, "INFO", m, d); }
  };
}

async function runCase(mut, expect) {
  const stub = await startStub(mut);
  const io = silentIO();
  let out = null;
  try {
    out = await runProbe("http://127.0.0.1:" + stub.port, io, { sseTimeoutMs: 8000 });
  } finally {
    await new Promise(function (r) { stub.server.close(function () { r(); }); });
  }
  const fails = io.results.filter(function (r) { return r.level === "FAIL"; }).map(function (r) { return r.id; });
  const pass = io.results.filter(function (r) { return r.level === "PASS"; }).length;
  const missing = expect.filter(function (id) { return fails.indexOf(id) < 0; });
  return { fails: fails, pass: pass, missing: missing, prereq: out && out.prereqMissing };
}

async function main() {
  console.log("# 集成层探测的负向对照（干净桩应全 PASS；每个变异必须被抓住）");
  let bad = 0;
  const clean = await runCase(null, []);
  if (clean.prereq) { console.log("PREREQ_MISSING: " + clean.prereq); process.exit(2); }
  if (clean.fails.length === 0) {
    console.log("[PASS] clean-stub          干净桩上探测全绿（PASS=" + clean.pass + " FAIL=0）—— 探测不会乱叫");
  } else {
    bad += 1;
    console.log("[FAIL] clean-stub          干净桩上出现误报 FAIL=" + JSON.stringify(clean.fails));
  }
  for (const m of MUTATIONS) {
    const r = await runCase(m.name, m.expect);
    if (r.missing.length === 0) {
      console.log("[PASS] mutation=" + m.name + "  被抓住（FAIL=" + JSON.stringify(r.fails.slice(0, 5)) + "，PASS=" + r.pass + "）");
    } else {
      bad += 1;
      console.log("[FAIL] mutation=" + m.name + "  没被抓到，期望 " + JSON.stringify(m.expect) + "，实际 FAIL=" + JSON.stringify(r.fails.slice(0, 8)));
    }
  }
  console.log("");
  console.log("---- 汇总：变异 " + MUTATIONS.length + " 项，未被抓/误报 " + bad + " 项 ----");
  process.exit(bad ? 1 : 0);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) { main(); }
