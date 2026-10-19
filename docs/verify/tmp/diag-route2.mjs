import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
const pw = require("C:/Users/woshiss/AppData/Roaming/npm/node_modules/openclaw/node_modules/playwright-core");
const BASE = "http://127.0.0.1:8300";
const browser = await pw.chromium.launch({ headless: true, executablePath: "C:/Users/woshiss/AppData/Local/ms-playwright/chromium-1223/chrome-win64/chrome.exe", args: ["--no-sandbox"] });
const ctx = await browser.newContext();
const u = "fx" + Date.now().toString(36);
await ctx.request.post(BASE + "/register", { form: { username: u, password: "Verify-2026-abc" } });
await ctx.request.post(BASE + "/login", { form: { username: u, password: "Verify-2026-abc" } });
const page = await ctx.newPage();
await page.goto(BASE + "/#/cua", { waitUntil: "load" });
await page.waitForTimeout(1500);
const out = await page.evaluate(function () {
  const app = document.querySelector(".app");
  const el = document.getElementById("view-cua");
  const kb = document.getElementById("view-kb");
  function chain(e) { const acc = []; let n = e; while (n && n.nodeType === 1) { const cs = getComputedStyle(n); if (cs.display === "none") acc.push((n.id ? "#" + n.id : n.tagName.toLowerCase() + "." + (n.className || "")) + " display=none" + (n.hasAttribute("hidden") ? " [hidden]" : "")); n = n.parentElement; if (acc.length > 3) break; } return acc; }
  function info(e) { const cs = getComputedStyle(e); const r = e.getBoundingClientRect(); return { display: cs.display, visibility: cs.visibility, rect: Math.round(r.width) + "x" + Math.round(r.height), hiddenAttr: e.hasAttribute("hidden"), inApp: !!(e.closest && e.closest(".app")), parentId: e.parentElement ? e.parentElement.id : null, parentClass: e.parentElement ? e.parentElement.className : null, blockedBy: chain(e) }; }
  const sheets = [];
  for (const sh of document.styleSheets) { try { for (const r of sh.cssRules) { if (r.selectorText && r.selectorText.indexOf("view-cua") >= 0) sheets.push(r.selectorText + " { " + r.style.cssText + " }"); } } catch (e) {} }
  return { appView: app ? app.getAttribute("data-view") : null, cua: info(el), kb: info(kb), matchingRules: sheets };
});
console.log(JSON.stringify(out, null, 1));
await page.click(".view-tab[data-view=cua]");
await page.waitForTimeout(800);
const out2 = await page.evaluate(function () { const e = document.getElementById("view-cua"); const r = e.getBoundingClientRect(); const st = document.getElementById("cua-stats"); const r2 = st.getBoundingClientRect(); return { cua: Math.round(r.width) + "x" + Math.round(r.height), stats: Math.round(r2.width) + "x" + Math.round(r2.height), statsDisplay: getComputedStyle(st).display }; });
console.log("after click:", JSON.stringify(out2));
await page.screenshot({ path: "docs/verify/tmp/cua-view.png", fullPage: false });
await browser.close();
