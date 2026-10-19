// 导航重排（横排 6 页签 -> 竖排分组）的浏览器验证探针。
// 复用 gstack skill 装的 playwright-core 1.58.2 与本机已缓存的 chromium-1223，
// 显式指定 executablePath 是因为本机没装 headless_shell-1223，用完整 chrome 跑无头。
const { chromium } = await import("file:///C:/Users/woshiss/.claude/skills/gstack/node_modules/playwright-core/index.mjs");
import { writeFileSync } from "node:fs";

const BASE = "http://127.0.0.1:8099";
const CHROME = "C:\\Users\\wossiss".replace("wossiss", "woshiss") + "\\AppData\\Local\\ms-playwright\\chromium-1223\\chrome-win64\\chrome.exe";
const OUT = "E:\\blog-my\\docs\\verify\\";
const lines = [];
let fail = 0;
const log = (s) => { lines.push(s); console.log(s); };
const check = (name, ok, detail) => {
  log((ok ? "[PASS] " : "[FAIL] ") + name + (detail ? "  " + detail : ""));
  if (!ok) fail++;
};

// 等服务就绪
let ready = false;
for (let i = 0; i < 60; i++) {
  try { const r = await fetch(BASE + "/api/health"); if (r.ok) { ready = true; break; } } catch {}
  await new Promise(res => setTimeout(res, 500));
}
if (!ready) { console.log("服务没起来"); process.exit(2); }

const browser = await chromium.launch({ headless: true, executablePath: CHROME });
const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
const page = await ctx.newPage();

// 注册 + 登录（与 verify-skills.ps1 同款表单流程，cookie 留在 context 里）
const user = "e2e_navp_" + Math.floor(Math.random() * 1e6);
await ctx.request.post(BASE + "/register", { form: { username: user, password: "e2e-pass-123" } });
await ctx.request.post(BASE + "/login", { form: { username: user, password: "e2e-pass-123" } });
await page.goto(BASE + "/", { waitUntil: "domcontentloaded" });
await page.waitForSelector(".view-nav .view-tab", { timeout: 15000 });
await page.waitForTimeout(600);

// 1) 页签数量与顺序
const tabs = await page.$$eval("nav.view-nav a.view-tab", els => els.map(e => ({
  view: e.getAttribute("data-view"),
  text: e.textContent.trim().replace(/测试版$/, ""),
  x: e.getBoundingClientRect().x,
  y: e.getBoundingClientRect().y,
  w: e.getBoundingClientRect().width,
  h: e.getBoundingClientRect().height,
})));
const order = tabs.map(t => t.view).join(",");
check("页签 6 个且顺序 = 对话,技能,助手,提示词,模型,电脑操控", tabs.length === 6 && order === "chat,skills,assistants,prompts,models,cua", order);
check("竖排：所有页签同 x、y 严格递增", tabs.every(t => Math.abs(t.x - tabs[0].x) < 2) && tabs.every((t, i) => i === 0 || t.y > tabs[i - 1].y), "x=" + tabs[0].x.toFixed(0));
check("不挤了：每个页签行宽 ≥ 150px（旧横排约 37px）", tabs.every(t => t.w >= 150), "宽=" + tabs.map(t => t.w.toFixed(0)).join("/"));
check("页签文案完整", ["对话","技能","助手","提示词","模型","电脑操控"].every((n, i) => tabs[i].text.startsWith(n)), tabs.map(t => t.text).join("|"));

// 2) 分组标签
const tags = await page.$$eval(".nav-group-tag", els => els.map(e => e.textContent.trim()));
check("两个分组标签 LIBRARY / LABS", tags.length === 2 && tags[0] === "LIBRARY" && tags[1] === "LABS", tags.join("|"));
const beta = await page.$eval("a.view-tab[data-view=cua] .view-tab-beta", e => {
  const r = e.getBoundingClientRect(); const p = e.closest("a").getBoundingClientRect();
  return { inline: r.width > 0 && r.x >= p.x && r.x < p.x + p.width, text: e.textContent.trim() };
});
check("测试版徽标是行内（不再浮出页签外）", beta.inline && beta.text === "测试版", JSON.stringify(beta));

// 3) 逐个点页签：.app[data-view] 跟着切、顶栏标题跟着变、目标视图可见
for (const v of ["skills", "assistants", "prompts", "models", "cua", "chat"]) {
  await page.click("a.view-tab[data-view=\"" + v + "\"]");
  await page.waitForTimeout(350);
  if (v === "cua") { // 每次进 CUA 都弹测试版警告（契约要求），先关掉再断言
    const open = await page.$eval("#cua-beta-dialog", d => d.open);
    if (open) await page.click("#cua-beta-ok");
    await page.waitForTimeout(200);
  }
  const st = await page.$eval(".app", a => a.getAttribute("data-view"));
  const title = await page.$eval("#topbar-title", t => t.textContent.trim());
  const vis = await page.$eval("#view-" + (v === "chat" ? "" : v) || (v === "chat" ? "#messages" : "#view-" + v), () => true).catch(() => null);
  const visible = v === "chat"
    ? await page.$eval("#messages", m => m.getClientRects().length > 0)
    : await page.$eval("#view-" + v, s => s.getClientRects().length > 0 && getComputedStyle(s).display !== "none");
  check("点「" + tabs.find(t => t.view === v).text + "」→ app[data-view]=" + v + " 且视图可见", st === v && visible, "标题=" + title);
}

// 4) 截图（记录用）
await page.click("a.view-tab[data-view=chat]");
await page.waitForTimeout(400);
await page.screenshot({ path: OUT + "nav-rearrange-chat.png" });
await page.screenshot({ path: OUT + "nav-rearrange-sidebar.png", clip: { x: 0, y: 0, width: 264, height: 800 } });
await page.click("a.view-tab[data-view=skills]");
await page.waitForTimeout(500);
await page.screenshot({ path: OUT + "nav-rearrange-skills.png" });
log("已存 3 张截图：nav-rearrange-chat/sidebar/skills.png");

// 5) 窄屏（≤820px 侧栏是 off-canvas 抽屉）：汉堡可见、打开后页签可点
await page.setViewportSize({ width: 390, height: 800 });
await page.waitForTimeout(300);
const burgerVisible = await page.$eval("#sidebar-toggle", b => b.offsetParent !== null);
const sidebarShifted = await page.$eval("#sidebar", s => { const r = s.getBoundingClientRect(); return r.x < 0; });
check("390px：汉堡按钮可见、侧栏移出画布", burgerVisible && sidebarShifted);
await page.click("#sidebar-toggle");
await page.waitForTimeout(350);
const drawerOpen = await page.$eval("#sidebar", s => s.getBoundingClientRect().x >= 0);
check("390px：点汉堡后抽屉滑入", drawerOpen);
await page.click("a.view-tab[data-view=skills]");
await page.waitForTimeout(350);
const narrowView = await page.$eval(".app", a => a.getAttribute("data-view"));
check("390px：抽屉里页签可点且切到技能", narrowView === "skills", "data-view=" + narrowView);
await page.screenshot({ path: OUT + "nav-rearrange-narrow.png" });

writeFileSync(OUT + "nav-rearrange-log.txt", lines.join("\n") + "\n\n结果: " + (fail === 0 ? "全部通过" : fail + " 项失败") + "\n账号: " + user + "\n", "utf8");
log("结果: " + (fail === 0 ? "全部通过" : fail + " 项失败") + "（探针账号 " + user + "，待清理）");
await browser.close();
process.exit(fail === 0 ? 0 : 1);
