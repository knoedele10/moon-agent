// task-31：用打包产物里的真实后端（resources/bin/blog-my.exe）的真实 stdout/stderr 喂给真实的 electron/ready.js。
import { createRequire } from "node:module";
import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

const require = createRequire(import.meta.url);
const { waitForReady } = require(resolve("electron/ready.js"));

const RES = resolve("dist/win-unpacked/resources");
const BIN = resolve(RES, "bin/blog-my.exe");
const envFile = process.argv[2] || ".env";

const vars = {};
for (const line of readFileSync(envFile, "utf8").split(/\r?\n/)) {
  const m = /^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/.exec(line);
  if (m) vars[m[1]] = m[2].replace(/^["\']|["\']$/g, "");
}
console.log("# 后端 = " + BIN + "  cwd = " + RES + "  .env 变量数 = " + Object.keys(vars).length);

const out = []; const err = [];
const child = spawn(BIN, [], { cwd: RES, env: { ...process.env, ...vars, APP_HOST: "127.0.0.1", APP_PORT: "0" }, stdio: ["ignore", "pipe", "pipe"] });
child.on("error", (e) => { console.log("[FAIL] spawn 失败 " + e.message); process.exit(1); });
child.stdout.on("data", (c) => out.push(c.toString()));
child.stderr.on("data", (c) => err.push(c.toString()));

function killTree() { try { spawn("taskkill", ["/PID", String(child.pid), "/T", "/F"], { stdio: "ignore" }); } catch (e) {} }
const withTimeout = (p, ms) => Promise.race([p, new Promise((r) => setTimeout(() => r("__TIMEOUT__"), ms))]);

let fails = 0;
const ts = Date.now();
const url = await withTimeout(waitForReady(child, { onStdout: (t) => out.push(t), onStderr: (t) => err.push(t) }), 45000);

if (typeof url === "string" && /^http:\/\/127\.0\.0\.1:\d+$/.test(url)) {
  console.log("[PASS] P1-shell-resolves  ready.js 从真实后端的真实两条流里解析出 " + url + "（" + (Date.now() - ts) + "ms）");
} else { fails++; console.log("[FAIL] P1-shell-resolves  没能解析出 URL，得到 " + JSON.stringify(url)); }

const eText = err.join("");
const oText = out.join("");
const readyOnErr = /READY\s+http/.test(eText);
const readyOnOut = /READY\s+http/.test(oText);
console.log("[INFO] P2-stream  READY 出现在 stderr=" + readyOnErr + " / stdout=" + readyOnOut + "；stdout 字节=" + oText.length + " stderr 字节=" + eText.length);
console.log("[INFO] P2-line    " + (eText.split(/\r?\n/).filter((l) => /READY/.test(l))[0] || oText.split(/\r?\n/).filter((l) => /READY/.test(l))[0] || "(none)").trim());

if (typeof url === "string" && url.startsWith("http")) {
  try {
    const res = await fetch(url + "/api/health");
    const body = await res.text();
    if (res.status === 200) console.log("[PASS] P3-health  解析出的 URL 上 /api/health = 200（body 前 50 字：" + body.slice(0, 50).replace(/\s+/g, " ") + "）");
    else { fails++; console.log("[FAIL] P3-health  /api/health = " + res.status); }
  } catch (e) { fails++; console.log("[FAIL] P3-health  请求失败：" + String(e).slice(0, 120)); }
}

killTree();
await new Promise((r) => setTimeout(r, 900));
console.log("---- 汇总：FAIL=" + fails + " ----");
process.exit(fails ? 1 : 0);
