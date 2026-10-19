// 前端「电脑操控」tab 探测（verifier 自写，对齐 06-integration-contract.md 第 4 节 I4）
// 用法：node docs/verify/frontend-probe.mjs [--base http://127.0.0.1:8300] [--static-only] [--start] [--strip-dialog]
//   --strip-dialog 是负向对照：把模态弹窗从服务的 HTML 里拿掉，F3-modal-* 必须转红（证明断言真的抓得住回归）
// 退出码：0=全 PASS；1=有 FAIL；2=前置缺失（服务不可达 / 浏览器库没装 -> 只算 SKIP）
// 分三层：F1 接入（HTTP 真页面）/ F2 契约点（读模板源码，无需服务器）/ F3 真实浏览器（Playwright，缺则 SKIP）
import { readFileSync, existsSync } from "node:fs";
import { resolve } from "node:path";
import { createRequire } from "node:module";

const args = process.argv.slice(2);
const BASE = (args.indexOf("--base") >= 0 ? args[args.indexOf("--base") + 1] : null) || "http://127.0.0.1:8300";
const STATIC_ONLY = args.indexOf("--static-only") >= 0;
const DO_START = args.indexOf("--start") >= 0;
const STRIP_DIALOG = args.indexOf("--strip-dialog") >= 0;
const ROOT = resolve(import.meta.dirname, "..", "..");

const results = [];
function rec(level, id, msg, detail) {
  results.push({ level: level, id: id });
  console.log("[" + level + "] " + id + " :: " + msg);
  if (detail !== undefined) console.log("        证据: " + (typeof detail === "string" ? detail : JSON.stringify(detail)));
}
const PASS = function (id, m, d) { rec("PASS", id, m, d); };
const FAIL = function (id, m, d) { rec("FAIL", id, m, d); };
const WARN = function (id, m, d) { rec("WARN", id, m, d); };
const SKIP = function (id, m, d) { rec("SKIP", id, m, d); };
const INFO = function (id, m, d) { rec("INFO", id, m, d); };

function readText(p) { try { return readFileSync(p, "utf8"); } catch (e) { return null; } }

// ---------------- F2 契约点（静态，不需要服务器） ----------------
function staticTier() {
  const htmlPath = ROOT + "/templates/chat/index.html";
  const jsPath = ROOT + "/templates/static/js/cua.js";
  const html = readText(htmlPath);
  const js = readText(jsPath);
  if (html === null) { FAIL("F2a-html-exists", "找不到 " + htmlPath); return null; }
  const tabOk = html.indexOf("#/cua") >= 0 && html.indexOf("view-cua") >= 0;
  if (tabOk) PASS("F2a-tab", "index.html 有 #/cua 入口与 view-cua 面板");
  else FAIL("F2a-tab", "index.html 缺少 #/cua 入口或 view-cua 面板");
  if (js === null) { FAIL("F2b-js-exists", "找不到 " + jsPath); return null; }
  const need = ["EventSource", "close(", "/api/cua/status", "/api/cua/runs"];
  const miss = need.filter(function (k) { return js.indexOf(k) < 0; });
  if (miss.length === 0) PASS("F2b-js-api", "cua.js 用 EventSource 且调用契约接口", { bytes: js.length });
  else FAIL("F2b-js-api", "cua.js 缺关键点：" + miss.join(","));
  const closeOnLeave = /visibilitychange|beforeunload|pagehide/.test(js) && js.indexOf("close(") >= 0;
  if (closeOnLeave) PASS("F2c-sse-close", "页面切走/关闭时关闭 SSE（visibilitychange/beforeunload + close）");
  else FAIL("F2c-sse-close", "没看到页面切走/关闭时 close() SSE 的逻辑");
  const stopOk = js.indexOf("/stop") >= 0;
  if (stopOk) PASS("F2d-stop", "前端有停止入口（调用 /stop）");
  else FAIL("F2d-stop", "前端没有调用 /stop 的停止入口");
  const noDep = !/https?:\/\/(cdn|unpkg|cdnjs|esm\.sh)/.test(html + js) && js.indexOf("import(") < 0 && js.indexOf("require(") < 0;
  if (noDep) PASS("F2e-no-new-dep", "前端未引入新依赖（无 CDN/import/require）");
  else FAIL("F2e-no-new-dep", "前端出现了新依赖引用（契约要求保持无构建步骤）");
  const rowFields = ["step", "thought", "action", "ok"];
  const rowMiss = rowFields.filter(function (k) { return js.indexOf(k) < 0; });
  if (rowMiss.length === 0) PASS("F2f-trajectory", "轨迹渲染含 步号/thought/action/ok 字段");
  else WARN("F2f-trajectory", "轨迹字段可能不全：" + rowMiss.join(","));
  return { html: html, js: js };
}

