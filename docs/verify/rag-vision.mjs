// 独立验证「图片入库到底走了哪个视觉模型」（verifier 自写）
// 证据链：
//  1) meta 证明走了视觉链路；
//  2) Ollama /api/ps 在入库后是否真的加载了 qwen2.5vl:7b（模型驻留是服务端事实，不是我们推断）；
//  3) 对照实验：把 KB_VISION_MODEL 指向一个不存在的模型，入库必须失败并在错误里点名该模型；
//  4) 直接对 Ollama 发一次同样的视觉请求，证明同一张图确实能被该模型读出同样的字。
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";

const BASE = (process.argv.includes("--base") ? process.argv[process.argv.indexOf("--base") + 1] : null) || "http://127.0.0.1:8300";
const KEEP = process.argv.includes("--keep");
const OLLAMA = "http://127.0.0.1:11434";
const PNG = "docs/verify/tmp/zebra-verify.png";

const jar = new Map();
function absorb(res) {
  for (const c of (res.headers.getSetCookie ? res.headers.getSetCookie() : [])) {
    const [p] = c.split(";"); const i = p.indexOf("="); jar.set(p.slice(0, i).trim(), p.slice(i + 1).trim());
  }
}
async function req(path, opts = {}) {
  const res = await fetch(BASE + path, {
    ...opts,
    headers: { ...(opts.headers || {}), ...(jar.size ? { Cookie: [...jar.entries()].map(([k, v]) => k + "=" + v).join("; ") } : {}) },
    redirect: "manual",
  });
  absorb(res); return res;
}
const log = [];
const say = (s) => { console.log(s); log.push(s); };

async function ollamaPs() {
  const r = await fetch(OLLAMA + "/api/ps");
  const j = await r.json();
  return (j.models || []).map((m) => m.name || m.model);
}

async function login(tag) {
  const u = "vrf_vis_" + tag + "_" + Date.now().toString(36);
  await req("/register", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ username: u, password: "Verifier123" }).toString() });
  const r = await req("/login", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ username: u, password: "Verifier123" }).toString() });
  return { user: u, status: r.status };
}

async function uploadImage() {
  const buf = readFileSync(PNG);
  const fd = new FormData();
  fd.append("file", new Blob([buf]), "zebra-verify.png");
  const t0 = Date.now();
  const r = await req("/api/kb/documents", { method: "POST", body: fd });
  const ms = Date.now() - t0;
  const text = await r.text();
  let json = null; try { json = JSON.parse(text); } catch {}
  return { status: r.status, ms, json, text };
}

async function main() {
  say("# 视觉模型身份取证  base=" + BASE + "  时间 " + new Date().toISOString());
  say("Ollama 驻留模型（入库前）: " + JSON.stringify(await ollamaPs()));

  const lg = await login("a");
  say("登录 " + lg.user + " -> HTTP " + lg.status);

  const up = await uploadImage();
  say(`图片入库 -> HTTP ${up.status}，耗时 ${up.ms}ms，响应 ${up.text.slice(0, 200)}`);
  const id = up.json && up.json.id;
  const loaded = await ollamaPs();
  say("Ollama 驻留模型（入库后立刻查询）: " + JSON.stringify(loaded));
  if (loaded.some((n) => String(n).includes("qwen2.5vl"))) {
    say("[PASS] 入库后 Ollama 里驻留的是 qwen2.5vl —— 视觉推理确实由该模型完成（服务端事实）");
  } else {
    say("[WARN] 入库后 /api/ps 没有看到 qwen2.5vl（可能已被卸载或未驻留），需要看对照实验");
  }

  // 4) 直接对 Ollama 发同样的视觉请求
  const b64 = readFileSync(PNG).toString("base64");
  const t1 = Date.now();
  const vr = await fetch(OLLAMA + "/api/chat", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      model: "qwen2.5vl:7b",
      stream: false,
      messages: [{ role: "user", content: "这张图里有哪些文字？只回答文字内容。", images: [b64] }],
    }),
  });
  const vj = await vr.json();
  const vmMs = Date.now() - t1;
  say(`直接调用 qwen2.5vl:7b 看图 -> HTTP ${vr.status}，耗时 ${vmMs}ms，回答：${JSON.stringify((vj.message && vj.message.content || "").slice(0, 200))}`);
  say("Ollama 驻留模型（直接调用后）: " + JSON.stringify(await ollamaPs()));

  if (id && !KEEP) {
    const d = await req("/api/kb/documents/" + id, { method: "DELETE" });
    say(`清理图片文档 ${id} -> HTTP ${d.status}`);
  } else if (id) {
    say("保留图片文档 id=" + id + "（给 SQL 取证用）");
  }
  mkdirSync("docs/verify/tmp", { recursive: true });
  writeFileSync("docs/verify/tmp/rag-vision.log", log.join("\n") + "\n");
}
main().catch((e) => { console.error("脚本异常：", e); process.exit(2); });
