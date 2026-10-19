// 内存上限复验：每 run 最多留最近 10 条 frame_image；非图片事件全留；实时广播不受影响。
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
async function readAll(path, timeoutMs) {
  const ac = new AbortController();
  const timer = setTimeout(function () { try { ac.abort(); } catch (e) {} }, timeoutMs);
  const events = [];
  try {
    const res = await fetch(BASE + path, { headers: { Cookie: [...jar.entries()].map(function (kv) { return kv[0] + "=" + kv[1]; }).join("; ") }, signal: ac.signal });
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    while (true) {
      let chunk;
      try { chunk = await reader.read(); } catch (e) { break; }
      if (chunk.done) break;
      buf += dec.decode(chunk.value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const block = buf.slice(0, idx); buf = buf.slice(idx + 2);
        if (!block.trim()) continue;
        const lines = block.split("\n");
        const ev = (lines[0] || "").replace(/^event:\s*/, "");
        const data = lines.filter(function (l) { return l.indexOf("data:") === 0; }).map(function (l) { return l.slice(5).trim(); }).join("");
        let j = null; try { j = JSON.parse(data); } catch (e) {}
        events.push({ ev: ev, j: j });
        if (ev === "exit") { try { await reader.cancel(); } catch (e) {} return events; }
      }
    }
  } finally { clearTimeout(timer); }
  return events;
}
const u = "itmem" + Date.now().toString(36);
const form = { "Content-Type": "application/x-www-form-urlencoded" };
await req("/register", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
await req("/login", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verify-2026-abc" }).toString() });
const r = await (await req("/api/cua/runs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ task: "内存上限复验", model: "deepseek-flash", max_steps: 20, dry_run: true }) })).json();
if (!r.run_id) { console.log("create failed: " + JSON.stringify(r)); process.exit(2); }
console.log("run_id = " + r.run_id);
const live = await readAll("/api/cua/runs/" + r.run_id + "/events", 90000);
const rep = await readAll("/api/cua/runs/" + r.run_id + "/events", 30000);
const cnt = function (evs, k) { return evs.filter(function (e) { return e.ev === k; }).length; };
const steps = function (evs) { return evs.filter(function (e) { return e.ev === "frame_image" && e.j; }).map(function (e) { return e.j.step; }); };
console.log("LIVE   : frame=" + cnt(live, "frame") + " frame_image=" + cnt(live, "frame_image") + " 事件总数=" + live.length);
console.log("REPLAY : frame=" + cnt(rep, "frame") + " frame_image=" + cnt(rep, "frame_image") + " 事件总数=" + rep.length);
console.log("LIVE 图片 step = " + JSON.stringify(steps(live).slice(-12)));
console.log("REPLAY 图片 step = " + JSON.stringify(steps(rep)));
const re = live.filter(function (e) { return e.ev === "run_end"; })[0];
console.log("run_end = " + JSON.stringify(re && re.j));
console.log("事件轨迹(前 24) = " + JSON.stringify(live.slice(0, 24).map(function (e) { return e.ev + (e.j && e.j.step !== undefined ? ":" + e.j.step : ""); })));
const liveImg = cnt(live, "frame_image"); const repImg = cnt(rep, "frame_image");
const nonImgLive = live.length - liveImg; const nonImgRep = rep.length - repImg;
console.log((liveImg > 10 ? "PASS" : "FAIL") + " 实时广播未被裁剪（live frame_image=" + liveImg + " > 10）");
console.log((repImg <= 10 ? "PASS" : "FAIL") + " 已存快照最多 10 条 frame_image（replay=" + repImg + "）");
console.log((nonImgLive === nonImgRep ? "PASS" : "FAIL") + " 非图片事件全留（live=" + nonImgLive + " replay=" + nonImgRep + "）");
const ls = steps(live); const rs = steps(rep);
const expectTail = ls.slice(-repImg);
console.log((JSON.stringify(expectTail) === JSON.stringify(rs) ? "PASS" : "FAIL") + " 保留的是最近 " + repImg + " 条（step=" + JSON.stringify(expectTail) + "）");
await req("/api/cua/runs/" + r.run_id + "/stop", { method: "POST" });