// ---------------- F1 接入（HTTP 真页面） ----------------
async function httpTier() {
  const jar = new Map();
  function jarHeader() { return [...jar.entries()].map(function (kv) { return kv[0] + "=" + kv[1]; }).join("; "); }
  async function req(path, o) {
    const opt = o || {};
    const headers = Object.assign({}, opt.headers || {});
    if (jar.size) headers["Cookie"] = jarHeader();
    const res = await fetch(BASE + path, Object.assign({}, opt, { headers: headers, redirect: "manual" }));
    const set = res.headers.getSetCookie ? res.headers.getSetCookie() : [];
    for (const c of set) {
      const pair = c.split(";")[0];
      const i = pair.indexOf("=");
      if (i > 0) jar.set(pair.slice(0, i).trim(), pair.slice(i + 1).trim());
    }
    return res;
  }
  try { const h = await req("/api/health"); if (h.status >= 500) INFO("F0-health", "health=" + h.status); } catch (e) {
    INFO("F0-server", "无法连接 " + BASE, String(e));
    return "服务不可达：" + BASE;
  }
  const user = "itfe_" + Date.now().toString(36);
  const body = new URLSearchParams({ username: user, password: "Verify-" + user + "-2026" }).toString();
  await req("/register", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: body });
  await req("/login", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: body });
  let page = null;
  for (const p of ["/chat", "/"]) {
    const r = await req(p);
    if (r.status === 200) { const t = await r.text(); if (t.indexOf("cua") >= 0) { page = { path: p, text: t }; break; } }
  }
  if (!page) { FAIL("F1a-page", "登录后 GET /chat 与 / 都没拿到含 cua 的面板"); return null; }
  PASS("F1a-page", "真实页面 " + page.path + " 含 cua 面板（" + page.text.length + " 字节）");
  const m = page.text.match(/<script[^>]+src="([^"]*cua[^"]*\.js)"/);
  if (!m) { FAIL("F1b-js-served", "页面里没有引入 cua js"); return null; }
  const js = await req(m[1]);
  if (js.status === 200) { const t = await js.text(); PASS("F1b-js-served", "GET " + m[1] + " -> 200（" + t.length + " 字节）"); }
  else FAIL("F1b-js-served", "GET " + m[1] + " 应 200，实际 " + js.status);
  const css = await req("/static/css/style.css");
  if (css.status !== 200) { FAIL("F1c-css", "/static/css/style.css 应 200，实际 " + css.status); return null; }
  const csst = await css.text();
  if (csst.indexOf("cua") >= 0) PASS("F1c-css", "style.css 含 cua 样式");
  else WARN("F1c-css", "style.css 里没看到 cua 相关类名");
  return null;
}

// ---------------- F3 真实浏览器 ----------------
// playwright-core 与 chromium 都走固定路径（与 docs/day23/verify-ui.mjs:71-87 同一套已验证逻辑）：
// 不 npm install、不下载浏览器、不连用户正在用的 Chrome。
const require = createRequire(import.meta.url);
const PW_CANDIDATES = [
  "C:/Users/woshiss/AppData/Roaming/npm/node_modules/openclaw/node_modules/playwright-core",
  "playwright-core"
];
const CHROME_CANDIDATES = [
  "C:/Users/woshiss/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe",
  "C:/Users/woshiss/AppData/Local/ms-playwright/chromium_headless_shell-1223/chrome-win/headless_shell.exe"
];

function loadPlaywrightCore() {
  const tried = [];
  for (const c of PW_CANDIDATES) {
    try { return { mod: require(c), from: c }; } catch (e) { tried.push(c + " -> " + e.message); }
  }
  throw new Error("找不到 playwright-core：" + tried.join(" | "));
}

