// stop 进程树复验：子进程 + 孙进程都必须消失，且不留残留。
import { readFileSync } from "node:fs";
const BASE = process.argv[2];
const jar = new Map();
async function req(path, o) {
  const opt = o || {};
  const headers = Object.assign({}, opt.headers || {});
  if (jar.size) headers["Cookie"] = [...jar.entries()].map(function (kv) { return kv[0] + "=" + kv[1]; }).join("; ");
  const res = await fetch(BASE + path, Object.assign({}, opt, { headers: headers, redirect: "manual" }));
  const set = res.headers.getSetCookie ? res.headers.getSetCookie() : [];
  for (const c of set) { const p = c.split(";")[0]; const i = p.indexOf("="); if (i > 0) jar.set(p.slice(0, i).trim(), p.slice(i + 1).trim()); }
  return res;
}
const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };
const rd = function (p) { try { return Number(readFileSync(p, "utf8").trim()); } catch (e) { return null; } };
const alive = function (p) { if (!p) return "no-pid"; try { process.kill(p, 0); return true; } catch (e) { return false; } };
const GC = process.env.GC_PIDFILE, DP = process.env.DIRECT_PIDFILE;
const u = "ittk" + Date.now().toString(36);
const form = { "Content-Type": "application/x-www-form-urlencoded" };
await req("/register", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
await req("/login", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
const r = await (await req("/api/cua/runs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ task: "tree kill probe", model: "deepseek-flash", max_steps: 1, dry_run: true }) })).json();
console.log("run_id = " + r.run_id);
await sleep(3500);
const gp = rd(GC), dp = rd(DP);
console.log("BEFORE stop : grandchild pid=" + gp + " alive=" + alive(gp) + " | direct child pid=" + dp + " alive=" + alive(dp));
await req("/api/cua/runs/" + r.run_id + "/stop", { method: "POST" });
await sleep(3000);
const ga = alive(gp), da = alive(dp);
console.log("AFTER  stop : grandchild pid=" + gp + " alive=" + ga + " | direct child pid=" + dp + " alive=" + da);
console.log((da === false && ga === false ? "PASS" : "FAIL") + " stop 收掉了整棵进程树（子 + 孙都 gone）");
