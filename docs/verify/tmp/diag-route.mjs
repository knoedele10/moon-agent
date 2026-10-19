// 临时诊断：冷加载 #/cua 与点击 tab 的真实 DOM 事实
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
const pw = require("C:/Users/woshiss/AppData/Roaming/npm/node_modules/openclaw/node_modules/playwright-core");
const BASE = process.argv[2] || "http://127.0.0.1:8300";
const exe = "C:/Users/woshiss/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe";
const browser = await pw.chromium.launch({ headless: true, executablePath: exe, args: ["--no-sandbox"] });
const ctx = await browser.newContext();
const u = "fe" + Date.now().toString(36);
await ctx.request.post(BASE + "/register", { form: { username: u, password: "Verify-2026-abc" } });
await ctx.request.post(BASE + "/login", { form: { username: u, password: "Verify-2026-abc" } });
const page = await ctx.newPage();
const errs = [];
page.on("pageerror", function (e) { errs.push(String(e).slice(0, 200)); });
page.on("console", function (m) { if (m.type() === "error") errs.push("console: " + m.text().slice(0, 160)); });
async function snap(tag) {
  const info = await page.evaluate(function () {
    const app = document.querySelector("[data-view]");
    function vis(sel) { const e = document.querySelector(sel); if (!e) return "missing"; const r = e.getBoundingClientRect(); return (r.width > 0 && r.height > 0) ? "visible" : "hidden"; }
    return { hash: location.hash, appView: app ? app.getAttribute("data-view") : null,
      viewCua: vis("#view-cua"), viewChat: vis("#view-chat"), viewKb: vis("#view-kb"),
      cuaStats: vis("#cua-stats"), hasCUA: typeof window.CUA, activeTabs: [...document.querySelectorAll(".view-tab.active, .view-tab[aria-current]")].map(function (t) { return t.getAttribute("data-view"); }) };
  });
  console.log(tag, JSON.stringify(info));
}
await page.goto(BASE + "/#/cua", { waitUntil: "load", timeout: 20000 });
await page.waitForTimeout(1200);
await snap("cold-#/cua     :");
await page.goto(BASE + "/#/kb", { waitUntil: "load", timeout: 20000 });
await page.waitForTimeout(1200);
await snap("cold-#/kb      :");
await page.goto(BASE + "/#/cua", { waitUntil: "load", timeout: 20000 });
await page.waitForTimeout(600);
await page.click(".view-tab[data-view=cua]");
await page.waitForTimeout(1200);
await snap("after-click-cua:");
console.log("pageerrors:", JSON.stringify(errs.slice(0, 4)));
await browser.close();