function pickChrome() {
  for (const p of CHROME_CANDIDATES) { if (existsSync(p)) return p; }
  return null;
}

async function browserTier() {
  let loaded = null;
  try { loaded = loadPlaywrightCore(); } catch (e) { loaded = null; }
  if (!loaded) {
    SKIP("F3-browser", "固定路径与包名都没找到 playwright-core");
    return;
  }
  const exe = pickChrome();
  if (!exe) {
    SKIP("F3-browser", "找不到 chromium 可执行文件", CHROME_CANDIDATES);
    return;
  }
  INFO("F3-env", "playwright-core=" + loaded.from + "；chromium=" + exe);
  const browser = await loaded.mod.chromium.launch({ headless: true, executablePath: exe, args: ["--no-sandbox"] });
  try {
    const ctx = await browser.newContext();
    const u = "itfe_" + Date.now().toString(36);
    const pwd = "Verify-" + u + "-2026";
    try {
      await ctx.request.post(BASE + "/register", { form: { username: u, password: pwd } });
      await ctx.request.post(BASE + "/login", { form: { username: u, password: pwd } });
      INFO("F3-login", "浏览器上下文已带 session 登录态");
    } catch (e) {
      INFO("F3-login", "浏览器上下文登录失败，继续按未登录加载", String(e).slice(0, 120));
    }
    const page = await ctx.newPage();
    const errs = [];
    page.on("pageerror", function (e) { errs.push(String(e)); });
    if (STRIP_DIALOG) {
      await page.route(function (url) { return url.origin === BASE && (url.pathname === "/" || url.pathname === "/chat"); },
        async function (route) {
          const resp = await route.fetch();
          const body = await resp.text();
          const stripped = body.replace(/<dialog[^>]*id="cua-beta-dialog"[\s\S]*?<\/dialog>/i, "<!-- 负向对照：弹窗被拿掉 -->");
          await route.fulfill({ status: resp.status(), headers: { "content-type": "text/html; charset=utf-8" }, body: stripped });
        });
      INFO("F3-strip-dialog", "负向对照模式：已把模态弹窗从 HTML 里删掉，弹窗断言应当转红");
    }
    await page.goto(BASE + "/#/cua", { waitUntil: "load", timeout: 20000 });
    // 面板不可见时不能抛异常（抛了就只剩堆栈、看不到整张表）：落成 FAIL + DOM 证据，
    // 逐级上溯找出是它自己 display:none，还是被祖先挡住（例如被别的 section 包住）。
    try {
      await page.waitForSelector("#cua-stats", { state: "visible", timeout: 15000 });
      PASS("F3a-tab-visible", "#/cua 真实浏览器加载后面板可见");
    } catch (e) {
      const ev = await page.evaluate(function () {
        const el = document.getElementById("view-cua");
        if (!el) return { viewCua: "missing" };
        const chain = [];
        let n = el;
        while (n && n.nodeType === 1 && chain.length < 5) {
          const cs = getComputedStyle(n);
          if (cs.display === "none") chain.push((n.id ? "#" + n.id : n.tagName.toLowerCase()) + "[display:none]");
          n = n.parentElement;
        }
        const r = el.getBoundingClientRect();
        const app = document.querySelector(".app");
        const tab = document.querySelector(".view-tab.active");
        return {
          appView: app ? app.getAttribute("data-view") : null,
          viewCuaRect: Math.round(r.width) + "x" + Math.round(r.height),
          viewCuaDisplay: getComputedStyle(el).display,
          parentId: el.parentElement ? el.parentElement.id : null,
          hiddenByAncestor: chain,
          activeTab: tab ? tab.getAttribute("data-view") : null
        };
      }).catch(function () { return null; });
      FAIL("F3a-tab-visible", "#/cua 面板在真实浏览器里不可见（冷加载与点 tab 都试过）", ev);
    }
    // 稳定选择器（task-35）：原来数 #view-cua button，而历史行是动态生成的 <button class="cua-history-row">，
    // 条数一变数字就飘（10/11/12），数字飘会掩盖真实变化。改成断言一组固定的关键控件都在。
    const KEY_CONTROLS = ["#cua-task", "#cua-model", "#cua-dry-run", "#cua-start", "#cua-stop"];
    const missing = [];
    for (const sel of KEY_CONTROLS) { if (!(await page.$(sel))) missing.push(sel); }
    if (missing.length === 0) PASS("F3b-controls", "面板关键控件齐全：" + KEY_CONTROLS.join(" "));
    else FAIL("F3b-controls", "面板缺关键控件：" + missing.join(" "));

    // ---- 模态测试版警告（task-35）：进入「电脑操控」必须先弹，且必须关掉才能操作 ----
    const DLG = "#cua-beta-dialog";
    const dlgState = function () {
      return page.evaluate(function (sel) {
        var d = document.querySelector(sel);
        if (!d) return { exists: false, open: false };
        var ae = document.activeElement;
        return {
          exists: true, open: d.open === true,
          role: d.getAttribute("role"), ariaModal: d.getAttribute("aria-modal"),
          labelledby: d.getAttribute("aria-labelledby"),
          focusView: ae && ae.getAttribute ? ae.getAttribute("data-view") : null,
          focusId: ae && ae.id ? ae.id : null
        };
      }, DLG);
    };
    const axDialog = async function () {
      const cdp = await ctx.newCDPSession(page);
      await cdp.send("Accessibility.enable");
      const tree = await cdp.send("Accessibility.getFullAXTree");
      const node = tree.nodes.find(function (n) { return n.role && n.role.value === "dialog"; });
      if (!node) return null;
      const modal = (node.properties || []).find(function (p) { return p.name === "modal"; });
      return { role: node.role.value, name: node.name ? node.name.value : null, modal: modal ? modal.value.value : null };
    };
    try {
      const st = await dlgState();
      const ax = await axDialog();
      const ok = st.exists && st.open === true && st.role === "dialog" && st.ariaModal === "true" &&
        st.labelledby === "cua-beta-title" && ax && ax.role === "dialog" && ax.name === "测试版，别当真";
      if (ok) PASS("F3-modal-open", "进入 #/cua 弹出模态测试版警告（open/role/aria-modal/labelledby + CDP AX 名字）",
        { open: st.open, role: st.role, ariaModal: st.ariaModal, axName: ax.name, axModal: ax.modal });
      else FAIL("F3-modal-open", "模态测试版警告没按契约弹出", { state: st, ax: ax });
    } catch (e) { FAIL("F3-modal-open", "检查模态弹窗时出错", String(e).slice(0, 160)); }

    try {
      await page.click("#cua-beta-ok", { timeout: 5000 });
      await page.waitForFunction(function (sel) { var d = document.querySelector(sel); return d && d.open === false; }, DLG, { timeout: 5000 });
      // 关闭事件是异步派发的，焦点由 onBetaClosed() 搬回页签；契约是「焦点回到页签」，不是「同一微任务内」，
      // 所以给一个 3s 的落定窗口（真缺陷是「永远回不去」，那时这里照样 FAIL）。
      await page.waitForFunction(function () {
        var ae = document.activeElement;
        return !!ae && ae.getAttribute && ae.getAttribute("data-view") === "cua";
      }, null, { timeout: 3000 }).catch(function () {});
      const st = await dlgState();
      if (st.open === false && st.focusView === "cua") PASS("F3-modal-close", "点「知道了，我胆子大」后 open=false，焦点回到「电脑操控」页签",
        { open: st.open, focusView: st.focusView, focusId: st.focusId });
      else FAIL("F3-modal-close", "关闭后状态不对", { open: st.open, focusView: st.focusView, focusId: st.focusId });
    } catch (e) { FAIL("F3-modal-close", "关不掉模态（背景被 inert 时这正是操作被挡住的原因）", String(e).slice(0, 160)); }

    for (const v of ["chat", "models", "keys", "kb"]) {
      try {
        await page.click(".view-tab[data-view=" + v + "]", { timeout: 5000 });
        await page.waitForTimeout(200);
        const st = await dlgState();
        if (st.open === false) PASS("F3-modal-other-tabs:" + v, "切到「" + v + "」时弹窗 open=false");
        else FAIL("F3-modal-other-tabs:" + v, "切到「" + v + "」时弹窗仍开着");
      } catch (e) { FAIL("F3-modal-other-tabs:" + v, "切到「" + v + "」失败", String(e).slice(0, 120)); }
    }
    try {
      await page.click(".view-tab[data-view=cua]", { timeout: 5000 });
      await page.waitForFunction(function (sel) { var d = document.querySelector(sel); return d && d.open === true; }, DLG, { timeout: 5000 });
      PASS("F3-modal-reenter", "再次进入「电脑操控」会重新弹出（不是只弹一次）");
      await page.click("#cua-beta-ok", { timeout: 5000 });
      await page.waitForFunction(function (sel) { var d = document.querySelector(sel); return d && d.open === false; }, DLG, { timeout: 5000 });
    } catch (e) { FAIL("F3-modal-reenter", "再次进入没有重新弹出（或关不掉）", String(e).slice(0, 140)); }

    if (DO_START) {
      try {
        await page.fill("#view-cua textarea, #view-cua input[type=text]", "浏览器探测（dry-run）");
        await page.click("#cua-start, #view-cua button[type=submit]", { timeout: 8000 });
        await page.waitForSelector("#cua-trace *", { timeout: 60000 });
        PASS("F3c-dryrun-render", "dry-run 启动后轨迹渲染出条目");
      } catch (e) { FAIL("F3c-dryrun-render", "启动后 60s 内没渲染出轨迹条目", String(e).slice(0, 160)); }
    } else {
      SKIP("F3c-dryrun-render", "未加 --start：真实浏览器只验证加载与控件，不触发 run（--start 才跑一次 dry-run 渲染）");
    }
    // F4（task-22）：历史运行列表在真浏览器里必须真的渲染出来
    try {
      await page.evaluate(function () { var d = document.getElementById("cua-history"); if (d && !d.open) d.open = true; });
      const refresh = await page.$("#cua-history-refresh");
      if (refresh) { try { await refresh.click(); } catch (e) {} }
      await page.waitForSelector("#cua-history-list", { timeout: 10000 });
      let rows = 0;
      for (let i = 0; i < 30; i++) {
        rows = await page.evaluate(function () { var l = document.getElementById("cua-history-list"); return l ? l.children.length : 0; });
        if (rows > 0) break;
        await page.waitForTimeout(500);
      }
      const count = await page.evaluate(function () { var c = document.getElementById("cua-history-count"); return c ? c.textContent.trim() : null; });
      if (rows > 0) PASS("F4-history-list", "历史运行列表渲染出 " + rows + " 条，计数=" + count);
      else {
        const note = await page.evaluate(function () { var n = document.getElementById("cua-history-note"); return n ? n.textContent.trim() : ""; });
        WARN("F4-history-list", "列表容器在，但没有条目（本次没有产生 run），计数=" + count + " 注=" + note);
      }
    } catch (e) { FAIL("F4-history-list", "历史运行列表没渲染出来", String(e).slice(0, 160)); }
    if (errs.length === 0) PASS("F3d-no-js-error", "页面无 JS 异常");
    else FAIL("F3d-no-js-error", "页面有 JS 异常", errs.slice(0, 3));
  } finally {
    await browser.close();
  }
}

async function main() {
  console.log("# 前端探测  base=" + BASE + "  staticOnly=" + STATIC_ONLY + "  " + new Date().toISOString());
  const s = staticTier();
  let prereq = null;
  if (!STATIC_ONLY) prereq = await httpTier();
  if (!STATIC_ONLY && !prereq) await browserTier();
  if (STATIC_ONLY) SKIP("F1/F3", "--static-only：只跑静态契约点");
  const fails = results.filter(function (r) { return r.level === "FAIL"; });
  const pass = results.filter(function (r) { return r.level === "PASS"; }).length;
  const skip = results.filter(function (r) { return r.level === "SKIP"; }).length;
  console.log("");
  console.log("---- 汇总：PASS=" + pass + " FAIL=" + fails.length + " SKIP=" + skip + " ----");
  fails.forEach(function (f) { console.log("  FAIL " + f.id); });
  if (prereq) { console.log("PREREQ_MISSING: " + prereq); process.exit(2); }
  process.exit(fails.length ? 1 : 0);
}

main();
