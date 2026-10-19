// 视觉模型身份的因果取证（verifier 自写）
// 1) 先把 Ollama 里的模型全部卸载 -> /api/ps 应为空（干净基线）
// 2) 拿一张**全新字节**的图片上传到「KB_VISION_MODEL 指向不存在模型」的实例(8301)
//    -> 必须 400，且错误文案里出现那个不存在的模型名（证明入库真的读 config 的视觉模型）
// 3) 再拿另一张全新字节的图片上传到正常实例(8300) -> 201，随后 /api/ps 里应出现 qwen2.5vl:7b
//    （证明是这次入库自己把视觉模型拉起来的，不是之前驻留的）
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";

const GOOD = (process.argv.includes("--good") ? process.argv[process.argv.indexOf("--good") + 1] : null) || "http://127.0.0.1:8300";
const BAD = (process.argv.includes("--bad") ? process.argv[process.argv.indexOf("--bad") + 1] : null) || "http://127.0.0.1:8301";
const OLLAMA = "http://127.0.0.1:11434";
const log = [];
const say = (s) => { console.log(s); log.push(s); };

async function ps() {
  const r = await fetch(OLLAMA + "/api/ps");
  const j = await r.json();
  return (j.models || []).map((m) => m.name || m.model);
}
async function unload(model) {
  await fetch(OLLAMA + "/api/generate", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model, keep_alive: 0 }),
  });
}
function makeJar() {
  const jar = new Map();
  const hdr = () => (jar.size ? { Cookie: [...jar.entries()].map(([k, v]) => k + "=" + v).join("; ") } : {});
  return { jar, hdr };
}
async function session(base, tag) {
  const { jar, hdr } = makeJar();
  const u = "vrf_ct_" + tag + "_" + Date.now().toString(36);
  const form = { "Content-Type": "application/x-www-form-urlencoded" };
  await fetch(base + "/register", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verifier123" }).toString(), redirect: "manual" });
  const r = await fetch(base + "/login", { method: "POST", headers: form, body: new URLSearchParams({ username: u, password: "Verifier123" }).toString(), redirect: "manual" });
  for (const c of (r.headers.getSetCookie ? r.headers.getSetCookie() : [])) {
    const [p] = c.split(";"); const i = p.indexOf("="); jar.set(p.slice(0, i).trim(), p.slice(i + 1).trim());
  }
  return { base, user: u, hdr, cookieCount: jar.size };
}
async function upload(s, png) {
  const buf = readFileSync(png);
  const fd = new FormData();
  fd.append("file", new Blob([buf]), png.split(/[\\/]/).pop());
  const t0 = Date.now();
  const r = await fetch(s.base + "/api/kb/documents", { method: "POST", headers: s.hdr(), body: fd, redirect: "manual" });
  const ms = Date.now() - t0;
  const text = await r.text();
  return { status: r.status, ms, text };
}

async function main() {
  say("# 视觉模型身份因果取证  时间 " + new Date().toISOString());
  for (const m of ["qwen2.5vl:7b", "bge-m3:latest"]) await unload(m);
  await new Promise((r) => setTimeout(r, 1500));
  const base0 = await ps();
  say("卸载后 /api/ps = " + JSON.stringify(base0) + (base0.length === 0 ? "  [干净基线]" : "  [仍有驻留]"));

  // 2) 对照实验：伪造视觉模型名
  const sBad = await session(BAD, "bad");
  say(`对照实例 ${BAD} 登录 ${sBad.user}（cookie ${sBad.cookieCount}）`);
  const upBad = await upload(sBad, "docs/verify/tmp/zebra-variant-bad.png");
  say(`对照实例上传 -> HTTP ${upBad.status}（${upBad.ms}ms）响应：${upBad.text.slice(0, 300)}`);
  const badNamed = upBad.text.includes("definitely-not-a-real-model");
  const idBad = (() => { try { return JSON.parse(upBad.text).id; } catch { return null; } })();
  if (upBad.status >= 400 && badNamed) say("[PASS] 对照实验：视觉得到确实用的是 config 里的模型（错误文案点名了伪造的模型名）");
  else if (upBad.status === 200 && idBad) say(`[WARN] 对照实例竟然入库成功（可能命中去重）id=${idBad}；该实验不能证明模型来自 config`);
  else say(`[WARN] 对照实验结论不明确：status=${upBad.status}`);

  // 3) 正常实例：全新字节的图片，观察是不是这次入库把视觉模型拉起来的
  const sGood = await session(GOOD, "good");
  say(`正常实例 ${GOOD} 登录 ${sGood.user}（cookie ${sGood.cookieCount}）`);
  say("上传前 /api/ps = " + JSON.stringify(await ps()));
  const upGood = await upload(sGood, "docs/verify/tmp/zebra-variant-good.png");
  say(`正常实例上传 -> HTTP ${upGood.status}（${upGood.ms}ms）响应：${upGood.text.slice(0, 300)}`);
  const after = await ps();
  say("上传后 /api/ps = " + JSON.stringify(after));
  let idGood = null; try { idGood = JSON.parse(upGood.text).id; } catch {}
  if (upGood.status === 201 && after.some((n) => String(n).includes("qwen2.5vl")))
    say("[PASS] 因果证据：这次入库自己把 qwen2.5vl 拉起来了（上传前为空、上传后驻留），视觉链路确实由 qwen2.5vl 完成");
  else if (upGood.status === 200 && idGood)
    say("[WARN] 命中去重，未重新调用视觉模型，无法据此推断");
  else say(`[WARN] 未取得因果证据：status=${upGood.status}, ps=${JSON.stringify(after)}`);

  say("本轮留下的文档 id：bad=" + idBad + " good=" + idGood + "（收尾用 SQL 清理）");
  mkdirSync("docs/verify/tmp", { recursive: true });
  writeFileSync("docs/verify/tmp/rag-vision-contrast.log", log.join("\n") + "\n");
}
main().catch((e) => { console.error("脚本异常：", e); process.exit(2); });
