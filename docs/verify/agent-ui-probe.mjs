// verifier 自写的 §9.8 前端验收探针（**不是** cua-ui 的探针，也不复用它的断言）。
// 用法: node docs/verify/agent-ui-probe.mjs [--base http://127.0.0.1:10722] [--fs-root <dir>] [--with-model]
//        负向对照（各自单独跑，断言必须转红）:
//          --strip-tool-hook   把 chat.js 里 onTool 回调那行掐死  -> 工具卡片断言应转红
//          --strip-keys-open   从 HTML 里删掉 #keys-open        -> 密钥断言应转红
//          --strip-memory      把 #memory-panel 的 id 改掉      -> 记忆断言应转红
// 退出码 0=全 PASS / 1=有 FAIL / 2=前置缺失（服务不可达、浏览器库没装）
import { readFileSync, existsSync } from "node:fs";
import { resolve } from "node:path";
import { createRequire } from "node:module";

const A = process.argv.slice(2);
function arg(name, dflt) { const i = A.indexOf(name); return i >= 0 && A[i + 1] ? A[i + 1] : dflt; }
const BASE = arg("--base", "http://127.0.0.1:10722");
const ROOT = arg("--root", resolve(import.meta.dirname, "..", ".."));
const FS_ROOT = arg("--fs-root", (process.env.TEMP || "/tmp") + "/agent-verify/ui-root");
const WITH_MODEL = A.indexOf("--with-model") >= 0;
const STRIP_TOOL = A.indexOf("--strip-tool-hook") >= 0;
const STRIP_KEYS = A.indexOf("--strip-keys-open") >= 0;
const STRIP_MEM = A.indexOf("--strip-memory") >= 0;

const results = [];
function rec(level, id, msg, detail) {
  results.push({ level: level, id: id });
  console.log("[" + level + "] " + id + " :: " + msg);
  if (detail !== undefined) console.log("        证据: " + (typeof detail === "string" ? detail : JSON.stringify(detail)));
}
const PASS = (id, m, d) => rec("PASS", id, m, d);
const FAIL = (id, m, d) => rec("FAIL", id, m, d);
const SKIP = (id, m, d) => rec("SKIP", id, m, d);
const INFO = (id, m, d) => rec("INFO", id, m, d);

const PW = ["C:/Users/woshiss/AppData/Roaming/npm/node_modules/openclaw/node_modules/playwright-core", "playwright-core"];
const CHROME = ["C:/Users/woshiss/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe"];
const require2 = createRequire(import.meta.url);

function readText(p) { try { return readFileSync(p, "utf8"); } catch (e) { return null; } }

// ---------- S 层：读源码的结构性断言（不需要服务器） ----------
function staticTier() {
  const html = readText(ROOT + "/templates/chat/index.html");
  if (!html) { FAIL("S-html", "读不到 templates/chat/index.html"); return; }
  const views = [];
  const tabRe = /data-view="([a-z]+)"/g;
  let m;
  while ((m = tabRe.exec(html)) !== null) if (views.indexOf(m[1]) < 0) views.push(m[1]);
  const tabsWanted = ["chat", "models", "cua"];
  const tabsOk = views.length === tabsWanted.length && tabsWanted.every((v) => views.indexOf(v) >= 0);
  if (tabsOk) PASS("S-tabs", "页签恰好是 chat/models/cua", views);
  else FAIL("S-tabs", "页签集合不对", views);
  const hasViewKeys = html.indexOf("id=\"view-keys\"") >= 0;
  const hasViewKb = html.indexOf("id=\"view-kb\"") >= 0;
  if (!hasViewKeys && !hasViewKb) PASS("S-no-legacy-views", "没有 #view-keys / #view-kb 区块");
  else FAIL("S-no-legacy-views", "旧页签区块还在", { hasViewKeys: hasViewKeys, hasViewKb: hasViewKb });
  const topStart = html.indexOf("class=\"topbar\"");
  const topEnd = html.indexOf("</header>", topStart);
  const keysIdx = html.indexOf("id=\"keys-open\"");
  if (topStart >= 0 && keysIdx > topStart && keysIdx < topEnd) PASS("S-keys-in-topbar", "#keys-open 在 .topbar 内", { topStart: topStart, topEnd: topEnd, keysIdx: keysIdx });
  else FAIL("S-keys-in-topbar", "#keys-open 不在 .topbar 内", { topStart: topStart, topEnd: topEnd, keysIdx: keysIdx });
  const memIdx = html.indexOf("id=\"memory-panel\"");
  const modelsIdx = html.indexOf("id=\"view-models\"");
  const cuaIdx = html.indexOf("id=\"view-cua\"");
  if (memIdx > 0 && memIdx < modelsIdx && memIdx < cuaIdx) PASS("S-memory-in-chat", "记忆面板在对话区（排在两个 view 之前，不在它们内部）", { memIdx: memIdx, modelsIdx: modelsIdx, cuaIdx: cuaIdx });
  else FAIL("S-memory-in-chat", "记忆面板位置不对", { memIdx: memIdx, modelsIdx: modelsIdx, cuaIdx: cuaIdx });
}

