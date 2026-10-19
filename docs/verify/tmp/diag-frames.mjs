// 取证：8300 上那个不是我的实例，它跑出来的 run 到底是 mock 还是真实屏？
const BASE = "http://127.0.0.1:8300";
const rid = process.argv[2];
const jar = new Map();
function jarHeader() { return [...jar.entries()].map(function (kv) { return kv[0] + "=" + kv[1]; }).join("; "); }
async function req(path, o) {
  const opt = o || {};
  const headers = Object.assign({}, opt.headers || {});
  if (jar.size) headers["Cookie"] = jarHeader();
  const res = await fetch(BASE + path, Object.assign({}, opt, { headers: headers }));
  for (const c of (res.headers.getSetCookie ? res.headers.getSetCookie() : [])) {
    const p = c.split(";")[0]; const i = p.indexOf("="); if (i > 0) jar.set(p.slice(0, i).trim(), p.slice(i + 1).trim());
  }
  return res;
}
const u = "itfx" + Date.now().toString(36);
const form = { "Content-Type": "application/x-www-form-urlencoded" };
await req("/register", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
await req("/login", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
const res = await req("/api/cua/runs/" + rid + "/events");
const text = await res.text();
const frames = [];
for (const block of text.split("\n\n")) {
  const lines = block.split("\n");
  const ev = (lines[0] || "").replace(/^event:\s*/, "");
  const data = lines.filter(function (l) { return l.indexOf("data:") === 0; }).map(function (l) { return l.slice(5).trim(); }).join("");
  if (ev === "frame" || ev === "frame_image") { try { frames.push({ ev: ev, d: JSON.parse(data) }); } catch (e) {} }
}
console.log("run " + rid + "  status=" + res.status + "  frame事件 " + frames.length + " 条");
for (const f of frames.slice(0, 3)) {
  const d = f.d;
  console.log("  " + f.ev + ": " + (d.image ? ("image=" + d.image.width + "x" + d.image.height) : "")
    + (d.media_type ? (" " + d.media_type + " " + d.width + "x" + d.height) : "")
    + (d.data_uri ? (" data_uri_len=" + d.data_uri.length) : ""));
}
