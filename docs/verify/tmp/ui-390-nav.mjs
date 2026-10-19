import { createRequire } from "node:module";
const require2 = createRequire(import.meta.url);
const mod = require2("C:/Users/woshiss/AppData/Roaming/npm/node_modules/openclaw/node_modules/playwright-core");
const exe = "C:/Users/woshiss/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe";
const BASE = "http://127.0.0.1:10722";
const b = await mod.chromium.launch({ headless: true, executablePath: exe, args: ["--no-sandbox"] });
const ctx = await b.newContext({ viewport: { width: 390, height: 844 } });
const u = "diagnav_" + Date.now().toString(36);
await ctx.request.post(BASE + "/register", { form: { username: u, password: "Verify-" + u + "-2026" } });
await ctx.request.post(BASE + "/login", { form: { username: u, password: "Verify-" + u + "-2026" } });
const page = await ctx.newPage();
await page.goto(BASE + "/", { waitUntil: "load" });
await page.waitForSelector("#input", { timeout: 20000 });
await page.waitForTimeout(1500);
const info = await page.evaluate(() => {
  const vis = [];
  const nodes = document.querySelectorAll("button, a, [role=button]");
  for (const n of nodes) {
    const r = n.getBoundingClientRect();
    const cs = getComputedStyle(n);
    if (r.width > 0 && r.height > 0 && cs.visibility !== "hidden" && cs.display !== "none" && r.x + r.width > 0 && r.x < window.innerWidth) {
      vis.push({ tag: n.tagName, id: n.id, cls: (n.className && String(n.className).slice(0, 40)) || "", label: (n.getAttribute("aria-label") || n.textContent || "").trim().slice(0, 24), x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width) });
    }
  }
  const sidebar = document.querySelector(".sidebar");
  const nav = document.querySelector(".view-nav");
  function box(el) { if (!el) return null; const r = el.getBoundingClientRect(); const cs = getComputedStyle(el); return { x: Math.round(r.x), w: Math.round(r.width), transform: cs.transform, position: cs.position, left: cs.left, overflowX: cs.overflowX, zIndex: cs.zIndex }; }
  return { vw: window.innerWidth, visibleControls: vis, sidebar: box(sidebar), nav: box(nav), ariaExpanded: Array.prototype.slice.call(document.querySelectorAll("[aria-expanded]")).map((e) => ({ id: e.id, cls: String(e.className).slice(0, 30), v: e.getAttribute("aria-expanded"), visible: !!e.offsetParent })) };
});
console.log(JSON.stringify(info, null, 1));
await b.close();
