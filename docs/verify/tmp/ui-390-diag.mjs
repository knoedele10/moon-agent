import { existsSync } from "node:fs";
import { createRequire } from "node:module";
const require2 = createRequire(import.meta.url);
const mod = require2("C:/Users/woshiss/AppData/Roaming/npm/node_modules/openclaw/node_modules/playwright-core");
const exe = "C:/Users/woshiss/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe";
const BASE = "http://127.0.0.1:10722";
const b = await mod.chromium.launch({ headless: true, executablePath: exe, args: ["--no-sandbox"] });
const ctx = await b.newContext({ viewport: { width: 1440, height: 900 } });
const u = "diag390_" + Date.now().toString(36);
await ctx.request.post(BASE + "/register", { form: { username: u, password: "Verify-" + u + "-2026" } });
await ctx.request.post(BASE + "/login", { form: { username: u, password: "Verify-" + u + "-2026" } });
const page = await ctx.newPage();
await page.goto(BASE + "/", { waitUntil: "load" });
await page.waitForSelector("#input", { timeout: 20000 });
function measure(label) {
  return page.evaluate((lb) => {
    const out = { label: lb, vw: window.innerWidth, sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth };
    const tabs = Array.prototype.slice.call(document.querySelectorAll("a.view-tab"));
    out.tabs = tabs.map((t) => { const r = t.getBoundingClientRect(); return { v: t.getAttribute("data-view"), x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) }; });
    const nav = document.querySelector(".view-tabs") || (tabs[0] ? tabs[0].parentElement : null);
    if (nav) { const r = nav.getBoundingClientRect(); out.nav = { cls: nav.className, x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height), overflowX: getComputedStyle(nav).overflowX, display: getComputedStyle(nav).display }; }
    const ko = document.getElementById("keys-open");
    if (ko) { const r = ko.getBoundingClientRect(); out.keysOpen = { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), right: Math.round(r.right), visible: !!ko.offsetParent }; }
    out.bodyOverflowX = getComputedStyle(document.body).overflowX;
    out.htmlOverflowX = getComputedStyle(document.documentElement).overflowX;
    return out;
  }, label);
}
console.log("--- 1440px ---");
console.log(JSON.stringify(await measure("1440"), null, 1));
await page.setViewportSize({ width: 390, height: 844 });
await page.waitForTimeout(2500);
console.log("--- 390px ---");
console.log(JSON.stringify(await measure("390"), null, 1));
await page.screenshot({ path: "docs/verify/tmp/ui-390-chat.png", fullPage: false });
try {
  await page.click("a.view-tab[data-view=\"chat\"]", { timeout: 5000 });
  console.log("普通点击 chat = 成功");
} catch (e) {
  console.log("普通点击 chat = 失败: " + String(e).split("\n")[0]);
  const h = await page.$("a.view-tab[data-view=\"chat\"]");
  try { await h.click({ force: true, timeout: 5000 }); console.log("force 点击 chat = 成功（说明只是可点性/稳定性问题，不是布局外）"); }
  catch (e2) { console.log("force 点击 chat 也失败: " + String(e2).split("\n")[0]); }
}
for (const v of ["chat", "models", "cua"]) {
  await page.evaluate((vv) => { location.hash = "#/" + vv; }, v);
  await page.waitForTimeout(1200);
  const open = await page.evaluate(() => { const d = document.getElementById("cua-beta-dialog"); return !!(d && d.open); });
  if (open) { await page.keyboard.press("Escape"); await page.waitForTimeout(600); }
  const m = await measure(v);
  console.log("--- 390px view=" + v + " : sw=" + m.sw + " cw=" + m.cw + " 溢出=" + (m.sw > m.cw + 1) + " ---");
  await page.screenshot({ path: "docs/verify/tmp/ui-390-" + v + ".png", fullPage: false });
}
await b.close();
