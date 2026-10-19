// 上游桩（OpenAI 兼容）：抓取代理发来的请求体，并可返回空 content（契约 §3.1）。
// 用法：node docs/verify/it-upstream-capture.mjs --port 8399 --mode normal|empty --out docs/verify/tmp/upstream-capture.json
import { writeFileSync, appendFileSync, mkdirSync } from "node:fs";
import { createServer } from "node:http";
import { dirname } from "node:path";

const args = process.argv.slice(2);
const arg = function (n, d) { return args.indexOf(n) >= 0 ? args[args.indexOf(n) + 1] : d; };
const PORT = Number(arg("--port", "8399"));
const MODE = arg("--mode", "normal");
const OUT = arg("--out", "docs/verify/tmp/upstream-capture.json");
let seq = 0;

function readBody(req) {
  return new Promise(function (resolve) {
    let b = "";
    req.on("data", function (c) { b += c; });
    req.on("end", function () { resolve(b); });
  });
}

const server = createServer(async function (req, res) {
  if (req.method !== "POST" || req.url.indexOf("/chat/completions") < 0) {
    res.writeHead(404, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: { message: "stub only serves POST /v1/chat/completions" } }));
    return;
  }
  const raw = await readBody(req);
  let body = null;
  try { body = JSON.parse(raw); } catch (e) { body = { raw: raw.slice(0, 400) }; }
  seq += 1;
  let content = "这是一张 1x1 的测试图片（上游桩的正常回复）。";
  if (MODE === "empty") content = "";
  if (MODE === "actions") {
    // 返回**合法的动作 JSON**，让子进程循环继续跑很多步（用来验证 frame_image 的内存上限）。
    // wait 在 dry-run 下不产生任何真实输入；步数变化避免被 stall 检测提前收尾。
    // 必须满足 cua/1 schema：根级要有 protocol，action.ms 下限 100（否则子进程报 E_SCHEMA 只走 1 步）。
    content = JSON.stringify({
      protocol: "cua/1",
      thought: "桩回复第 " + seq + " 步：继续观察界面",
      action: { type: "wait", ms: 100 + (seq % 40) * 20, reason: "桩回复" },
      risk: "low",
      done: false
    });
  }
  const reply = {
    id: "chatcmpl-stub-" + seq, object: "chat.completion", created: Math.floor(Date.now() / 1000),
    model: (body && body.model) || "stub-model",
    choices: [{ index: 0, message: { role: "assistant", content: content }, finish_reason: "stop" }],
    usage: { prompt_tokens: 100, completion_tokens: 12, total_tokens: 112 }
  };
  const cap = {
    seq: seq, mode: MODE, at: new Date().toISOString(), url: req.url,
    auth_seen: typeof req.headers.authorization === "string" && req.headers.authorization.length > 0,
    model: body && body.model, stream: body && body.stream,
    max_tokens: body && body.max_tokens, temperature: body && body.temperature,
    messages: (body && body.messages) || [],
    message_content_types: ((body && body.messages) || []).map(function (m) {
      return Array.isArray(m.content) ? m.content.map(function (p) { return p.type; }) : typeof m.content;
    }),
    reply_content_len: content.length
  };
  try {
    mkdirSync(dirname(OUT), { recursive: true });
    writeFileSync(OUT, JSON.stringify(cap, null, 2), "utf8");
    // 每一次请求都追加一行，便于逐个向量对照（max_tokens 下限这类要多组取值一起看）。
    appendFileSync(OUT + ".all.jsonl", JSON.stringify(cap) + "\n", "utf8");
  } catch (e) {}
  console.log("[stub] #" + seq + " model=" + (body && body.model) + " content_parts=" + JSON.stringify(cap.message_content_types) + " -> content_len=" + content.length);
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify(reply));
});

server.listen(PORT, "127.0.0.1", function () {
  console.log("[stub] listening http://127.0.0.1:" + PORT + "/v1  mode=" + MODE + "  out=" + OUT);
});
