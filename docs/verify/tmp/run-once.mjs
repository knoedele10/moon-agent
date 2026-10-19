// 起一个 run 并等它结束（用于观察子进程/验证 §5 sidecar 优先）
const BASE = process.argv[2];
const jar = new Map();
async function req(p, o) {
  const opt = o || {}; const h = Object.assign({}, opt.headers || {});
  if (jar.size) h["Cookie"] = [...jar.entries()].map(function (kv) { return kv[0] + "=" + kv[1]; }).join("; ");
  const r = await fetch(BASE + p, Object.assign({}, opt, { headers: h, redirect: "manual" }));
  const s = r.headers.getSetCookie ? r.headers.getSetCookie() : [];
  for (const c of s) { const q = c.split(";")[0]; const i = q.indexOf("="); if (i > 0) jar.set(q.slice(0, i).trim(), q.slice(i + 1).trim()); }
  return r;
}
const u = "itrun" + Date.now().toString(36);
const f = { "Content-Type": "application/x-www-form-urlencoded" };
await req("/register", { method: "POST", headers: f, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
await req("/login", { method: "POST", headers: f, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
const st = await (await req("/api/cua/status")).json();
console.log("notes=" + JSON.stringify(st.notes));
const r = await (await req("/api/cua/runs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ task: "i5 sidecar probe", model: "deepseek-flash", max_steps: 2, dry_run: true }) })).json();
console.log("run_id=" + r.run_id);
for (let i = 0; i < 40; i++) {
  const h = await (await req("/api/cua/runs")).json();
  const list = (h && h.runs) || h || [];
  const mine = (Array.isArray(list) ? list : []).filter(function (x) { return x.run_id === r.run_id; })[0];
  if (mine && mine.status !== "running") { console.log("final=" + JSON.stringify(mine)); break; }
  await new Promise(function (res) { setTimeout(res, 500); });
}
