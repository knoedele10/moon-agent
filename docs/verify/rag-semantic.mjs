// 独立语义检索判别力测试（verifier 自写）
// 关键设计：目标文档**故意不包含**查询里的关键词（玫瑰/修剪/季节），
// 库里同时存在 5 篇同语言干扰文档；若检索退化成关键词匹配，目标文档不会排第一。
import { writeFileSync, mkdirSync } from "node:fs";

const BASE = (process.argv.includes("--base") ? process.argv[process.argv.indexOf("--base") + 1] : null) || "http://127.0.0.1:8300";
const jar = new Map();
const hdr = () => (jar.size ? { Cookie: [...jar.entries()].map(([k, v]) => k + "=" + v).join("; ") } : {});
function absorb(res) {
  for (const c of (res.headers.getSetCookie ? res.headers.getSetCookie() : [])) {
    const [p] = c.split(";"); const i = p.indexOf("="); jar.set(p.slice(0, i).trim(), p.slice(i + 1).trim());
  }
}
async function req(path, opts = {}) {
  const res = await fetch(BASE + path, { ...opts, headers: { ...(opts.headers || {}), ...hdr() }, redirect: "manual" });
  absorb(res); return res;
}
async function j(path, method, body) {
  const r = await req(path, { method, headers: { "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
  const t = await r.text(); let v = null; try { v = JSON.parse(t); } catch {}
  return { status: r.status, json: v, text: t };
}
const round4 = (x) => Number(x.toFixed(4));
const out = [];
const say = (s) => { console.log(s); out.push(s); };

// 目标文档：不含「玫瑰」「修剪」「季节」这几个查询词
const TARGET = "藤本月季在冬末还处于休眠期的时候动剪刀最合适。先去掉枯死枝和互相摩擦的交叉枝，再把剩下的枝条截短三分之一。"
  + "下刀的位置要挑朝外的芽上方，这样新枝会朝外生长，植株中间也能透风，来年花量更大。";
// 5 篇干扰文档（主题各不相同，与查询无字面重叠）
const DECOYS = [
  { t: "DNS 排障", x: "浏览器打不开网页时先看本机的域名解析服务是否可达，再检查默认网关。" },
  { t: "炖牛肉", x: "牛腩冷水下锅焯去血沫，加姜片料酒小火炖两小时，起锅前十分钟放盐。" },
  { t: "轮胎保养", x: "夏季胎压不要打得太高，跑高速前检查花纹深度与是否有鼓包。" },
  { t: "个税申报", x: "专项附加扣除包括子女教育、继续教育、住房贷款利息与赡养老人。" },
  { t: "疫苗时间表", x: "新生儿出生后按免疫规划接种乙肝疫苗与卡介苗，后续按月龄补种。" },
];
const Q1 = "玫瑰应该在什么季节修剪？";            // 语义命中 TARGET，但字面词都不在 TARGET 里
const Q2 = "怎么让电脑重新连上网？";              // 语义命中 DNS 干扰篇
const QN = "量子色动力学里的渐近自由是怎么来的？"; // 无关提问：分数应当明显更低

async function main() {
  say("# 语义检索判别力测试  base=" + BASE);
  const u = "vrf_sem_" + Date.now().toString(36);
  await req("/register", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ username: u, password: "Verifier123" }).toString() });
  const lg = await req("/login", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ username: u, password: "Verifier123" }).toString() });
  say("登录 HTTP " + lg.status + " ; cookie: " + [...jar.keys()].join(","));

  const ids = {};
  const rt = await j("/api/kb/text", "POST", { title: "月季冬剪 " + u, text: TARGET });
  ids.target = rt.json.id; say("目标文档 id=" + ids.target + " chunk_count=" + rt.json.chunk_count);
  for (const d of DECOYS) {
    const r = await j("/api/kb/text", "POST", { title: d.t + " " + u, text: d.x });
    ids[d.t] = r.json.id;
  }
  say("干扰文档 id = " + DECOYS.map((d) => d.t + ":" + ids[d.t]).join(", "));

  async function rank(q, expectId, label) {
    const r = await j("/api/kb/search", "POST", { query: q, top_k: 20 });
    if (r.status !== 200) { say(`[FAIL] ${label} 检索失败 HTTP ${r.status}`); return; }
    const hits = r.json.hits;
    const lines = hits.map((h, i) => `  #${i + 1} doc${h.document_id} seq${h.seq} score=${round4(h.score)} text="${h.text.slice(0, 28)}"`);
    say(`查询「${q}」 命中 ${hits.length} 条：`); for (const l of lines) say(l);
    const top = hits[0];
    const expectScore = hits.find((h) => h.document_id === expectId)?.score;
    const otherMax = Math.max(...hits.filter((h) => h.document_id !== expectId).map((h) => h.score), -1);
    if (top && top.document_id === expectId) {
      say(`[PASS] ${label}：预期文档排第 1（score=${round4(top.score)}），相对其它文档的最大优势 = ${round4(expectScore - otherMax)}`);
    } else {
      say(`[FAIL] ${label}：预期文档未排第 1。预期 id=${expectId}（score=${expectScore === undefined ? "未进前 20" : round4(expectScore)}），实际 Top1 = doc${top && top.document_id} score=${top && round4(top.score)}`);
    }
    return { hits, expectScore, otherMax };
  }

  await rank(Q1, ids.target, "语义命中-月季冬剪");
  await rank(Q2, ids["DNS 排障"], "语义命中-DNS 排障");
  const neg = await j("/api/kb/search", "POST", { query: QN, top_k: 20 });
  const negMax = Math.max(...neg.json.hits.map((h) => h.score), -1);
  const posMax = (await j("/api/kb/search", "POST", { query: Q1, top_k: 20 })).json.hits[0].score;
  say(`无关提问最高分 = ${round4(negMax)} ；相关问题最高分 = ${round4(posMax)} ；差值 = ${round4(posMax - negMax)}`);
  if (posMax - negMax > 0.05) say("[PASS] 相关与无关提问的分数有可观测差异（不是「一律高分」）");
  else say("[WARN] 相关与无关提问分数区分度不足");

  // 清理
  for (const [k, id] of Object.entries(ids)) {
    const r = await j("/api/kb/documents/" + id, "DELETE");
    say(`清理 ${k} doc${id} -> HTTP ${r.status}`);
  }
  mkdirSync("docs/verify/tmp", { recursive: true });
  writeFileSync("docs/verify/tmp/rag-semantic.log", out.join("\n") + "\n");
}
main().catch((e) => { console.error("脚本异常：", e); process.exit(2); });
