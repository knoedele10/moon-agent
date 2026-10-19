// 安全取证：这个实例的子进程到底是 mock（合成屏 640x360）还是真实屏（本机 2560x1600）？
const BASE = "http://127.0.0.1:8317";
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
const st = await (await req("/api/cua/status")).json();
console.log("status.env = " + JSON.stringify(st.env) + "  (真实屏宽高)")
const u = "itmk" + Date.now().toString(36);
const form = { "Content-Type": "application/x-www-form-urlencoded" };
await req("/register", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
await req("/login", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
const r = await (await req("/api/cua/runs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ task: "mock 取证", model: "qwen2.5vl:7b", max_steps: 1, dry_run: true }) })).json();
console.log("run_id = " + r.run_id);
const res = await req("/api/cua/runs/" + r.run_id + "/events");
const text = await res.text();
for (const block of text.split("\n\n")) {
  const lines = block.split("\n");
  const ev = (lines[0] || "").replace(/^event:\s*/, "");
  if (ev !== "frame") continue;
  const data = lines.filter(function (l) { return l.indexOf("data:") === 0; }).map(function (l) { return l.slice(5).trim(); }).join("");
  const j = JSON.parse(data);
  console.log("frame.image = " + JSON.stringify(j.image) + "  (640x360 = mock 合成屏) " + (j.image.width === 640 && j.image.height === 360 ? "=> MOCK" : "=> 非 640x360"));
  break;
}
await req("/api/cua/runs/" + r.run_id + "/stop", { method: "POST" });
