// task-24 独立复验：模型代理的 max_tokens 下限 16384。
// 不采信单测：自己起桩、自己发请求、自己看**上游真正收到**的 max_tokens。
// 关键对照：既要有"小值被抬上来"，也要有"大值不被压回去"——否则只能说明它把所有值都写成 16384。
import { readFileSync } from "node:fs";
const BASE = process.argv[2];
const CAPTURE = process.argv[3];
const TOKEN = process.argv[4] || "verify-fixed-token-2026";
const jar = new Map();
async function req(p, o) {
  const opt = o || {}; const h = Object.assign({}, opt.headers || {});
  if (jar.size) h["Cookie"] = [...jar.entries()].map(function (kv) { return kv[0] + "=" + kv[1]; }).join("; ");
  const r = await fetch(BASE + p, Object.assign({}, opt, { headers: h, redirect: "manual" }));
  const s = r.headers.getSetCookie ? r.headers.getSetCookie() : [];
  for (const c of s) { const q = c.split(";")[0]; const i = q.indexOf("="); if (i > 0) jar.set(q.slice(0, i).trim(), q.slice(i + 1).trim()); }
  return r;
}
const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };
function captured(marker) {
  let txt = "";
  try { txt = readFileSync(CAPTURE + ".all.jsonl", "utf8"); } catch (e) { return null; }
  const lines = txt.split("\n").filter(Boolean);
  for (let i = lines.length - 1; i >= 0; i--) {
    try { const o = JSON.parse(lines[i]);
      if (JSON.stringify(o.messages || []).indexOf(marker) >= 0) return o;
    } catch (e) {}
  }
  return null;
}
const u = "itmt" + Date.now().toString(36);
const form = { "Content-Type": "application/x-www-form-urlencoded" };
await req("/register", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
await req("/login", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
const run = await (await req("/api/cua/runs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ task: "task-24 max_tokens 探针", model: "deepseek-flash", max_steps: 20, dry_run: true }) })).json();
if (!run.run_id) { console.log("建 run 失败: " + JSON.stringify(run)); process.exit(2); }
console.log("run_id=" + run.run_id + "（用它的 token 直连代理，模拟子进程发的请求）");
const vectors = [8, 1024, 16383, 16384, 20000, undefined];
const rows = [];
let fail = 0;
for (let i = 0; i < vectors.length; i++) {
  const v = vectors[i];
  const marker = "MARKER-" + i + "-" + Math.random().toString(36).slice(2, 8);
  const body = { model: "deepseek-flash", messages: [{ role: "user", content: "复验 " + marker }] };
  if (v !== undefined) body.max_tokens = v;
  const res = await req("/internal/cua/llm/v1/chat/completions", { method: "POST",
    headers: { "Content-Type": "application/json", Authorization: "Bearer " + TOKEN }, body: JSON.stringify(body) });
  const status = res.status; const text = await res.text();
  let got = null, tries = 0;
  while (tries < 25 && !got) { got = captured(marker); if (!got) { await sleep(120); tries++; } }
  const seen = got ? got.max_tokens : "(桩没收到)";
  const expect = (v === undefined || v < 16384) ? 16384 : v;
  const ok = got && seen === expect;
  if (!ok) fail++;
  rows.push({ 发送: v === undefined ? "(不带)" : v, 上游收到: seen, 期望: expect, HTTP: status, 回复长度: text.length, 判定: ok ? "PASS" : "FAIL" });
}
await req("/api/cua/runs/" + run.run_id + "/stop", { method: "POST" });
for (const r of rows) console.log(JSON.stringify(r));
console.log((fail === 0 ? "PASS" : "FAIL") + " task-24 max_tokens 下限：6 个向量，失败 " + fail);
console.log("对照说明：16384 原样、20000 原样 -> 是「下限」而不是「一律写死 16384」；8/1024/16383/缺省 -> 抬到 16384。");