// 进 CUA 视图会弹原生 <dialog>（showModal → 其余界面 inert），后续任何点击都会超时。
// 这是产品行为，不是缺陷；探针必须自己先把它关掉再继续。
async function dismissCuaDialog(page) {
  const open = await page.evaluate(() => { const d = document.getElementById("cua-beta-dialog"); return !!(d && d.open); });
  if (!open) return false;
  await page.keyboard.press("Escape");
  await page.waitForTimeout(600);
  const still = await page.evaluate(() => { const d = document.getElementById("cua-beta-dialog"); return !!(d && d.open); });
  return !still;
}

// ---------- B 层：真浏览器 ----------
async function main() {
  staticTier();
  let mod = null, from = null;
  for (const c of PW) { try { mod = require2(c); from = c; break; } catch (e) {} }
  if (!mod) { SKIP("B-all", "找不到 playwright-core（只跑了静态层）", PW); return; }
  const exe = CHROME.find((p) => existsSync(p));
  if (!exe) { SKIP("B-all", "找不到 chromium 可执行文件", CHROME); return; }
  INFO("B-env", "playwright-core=" + from + " ; chromium=" + exe);
  const browser = await mod.chromium.launch({ headless: true, executablePath: exe, args: ["--no-sandbox"] });
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const u = "verifier_ui_" + Date.now().toString(36);
  const pwd = "Verify-" + u + "-2026";
  await ctx.request.post(BASE + "/register", { form: { username: u, password: pwd } });
  await ctx.request.post(BASE + "/login", { form: { username: u, password: pwd } });
  INFO("B-login", "自建用户已登录", u);
  const page = await ctx.newPage();
  const pageErrors = [], consoleErrors = [], badResponses = [], failedReqs = [];
  page.on("pageerror", (e) => pageErrors.push(String(e)));
  page.on("dialog", (d) => { INFO("B-native-dialog", "浏览器原生对话框（自动接受）", d.type() + ": " + d.message().slice(0, 60)); d.accept(); });
  page.on("console", (msg) => { if (msg.type() === "error") { const loc = msg.location() || {}; consoleErrors.push({ text: msg.text(), url: loc.url || "" }); } });
  page.on("response", (r) => { if (r.status() >= 400) badResponses.push(r.status() + " " + r.url()); });
  page.on("requestfailed", (r) => failedReqs.push(r.url() + " " + (r.failure() ? r.failure().errorText : "")));

  if (STRIP_TOOL) {
    await page.route("**/static/js/chat.js*", async (route) => {
      const resp = await route.fetch();
      const body = await resp.text();
      const patched = body.split("onTool(event, tool)").join("void 0");
      INFO("NEG-tool", "负向对照：chat.js 里 onTool 回调已掐死", { changed: patched !== body });
      await route.fulfill({ status: resp.status(), headers: { "content-type": "application/javascript; charset=utf-8" }, body: patched });
    });
  }
  if (STRIP_KEYS || STRIP_MEM) {
    await page.route("**/*", async (route) => {
      const req = route.request();
      if (req.resourceType() !== "document") return route.continue();
      const resp = await route.fetch();
      let body = await resp.text();
      let note = [];
      if (STRIP_KEYS && body.indexOf("id=\"keys-open\"") >= 0) { body = body.split("<button type=\"button\" class=\"btn btn-ghost\" id=\"keys-open\"").join("<button type=\"button\" hidden data-stripped=\"keys-open\""); note.push("keys-open 已拿掉"); }
      if (STRIP_MEM && body.indexOf("id=\"memory-panel\"") >= 0) { body = body.split("id=\"memory-panel\"").join("id=\"memory-panel-stripped\""); note.push("memory-panel 已改名"); }
      INFO("NEG-html", "负向对照：改写了服务端 HTML", note);
      await route.fulfill({ status: resp.status(), headers: { "content-type": "text/html; charset=utf-8" }, body: body });
    });
  }

  await page.goto(BASE + "/", { waitUntil: "load", timeout: 25000 });
  await page.waitForSelector("#input", { timeout: 20000 });

  // 1) 三个页签 + 旧页签区块为 0
  const tabs = await page.$$eval("a.view-tab", (els) => els.map((e) => e.getAttribute("data-view")));
  if (tabs.length === 3 && tabs.join(",") === "chat,models,cua") PASS("B-tabs", "页签 = " + tabs.join(","));
  else FAIL("B-tabs", "页签不对", tabs);
  for (const v of ["chat", "models", "cua"]) {
    await page.click("a.view-tab[data-view=\"" + v + "\"]");
    await page.waitForTimeout(600);
    if (v === "cua") await dismissCuaDialog(page);
    const dv = await page.getAttribute(".app", "data-view");
    if (dv === v) PASS("B-tab-" + v, "点页签后 .app[data-view]=" + dv);
    else FAIL("B-tab-" + v, ".app[data-view] 没跟着切", { want: v, got: dv });
  }
  const legacy = await page.evaluate(() => document.querySelectorAll("#view-keys, #view-kb").length);
  if (legacy === 0) PASS("B-no-legacy-views", "DOM 里 #view-keys/#view-kb 数量 = 0");
  else FAIL("B-no-legacy-views", "旧区块还在 DOM 里", legacy);

  // 2) 密钥在右上角 + 弹层开关 + 焦点回按钮 + #/keys 路由
  await page.click("a.view-tab[data-view=\"chat\"]");
  await page.waitForTimeout(400);
  const inTopbar = await page.evaluate(() => { const b = document.getElementById("keys-open"); return !!(b && b.closest(".topbar")); });
  if (inTopbar) PASS("B-keys-topbar", "#keys-open 的最近祖先里有 .topbar");
  else FAIL("B-keys-topbar", "#keys-open 不在 .topbar 内");
  await page.click("#keys-open");
  await page.waitForTimeout(400);
  const opened = await page.evaluate(() => { const d = document.getElementById("keys-dialog"); return { open: !!(d && d.open), expanded: document.getElementById("keys-open").getAttribute("aria-expanded") }; });
  if (opened.open) PASS("B-keys-open", "点 #keys-open 后 #keys-dialog 打开", opened);
  else FAIL("B-keys-open", "弹层没打开", opened);
  await page.click("#keys-close");
  await page.waitForTimeout(500);
  const closed = await page.evaluate(() => ({ open: document.getElementById("keys-dialog").open, active: document.activeElement ? document.activeElement.id : "", expanded: document.getElementById("keys-open").getAttribute("aria-expanded") }));
  if (!closed.open && closed.active === "keys-open") PASS("B-keys-close", "关闭后焦点回到 #keys-open", closed);
  else FAIL("B-keys-close", "关闭后状态/焦点不对", closed);
  await page.goto(BASE + "/#/keys", { waitUntil: "load", timeout: 20000 });
  await page.waitForTimeout(900);
  const routeOpen = await page.evaluate(() => { const d = document.getElementById("keys-dialog"); return !!(d && d.open); });
  if (routeOpen) PASS("B-keys-route", "#/keys 路由仍可直达并打开弹层");
  else FAIL("B-keys-route", "#/keys 路由打不开弹层（路由能力回退）");
  await page.keyboard.press("Escape");
  await page.waitForTimeout(300);

  // 3) 记忆面板：默认折叠 / 只在对话视图可见 / CRUD
  await page.goto(BASE + "/#/chat", { waitUntil: "load", timeout: 20000 });
  await page.waitForTimeout(600);
  const memInit = await page.evaluate(() => { const p = document.getElementById("memory-panel"); return p ? { exists: true, open: p.open } : { exists: false }; });
  if (memInit.exists && memInit.open === false) PASS("B-memory-collapsed", "#memory-panel 存在且默认折叠");
  else FAIL("B-memory-collapsed", "记忆面板缺失或默认展开", memInit);
  const visChat = await page.isVisible("#memory-panel");
  await page.click("a.view-tab[data-view=\"models\"]");
  await page.waitForTimeout(500);
  const visModels = await page.isVisible("#memory-panel");
  await page.click("a.view-tab[data-view=\"cua\"]");
  await page.waitForTimeout(900);
  await dismissCuaDialog(page);
  await page.waitForTimeout(300);
  const visCua = await page.isVisible("#memory-panel");
  if (visChat && !visModels && !visCua) PASS("B-memory-scope", "记忆面板只在对话视图可见", { chat: visChat, models: visModels, cua: visCua });
  else FAIL("B-memory-scope", "记忆面板可见性与视图不匹配", { chat: visChat, models: visModels, cua: visCua });
  await page.click("a.view-tab[data-view=\"chat\"]");
  await page.waitForTimeout(500);
  await page.evaluate(() => { const p = document.getElementById("memory-panel"); if (p) p.open = true; });
  await page.waitForTimeout(900);
  const marker = "VERIFY-UI-" + Date.now().toString(36).toUpperCase();
  await page.fill("#kb-text-title", "验收记忆 " + marker);
  await page.fill("#kb-text-body", "暗号 " + marker + "：这是 verifier 自写探针写入的记忆");
  await page.click("#kb-text-btn");
  let created = false;
  for (let i = 0; i < 60; i++) {
    await page.waitForTimeout(500);
    const txt = await page.evaluate(() => (document.getElementById("kb-doc-list") || {}).textContent || "");
    if (txt.indexOf(marker) >= 0) { created = true; break; }
  }
  if (created) PASS("B-memory-create", "记忆面板内新增文档成功（列表出现 " + marker + "）");
  else FAIL("B-memory-create", "新增后列表没出现该文档");
  if (created) {
    const delClicked = await page.evaluate((mk) => {
      const list = document.getElementById("kb-doc-list");
      if (!list) return false;
      const items = Array.prototype.slice.call(list.children);
      for (const it of items) {
        if (it.textContent && it.textContent.indexOf(mk) >= 0) {
          const btns = it.querySelectorAll("button");
          for (const b of btns) { if (b.textContent && b.textContent.indexOf("删") >= 0) { b.click(); return true; } }
        }
      }
      return false;
    }, marker);
    if (!delClicked) FAIL("B-memory-delete", "在列表项里找不到删除按钮");
    else {
      let gone = false;
      for (let i = 0; i < 20; i++) {
        await page.waitForTimeout(500);
        const txt = await page.evaluate(() => (document.getElementById("kb-doc-list") || {}).textContent || "");
        if (txt.indexOf(marker) < 0) { gone = true; break; }
      }
      if (gone) PASS("B-memory-delete", "删除后列表里不再有该文档");
      else FAIL("B-memory-delete", "删除后文档仍在列表里");
    }
  }

  // 4) CUA 回归：页签可进 / 角标 / 警告弹窗 / 焦点回页签 / 零卡片泄漏
  await page.click("a.view-tab[data-view=\"cua\"]");
  await page.waitForTimeout(1200);
  const cuaVis = await page.isVisible("#view-cua");
  const badge = await page.evaluate(() => { const b = document.querySelector("a.view-tab[data-view=\"cua\"] .view-tab-beta"); return b ? b.textContent.trim() : ""; });
  const dlg = await page.evaluate(() => { const d = document.getElementById("cua-beta-dialog"); return d ? { open: d.open, role: d.getAttribute("role"), modal: d.getAttribute("aria-modal"), labelled: d.getAttribute("aria-labelledby") } : null; });
  if (cuaVis) PASS("B-cua-view", "CUA 页签能进且视图可见"); else FAIL("B-cua-view", "CUA 视图不可见");
  if (badge === "测试版") PASS("B-cua-badge", "测试版角标在", badge); else FAIL("B-cua-badge", "测试版角标缺失/文案不对", badge);
  if (dlg && dlg.open && dlg.role === "dialog" && dlg.modal === "true" && dlg.labelled) PASS("B-cua-dialog", "警告弹窗弹出且 role/aria-modal/aria-labelledby 齐", dlg);
  else FAIL("B-cua-dialog", "警告弹窗没弹或属性缺失", dlg);
  const leak = await page.evaluate(() => document.querySelectorAll("#view-cua .tool-card, #view-cua .tool-cards").length);
  if (leak === 0) PASS("B-cua-no-leak", "CUA 视图内工具卡片数量 = 0"); else FAIL("B-cua-no-leak", "CUA 视图里有卡片泄漏", leak);
  await page.keyboard.press("Escape");
  await page.waitForTimeout(500);
  const focusBack = await page.evaluate(() => { const d = document.getElementById("cua-beta-dialog"); const a = document.activeElement; return { open: d ? d.open : null, activeView: a && a.getAttribute ? a.getAttribute("data-view") : null, activeTag: a ? a.tagName : null }; });
  if (focusBack.open === false && focusBack.activeView === "cua") PASS("B-cua-focus", "关掉弹窗后焦点回到 CUA 页签", focusBack);
  else FAIL("B-cua-focus", "关掉后焦点没回页签", focusBack);

  // 5) 390px 无横向溢出
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForTimeout(1200);
  const toggleVis = await page.isVisible("#sidebar-toggle");
  if (toggleVis) PASS("B-390-toggle", "390px 下汉堡按钮 #sidebar-toggle 可见（侧栏是既有 off-canvas 设计）");
  else FAIL("B-390-toggle", "390px 下没有可见的侧栏开关，页签将不可达");
  const over = [];
  const navReach = [];
  for (const v of ["chat", "models", "cua"]) {
    const already = await page.evaluate(() => { const s = document.querySelector(".sidebar"); return !!(s && s.classList.contains("open")); });
    if (!already) { await page.click("#sidebar-toggle"); await page.waitForTimeout(500); }
    const opened = await page.evaluate(() => { const s = document.querySelector(".sidebar"); return !!(s && s.classList.contains("open")); });
    let clicked = true;
    try { await page.click("a.view-tab[data-view=\"" + v + "\"]", { timeout: 6000 }); } catch (e) { clicked = false; }
    await page.waitForTimeout(800);
    if (v === "cua") await dismissCuaDialog(page);
    navReach.push({ view: v, sidebarOpened: opened, tabClicked: clicked });
    const w = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth }));
    if (w.sw > w.cw + 1) over.push(v + ":" + w.sw + ">" + w.cw);
  }
  const allReach = navReach.every((x) => x.sidebarOpened && x.tabClicked);
  if (allReach) PASS("B-390-nav", "390px 下用汉堡打开侧栏后三个页签都能切", navReach);
  else FAIL("B-390-nav", "390px 下页签切不过去", navReach);
  if (over.length === 0) PASS("B-390", "390px 宽下三个视图都没有横向溢出（scrollWidth == clientWidth）");
  else FAIL("B-390", "390px 下有横向溢出", over);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.waitForTimeout(600);
  // 6) 工具卡片（真 deepseek-chat + 真 fs 工具；慢/花钱，用 --with-model 开）
  if (WITH_MODEL) {
    await page.click("a.view-tab[data-view=\"chat\"]");
    await page.waitForTimeout(500);
    // 轮询看不到「毫秒级」的运行中态，所以在发消息前装 MutationObserver，记录每次 class 变化。
    await page.evaluate(() => {
      window.__cardStates = [];
      const root = document.getElementById("messages") || document.body;
      const obs = new MutationObserver((muts) => {
        for (const m of muts) {
          if (m.type === "childList") {
            for (const n of m.addedNodes) {
              if (n.nodeType === 1 && n.classList && n.classList.contains("tool-card")) window.__cardStates.push({ how: "added", cls: String(n.className) });
            }
          } else if (m.type === "attributes" && m.target && m.target.classList && m.target.classList.contains("tool-card")) {
            window.__cardStates.push({ how: "class-change", cls: String(m.target.className) });
          }
        }
      });
      obs.observe(root, { childList: true, subtree: true, attributes: true, attributeFilter: ["class"] });
    });
    const canary = "UI-VERIFY-" + Date.now().toString(36).toUpperCase();
    await page.fill("#input", "请先用 read 工具读取 notes.md，再用 write 工具把它的完整内容写到 copy.md。必须真的调用工具，不要只描述。这次运行的标记是 " + canary);
    await page.click("#send-btn");
    let sawRunning = false;
    for (let i = 0; i < 900; i++) {
      const st = await page.evaluate(() => document.querySelectorAll(".tool-card.is-running").length);
      if (st > 0) sawRunning = true;
      const done = await page.evaluate(() => document.querySelectorAll(".tool-card.is-ok, .tool-card.is-error").length);
      if (done > 0 && st === 0) break;
      await page.waitForTimeout(150);
    }
    const states = await page.evaluate(() => window.__cardStates || []);
    const observedRunning = states.some((s) => String(s.cls).indexOf("is-running") >= 0);
    if (observedRunning) sawRunning = true;
    INFO("B-toolcard-transitions", "卡片 class 变化轨迹（前 8 条）", states.slice(0, 8));
    // 等第二个工具调用也落卡（read+write 两张）
    for (let i = 0; i < 60; i++) {
      const n = await page.evaluate(() => document.querySelectorAll(".tool-card").length);
      if (n >= 2) break;
      await page.waitForTimeout(500);
    }
    const cards = await page.evaluate(() => Array.prototype.slice.call(document.querySelectorAll(".tool-card")).map((c) => ({ cls: c.className, name: (c.querySelector(".tool-card-name") || {}).textContent, status: (c.querySelector(".tool-card-status") || {}).textContent })));
    if (cards.length > 0) PASS("B-toolcard-exists", "工具卡片渲染出来了", cards);
    else FAIL("B-toolcard-exists", "一条工具卡片都没有（工具事件没渲染）");
    if (sawRunning) PASS("B-toolcard-running", "观察到「运行中」态（is-running，由 MutationObserver 记录，轮询漏看不影响判定）");
    else FAIL("B-toolcard-running", "整个流里没看到 is-running 态");
    const okCards = cards.filter((c) => c.cls.indexOf("is-ok") >= 0 && c.status === "完成");
    if (okCards.length > 0) PASS("B-toolcard-ok", "有卡片转成完成态", okCards.length);
    else FAIL("B-toolcard-ok", "没有完成态卡片", cards);
    const det = await page.evaluate(() => { const d = document.querySelector(".tool-card-detail"); return d ? { exists: true, open: d.open, summary: (d.querySelector("summary") || {}).textContent, len: ((d.querySelector(".tool-card-body") || {}).textContent || "").length } : { exists: false }; });
    if (det.exists && det.open === false && det.len > 0) PASS("B-toolcard-collapsible", "完整结果在 <details> 里默认折叠且非空", det);
    else FAIL("B-toolcard-collapsible", "折叠结构不对", det);
    if (det.exists) {
      await page.click(".tool-card-detail > summary");
      await page.waitForTimeout(400);
      const openedDet = await page.evaluate(() => { const d = document.querySelector(".tool-card-detail"); return { open: d.open, body: ((d.querySelector(".tool-card-body") || {}).textContent || "").slice(0, 120) }; });
      if (openedDet.open && openedDet.body) PASS("B-toolcard-expand", "点开后能看到结果正文", openedDet);
      else FAIL("B-toolcard-expand", "点开后没有正文", openedDet);
    }
    const copyPath = FS_ROOT + "/copy.md";
    const copy = readText(copyPath);
    if (copy && copy.indexOf("UI-VERIFY-7731") >= 0) PASS("B-real-model-wrote", "盘上真出现了 copy.md，内容是模型自主写出的", { path: copyPath, head: copy.slice(0, 80) });
    else FAIL("B-real-model-wrote", "模型没有真的写文件", { path: copyPath, got: copy === null ? null : copy.slice(0, 120) });

    // 长结果内部滚动
    await page.fill("#input", "再用 read 工具读取 big.txt，把内容给我看（我会自己看完整结果）。");
    await page.click("#send-btn");
    await page.waitForTimeout(1000);
    for (let i = 0; i < 200; i++) {
      const n = await page.evaluate(() => document.querySelectorAll(".tool-card").length);
      const last = await page.evaluate(() => { const cs = document.querySelectorAll(".tool-card"); const c = cs[cs.length - 1]; return c ? c.className : ""; });
      if (n >= 3 && last.indexOf("is-running") < 0) break;
      await page.waitForTimeout(500);
    }
    const scrollInfo = await page.evaluate(() => {
      const cs = document.querySelectorAll(".tool-card");
      const c = cs[cs.length - 1];
      if (!c) return null;
      const d = c.querySelector(".tool-card-detail");
      if (d && !d.open) d.open = true;
      const pre = c.querySelector(".tool-card-body");
      if (!pre) return { hasBody: false, cls: c.className };
      return { hasBody: true, cls: c.className, overflowY: getComputedStyle(pre).overflowY, scrollH: pre.scrollHeight, clientH: pre.clientHeight, maxH: getComputedStyle(pre).maxHeight };
    });
    if (scrollInfo && scrollInfo.hasBody && scrollInfo.scrollH > scrollInfo.clientH && (scrollInfo.overflowY === "auto" || scrollInfo.overflowY === "scroll")) PASS("B-toolcard-scroll", "长结果限高 + 内部滚动", scrollInfo);
    else FAIL("B-toolcard-scroll", "长结果没有内部滚动", scrollInfo);

    // 失败态
    await page.fill("#input", "请用 read 工具读取 C:/Windows/win.ini —— 我知道它可能在允许目录之外，请照指令尝试一次，把结果告诉我。");
    await page.click("#send-btn");
    let sawErr = null;
    for (let i = 0; i < 300; i++) {
      sawErr = await page.evaluate(() => { const cs = document.querySelectorAll(".tool-card"); const c = cs[cs.length - 1]; return c ? { cls: c.className, status: (c.querySelector(".tool-card-status") || {}).textContent, summary: (c.querySelector(".tool-card-summary") || {}).textContent } : null; });
      if (sawErr && sawErr.cls.indexOf("is-error") >= 0) break;
      await page.waitForTimeout(500);
    }
    if (sawErr && sawErr.cls.indexOf("is-error") >= 0 && sawErr.status === "失败") PASS("B-toolcard-error", "工具失败时卡片转失败态", sawErr);
    else FAIL("B-toolcard-error", "失败态没出现", sawErr);
  } else {
    SKIP("B-toolcard-*", "未开 --with-model，跳过工具卡片与真模型复现（那是另一条独立跑的命令）");
  }

  // 7) 零真实 JS 异常（/favicon.ico 404 是既有现象，单独排除并记录）
  const favOnly = badResponses.filter((x) => x.indexOf("/favicon.ico") < 0);
  const favConsole = consoleErrors.filter((x) => x.url.indexOf("/favicon.ico") >= 0);
  const consoleClean = consoleErrors.filter((x) => x.url.indexOf("/favicon.ico") < 0);
  if (favConsole.length > 0) INFO("B-favicon-404", "favicon 404（既有现象：HEAD 里就没有该路由/文件，按 URL 排除，不算 JS 异常）", favConsole);
  INFO("B-console-raw", "记录到的原始噪声", { consoleErrors: consoleErrors.length, badResponses: badResponses.length, failedReqs: failedReqs.length, favicon404: badResponses.length - favOnly.length });
  if (pageErrors.length === 0) PASS("B-no-pageerror", "零 pageerror（未捕获 JS 异常）");
  else FAIL("B-no-pageerror", "有未捕获 JS 异常", pageErrors.slice(0, 3));
  if (consoleClean.length === 0) PASS("B-no-console-error", "除 favicon 外零 console.error", { faviconExcluded: favConsole.length, remaining: 0 });
  else FAIL("B-no-console-error", "有 console.error（已排除 favicon）", consoleClean.slice(0, 3));
  if (favOnly.length === 0) PASS("B-no-bad-response", "除 /favicon.ico 外没有 4xx/5xx 资源", favOnly.slice(0, 5));
  else FAIL("B-no-bad-response", "有非 favicon 的坏响应", favOnly.slice(0, 5));

  await browser.close();
}

main().then(() => {
  const p = results.filter((r) => r.level === "PASS").length;
  const f = results.filter((r) => r.level === "FAIL").length;
  const s = results.filter((r) => r.level === "SKIP").length;
  console.log("");
  console.log("=== 汇总: PASS=" + p + " FAIL=" + f + " SKIP=" + s + " ===");
  if (f > 0) { console.log("FAIL 项: " + results.filter((r) => r.level === "FAIL").map((r) => r.id).join(", ")); process.exit(1); }
  if (p === 0) { console.log("一条都没跑成（前置缺失）"); process.exit(2); }
  process.exit(0);
}).catch((e) => { console.log("探针自身异常: " + String(e && e.stack ? e.stack : e)); process.exit(2); });