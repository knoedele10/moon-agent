// 独立 RAG 端到端探测（verifier 自写；不使用 scripts/verify-rag.mjs）
// 用法：node docs/verify/rag-probe.mjs [--base http://127.0.0.1:8300] [--keep]
// 只做「发请求 -> 断言 -> 打印证据」，所有结论都以本脚本的原始输出为准。
import { writeFileSync, readFileSync, mkdirSync, existsSync } from "node:fs";
import { dirname, join } from "node:path";

const args = process.argv.slice(2);
const BASE = (args.includes("--base") ? args[args.indexOf("--base") + 1] : null) || "http://127.0.0.1:8300";
const KEEP = args.includes("--keep");

const results = [];
function rec(id, level, msg, detail) {
  results.push({ id, level, msg, detail });
  const line = `[${level}] ${id} :: ${msg}`;
  console.log(line);
  if (detail !== undefined) console.log("        证据: " + (typeof detail === "string" ? detail : JSON.stringify(detail)));
}
const PASS = (id, m, d) => rec(id, "PASS", m, d);
const FAIL = (id, m, d) => rec(id, "FAIL", m, d);
const WARN = (id, m, d) => rec(id, "WARN", m, d);
const INFO = (id, m, d) => rec(id, "INFO", m, d);

// ---------------- cookie jar ----------------
const jar = new Map();
function jarHeader() {
  return [...jar.entries()].map(([k, v]) => `${k}=${v}`).join("; ");
}
function absorb(res) {
  const set = res.headers.getSetCookie ? res.headers.getSetCookie() : [];
  for (const c of set) {
    const [pair] = c.split(";");
    const i = pair.indexOf("=");
    jar.set(pair.slice(0, i).trim(), pair.slice(i + 1).trim());
  }
}
async function req(path, opts = {}) {
  const headers = Object.assign({}, opts.headers || {});
  if (jar.size) headers["Cookie"] = jarHeader();
  const t0 = Date.now();
  const res = await fetch(BASE + path, { ...opts, headers, redirect: "manual" });
  const ms = Date.now() - t0;
  absorb(res);
  return { res, ms };
}
async function jsonReq(path, method, body, extraHeaders) {
  const r = await req(path, {
    method,
    headers: Object.assign({ "Content-Type": "application/json" }, extraHeaders || {}),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await r.res.text();
  let json = null;
  try { json = JSON.parse(text); } catch { /* 非 JSON */ }
  return { status: r.res.status, json, text, ms: r.ms };
}

function assertEq(id, actual, expected, msg) {
  if (actual === expected) PASS(id, msg, `${JSON.stringify(actual)}`);
  else FAIL(id, msg, `期望 ${JSON.stringify(expected)}，实际 ${JSON.stringify(actual)}`);
}

const TAG = "VRF-" + new Date().toISOString().replace(/[-:TZ.]/g, "").slice(0, 14);
const created = [];

async function main() {
  console.log("# 独立 RAG 探测  base=" + BASE + "  tag=" + TAG);
  console.log("# 时间 " + new Date().toISOString());

  // ---------- 0. 未登录必须 401 ----------
  {
    const r = await req("/api/kb/documents");
    assertEq("A1-unauth-list", r.res.status, 401, "未登录访问 /api/kb/documents 必须 401");
  }
  {
    const r = await jsonReq("/api/kb/search", "POST", { query: "x" });
    assertEq("A2-unauth-search", r.status, 401, "未登录访问 /api/kb/search 必须 401");
  }

  // ---------- 1. 注册/登录拿 session ----------
  const user = `vrf_${Date.now().toString(36)}`;
  const pass = "Verifier123";
  {
    const r = await req("/register", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ username: user, password: pass }).toString(),
    });
    INFO("B1-register", `注册 ${user} -> HTTP ${r.res.status}`);
  }
  {
    const r = await req("/login", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ username: user, password: pass }).toString(),
    });
    const ok = r.res.status === 302 || r.res.status === 200;
    if (ok && jar.size) PASS("B2-login", `登录成功（HTTP ${r.res.status}，session 变量 ${jar.size} 个）`, [...jar.keys()]);
    else FAIL("B2-login", "登录未拿到 session cookie", { status: r.res.status, cookies: [...jar.keys()] });
  }
  {
    const r = await jsonReq("/api/kb/documents", "GET");
    assertEq("B3-authed-list", r.status, 200, "登录后列出文档必须 200");
    if (r.json) INFO("B3-shape", "列表响应顶层字段", Object.keys(r.json));
  }

  // ---------- 2. 语义检索：三份词汇互不重叠的文档 ----------
  const docs = [
    { key: "garden", title: TAG + " 园艺笔记",
      text: "春季播种番茄前要先深翻土壤，施足腐熟有机肥，保持行距六十厘米。\n"
          + "浇水要避开正午高温，早晨浇水可以把叶面病害概率降下来。" },
    { key: "network", title: TAG + " 网络排障记录",
      text: "路由器频繁断线时先看光猫指示灯，再用 ping 检查链路丢包。\n"
          + "如果 DNS 解析超时，把首选域名服务器换成本地运营商地址通常能恢复。" },
    { key: "cooking", title: TAG + " 厨房备忘",
      text: "炖牛肉先用冷水下锅焯去血沫，再加姜片与料酒小火慢炖两小时。\n"
          + "起锅前十分钟放盐，肉质不会发柴。" },
  ];
  const ids = {};
  for (const d of docs) {
    const r = await jsonReq("/api/kb/text", "POST", { title: d.title, text: d.text });
    if (r.status === 201 && r.json && r.json.id) {
      ids[d.key] = r.json.id; created.push(r.json.id);
      PASS("C1-text-" + d.key, `入库 ${d.key}：id=${r.json.id} chunk_count=${r.json.chunk_count}`, `HTTP ${r.status} duplicated=${r.json.duplicated}`);
    } else {
      FAIL("C1-text-" + d.key, "文本入库失败", { status: r.status, body: r.text.slice(0, 300) });
    }
  }
  {
    const r = await jsonReq("/api/kb/text", "POST", { title: docs[0].title, text: docs[0].text });
    if (r.status === 200 && r.json && r.json.duplicated === true && r.json.id === ids.garden)
      PASS("C2-dedupe", "同一文本重复入库被去重（200 + duplicated:true + 同一个 id）", `id=${r.json.id}`);
    else FAIL("C2-dedupe", "sha256 去重不符合契约", { status: r.status, body: r.text.slice(0, 200) });
  }

  // 语义命中：改写提问，刻意不与任何文档共享关键词
  {
    const q = "怎么让种的菜长得更好、浇水要注意什么时间？";
    const r = await jsonReq("/api/kb/search", "POST", { query: q, top_k: 5 });
    if (r.status !== 200 || !r.json || !Array.isArray(r.json.hits)) {
      FAIL("C3-semantic", "检索失败", { status: r.status, body: r.text.slice(0, 200) });
    } else {
      const hits = r.json.hits;
      const top = hits[0];
      const own = hits.filter((h) => h.document_id === ids.garden);
      INFO("C3-raw", "检索命中（query=" + q + "）", hits.map((h) => ({ doc: h.document_id, seq: h.seq, score: Number(h.score.toFixed(4)), text: h.text.slice(0, 24) })));
      if (top && top.document_id === ids.garden) PASS("C3-semantic", "改写提问的 Top1 命中了正确的园艺文档（非关键词匹配）", `score=${top.score.toFixed(4)}`);
      else FAIL("C3-semantic", "改写提问未命中正确文档", { top_doc: top && top.document_id, garden_id: ids.garden, q });
      const gardenTop = own[0];
      const others = hits.filter((h) => h.document_id !== ids.garden).map((h) => h.score);
      if (gardenTop && others.length) {
        const margin = gardenTop.score - Math.max(...others);
        if (margin > 0) PASS("C4-margin", "正确文档得分高于其它文档", `margin=${margin.toFixed(4)}`);
        else WARN("C4-margin", "正确文档与其它文档得分没有区分度", { margin });
      }
      // 排序必须降序
      const sorted = hits.every((h, i) => i === 0 || hits[i - 1].score >= h.score);
      assertEq("C5-sorted", sorted, true, "hits 必须按 score 降序");
    }
  }
  {
    // 无关提问：不应把知识库全量倒出来
    const r = await jsonReq("/api/kb/search", "POST", { query: "量子色动力学的重整化群方程", top_k: 5 });
    if (r.status === 200 && r.json) {
      const max = r.json.hits.length ? Math.max(...r.json.hits.map((h) => h.score)) : null;
      INFO("C6-irrelevant", "无关提问的最高相似度", max);
      if (max !== null && max > 0.95) WARN("C6-irrelevant", "无关提问相似度异常高，需人工复核打分", max);
      else PASS("C6-irrelevant", "无关提问没有出现异常高相似度命中", max);
    }
  }
  {
    const r = await jsonReq("/api/kb/search", "POST", { query: "   ", top_k: 5 });
    assertEq("C7-empty-query", r.status, 400, "空白 query 必须 400");
  }

  // ---------- 3. 列表 / 详情 / 统计 与库内一致性 ----------
  {
    const r = await jsonReq("/api/kb/documents", "GET");
    if (r.status === 200 && r.json && Array.isArray(r.json.data)) {
      const mine = r.json.data.filter((d) => created.includes(d.id));
      assertEq("D1-list-count", mine.length, created.length, "列表应包含本次入库的全部文档");
      const hasContent = r.json.data.some((d) => "content" in d || "embedding" in d);
      assertEq("D2-list-no-blob", hasContent, false, "列表不得返回 content/embedding");
      INFO("D2-fields", "列表条目字段", Object.keys(r.json.data[0] || {}));
    } else FAIL("D1-list-count", "列表接口异常", { status: r.status });
  }
  {
    const id = ids.garden;
    const r = await jsonReq(`/api/kb/documents/${id}`, "GET");
    if (r.status === 200 && r.json && r.json.data) {
      const d = r.json.data;
      const seqs = d.chunks.map((c) => c.seq);
      const sorted = seqs.every((s, i) => i === 0 || seqs[i - 1] < s);
      assertEq("D3-detail-chunks-sorted", sorted, true, "详情 chunks 必须按 seq 升序");
      assertEq("D4-detail-chunkcount", d.chunks.length, d.chunk_count, "chunk_count 必须等于返回的块数");
      const extra = d.chunks.some((c) => "embedding" in c || "document_id" in c || "image_path" in c);
      assertEq("D5-detail-no-internals", extra, false, "详情块不得含 embedding/document_id/image_path");
      const dims = [...new Set(d.chunks.map((c) => c.dim))];
      assertEq("D6-dim-1024", JSON.stringify(dims), JSON.stringify([1024]), "块维度必须是 1024");
      INFO("D7-detail", `文档 ${id}：${d.chunks.length} 块，seq=${seqs.join(",")}`);
    } else FAIL("D3-detail", "详情接口异常", { status: r.status, body: r.text.slice(0, 200) });
  }
  {
    const r = await jsonReq("/api/kb/stats", "GET");
    if (r.status === 200 && r.json) INFO("D8-stats", "统计接口", r.json);
    else FAIL("D8-stats", "统计接口异常", r.status);
  }
  {
    const r = await jsonReq("/api/kb/documents/999999999", "GET");
    assertEq("D9-missing-id", r.status, 404, "不存在的文档 id 必须 404");
    const r2 = await jsonReq("/api/kb/documents/abc", "GET");
    assertEq("D10-bad-id", r2.status, 400, "非法 id 必须 400");
  }

  // ---------- 4. 多模态上传（图片走 qwen2.5vl） ----------
  const pngPath = join("docs", "verify", "tmp", "zebra-verify.png");
  {
    const t0 = Date.now();
    const buf = readFileSync(pngPath);
    const fd = new FormData();
    fd.append("file", new Blob([buf]), pngPath.split(/[\\/]/).pop());
    const r = await req("/api/kb/documents", { method: "POST", body: fd });
    const ms = Date.now() - t0;
    const text = await r.res.text();
    let json = null; try { json = JSON.parse(text); } catch { /* */ }
    if (r.res.status === 201 && json && json.kind === "image") {
      ids.image = json.id; created.push(json.id);
      PASS("E1-image-upload", `图片入库 ${ms}ms：id=${json.id} kind=${json.kind} chunk_count=${json.chunk_count}`, text.slice(0, 200));
      if (ms > 20000) WARN("E2-image-upload-latency", `图片上传耗时 ${ms}ms，超过 20s`, "这正是实现者把验收脚本超时从 20s 改到 300s 的场景；本脚本独立测到该耗时");
      else PASS("E2-image-upload-latency", `图片上传耗时 ${ms}ms（< 20s）`);
    } else {
      FAIL("E1-image-upload", "图片上传失败", { status: r.res.status, body: text.slice(0, 300) });
    }
  }
  if (ids.image) {
    const r = await jsonReq(`/api/kb/documents/${ids.image}`, "GET");
    if (r.status === 200 && r.json && r.json.data) {
      const d = r.json.data;
      const chunk = d.chunks[0] || {};
      INFO("E3-image-caption", "图片块文本（视觉模型输出）", String(chunk.text || "").slice(0, 300));
      assertEq("E4-image-chunk-kind", chunk.kind, "image_caption", "图片块 kind 必须是 image_caption");
      if (String(chunk.text || "").length > 10) PASS("E5-caption-nonempty", "视觉模型产出了非空描述");
      else FAIL("E5-caption-nonempty", "视觉模型描述为空", chunk.text);
    }
    {
      const r = await jsonReq("/api/kb/search", "POST", { query: "ZEBRA-7731", top_k: 5 });
      if (r.status === 200 && r.json.hits.length) {
        const hit = r.json.hits.find((h) => h.document_id === ids.image);
        if (hit) PASS("E6-image-retrieval", "图片块可以被检索命中", `score=${hit.score.toFixed(4)} rank=${r.json.hits.indexOf(hit) + 1}`);
        else WARN("E6-image-retrieval", "图片块未被该 query 命中（视觉描述不可控，如实记 WARN）", r.json.hits.map((h) => ({ doc: h.document_id, s: Number(h.score.toFixed(4)) })));
      }
    }
  }

  // ---------- 5. SSE /api/kb/ask ----------
  {
    const models = await jsonReq("/api/models", "GET");
    let model = "qwen2.5:7b";
    if (models.status === 200 && models.json) {
      const list = models.json.data || models.json.models || models.json;
      const arr = Array.isArray(list) ? list : [];
      const found = arr.find((m) => (m.id || m.name || "").includes("qwen2.5:7b"));
      if (found) model = found.id || found.name;
      INFO("F1-models", `可用模型 ${arr.length} 个，选用 ${model}`);
    } else WARN("F1-models", "/api/models 未返回可用列表", models.status);
    const t0 = Date.now();
    const r = await req("/api/kb/ask", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query: "番茄什么时候浇水比较好？", model, top_k: 3 }),
    });
    const ct = r.res.headers.get("content-type") || "";
    if (r.res.status !== 200 || !ct.includes("text/event-stream")) {
      const txt = await r.res.text();
      FAIL("F2-sse-headers", "ask 未返回 SSE", { status: r.res.status, ct, body: txt.slice(0, 300) });
    } else {
      PASS("F2-sse-headers", "Content-Type 为 text/event-stream", ct);
      const reader = r.res.body.getReader();
      const dec = new TextDecoder();
      let buf = "", evts = [];
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const raw = buf.slice(0, i); buf = buf.slice(i + 2);
          const ev = (raw.match(/^event:\s*(.+)$/m) || [])[1];
          const data = (raw.match(/^data:\s*([\s\S]*)$/m) || [])[1];
          evts.push({ ev: (ev || "").trim(), data: (data || "").trim() });
        }
      }
      const names = evts.map((e) => e.ev);
      const deltas = evts.filter((e) => e.ev === "delta");
      const answer = deltas.map((d) => { try { return JSON.parse(d.data).content || ""; } catch { return ""; } }).join("");
      const ms = Date.now() - t0;
      INFO("F3-sse-events", `事件序列（${evts.length} 个，用时 ${ms}ms）`, names.slice(0, 40).join(" -> ") + (names.length > 40 ? " ..." : ""));
      if (names[0] === "sources") PASS("F4-first-event", "首个事件是 sources");
      else FAIL("F4-first-event", "首个事件不是 sources", names[0]);
      if (names[names.length - 1] === "done" && !names.includes("error")) PASS("F5-last-event", "以 done 结束且无 error 事件");
      else FAIL("F5-last-event", "事件序列未正常收尾", { last: names[names.length - 1], hasError: names.includes("error") });
      if (deltas.length > 0 && answer.trim().length > 0) PASS("F6-answer-nonempty", `delta ${deltas.length} 个、答案 ${answer.length} 字`, answer.slice(0, 200));
      else FAIL("F6-answer-nonempty", "没有非空 delta", { deltas: deltas.length, answer });
      const src = evts.find((e) => e.ev === "sources");
      if (src) {
        try { const j = JSON.parse(src.data); assertEq("F7-sources-shape", Array.isArray(j.hits), true, "sources 事件必须带 hits 数组"); }
        catch (e) { FAIL("F7-sources-shape", "sources 事件不是合法 JSON", src.data.slice(0, 200)); }
      }
    }
  }
  {
    const r = await jsonReq("/api/kb/ask", "POST", { query: "   " });
    assertEq("F8-ask-empty-before-sse", r.status, 400, "空白 query 必须在写 SSE 之前 400");
  }

  // ---------- 6. 错误路径 ----------
  {
    const r = await jsonReq("/api/kb/text", "POST", { text: "   " });
    assertEq("G1-empty-text", r.status, 400, "空白正文必须 400");
  }
  {
    const fd = new FormData();
    fd.append("file", new Blob([Buffer.from("hello")]), "bad.pdf");
    const r = await req("/api/kb/documents", { method: "POST", body: fd });
    const body = await r.res.text();
    assertEq("G2-bad-ext", r.res.status, 400, "不支持的扩展名必须 400（.pdf）");
    if (body.includes("pdf")) PASS("G2b-bad-ext-msg", "错误信息点名了扩展名", body.slice(0, 160));
    else WARN("G2b-bad-ext-msg", "错误信息未点名扩展名", body.slice(0, 160));
  }
  {
    const r = await req("/api/kb/documents", { method: "POST", body: new FormData() });
    assertEq("G3-missing-file", r.res.status, 400, "缺 file 字段必须 400");
  }
  {
    const big = Buffer.alloc(21 << 20, 0x61);
    const fd = new FormData();
    fd.append("file", new Blob([big]), "big.txt");
    const r = await req("/api/kb/documents", { method: "POST", body: fd });
    const body = await r.res.text();
    if (r.res.status === 400) PASS("G4-oversize", "21MiB 超限被拒（400）", body.slice(0, 160));
    else FAIL("G4-oversize", "超过 20MiB 未被拒", { status: r.res.status, body: body.slice(0, 160) });
  }
  {
    const r = await req("/api/kb/text", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{不是 JSON",
    });
    assertEq("G5-bad-json", r.res.status, 400, "非法 JSON 必须 400");
  }
  {
    const r = await jsonReq("/api/kb/search", "POST", { query: "x", top_k: 999 });
    if (r.status === 200) PASS("G6-topk-clamp", "top_k=999 被收敛而不是报错", `hits=${(r.json.hits || []).length}`);
    else FAIL("G6-topk-clamp", "top_k 越界处理异常", r.status);
  }

  // ---------- 7. 删除与级联 ----------
  {
    const id = ids.cooking;
    const r = await jsonReq(`/api/kb/documents/${id}`, "DELETE");
    if (r.status === 200 && r.json && r.json.deleted === true) PASS("H1-delete", `删除文档 ${id} 成功`, r.text.slice(0, 160));
    else FAIL("H1-delete", "删除失败", { status: r.status, body: r.text.slice(0, 200) });
    const d = await jsonReq(`/api/kb/documents/${id}`, "GET");
    assertEq("H2-delete-detail-404", d.status, 404, "删除后详情必须 404");
    const r2 = await jsonReq(`/api/kb/documents/${id}`, "DELETE");
    assertEq("H3-delete-again-404", r2.status, 404, "重复 DELETE 必须 404");
    const list = await jsonReq("/api/kb/documents", "GET");
    const still = (list.json.data || []).some((x) => x.id === id);
    assertEq("H4-delete-list", still, false, "删除后列表不得再出现该文档");
    const s = await jsonReq("/api/kb/search", "POST", { query: "炖牛肉 冷水下锅 焯血沫", top_k: 3 });
    const orphan = (s.json.hits || []).some((h) => h.document_id === id);
    assertEq("H5-delete-search", orphan, false, "删除后检索不得再命中该文档的块");
    created.splice(created.indexOf(id), 1);
  }

  // ---------- 8. 收尾 ----------
  if (KEEP) {
    INFO("Z1-keep", "按要求保留测试数据", created);
  } else {
    for (const id of [...created]) {
      const r = await jsonReq(`/api/kb/documents/${id}`, "DELETE");
      INFO("Z2-cleanup", `清理文档 ${id} -> HTTP ${r.status}`);
    }
  }
  INFO("Z3-summary", `PASS=${results.filter((r) => r.level === "PASS").length} FAIL=${results.filter((r) => r.level === "FAIL").length} WARN=${results.filter((r) => r.level === "WARN").length}`);

  const out = { base: BASE, tag: TAG, user, created, results };
  mkdirSync(join("docs", "verify", "tmp"), { recursive: true });
  writeFileSync(join("docs", "verify", "tmp", `probe-${TAG}.json`), JSON.stringify(out, null, 2));
  console.log("\n# 机器可读结果：docs/verify/tmp/probe-" + TAG + ".json");
  const fails = results.filter((r) => r.level === "FAIL");
  if (fails.length) {
    console.log("\n# 失败清单：");
    for (const f of fails) console.log("  - " + f.id + " :: " + f.msg + " " + (typeof f.detail === "string" ? f.detail : JSON.stringify(f.detail)));
  }
  process.exit(fails.length ? 1 : 0);
}

main().catch((e) => { console.error("探测脚本异常：", e); process.exit(2); });
