// 把 electron/ready.js 按名字变异后写到 %TEMP%\ready-mut\ready.js（不动仓库文件）。
import { readFileSync, writeFileSync, mkdirSync, copyFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

const name = process.argv[2];
const out = process.argv[3] || join(tmpdir(), "ready-mut");
mkdirSync(out, { recursive: true });
const src = readFileSync("electron/ready.js", "utf8");
copyFileSync("electron/ready.test.js", join(out, "ready.test.js"));

const MUTS = {
  clean: (s) => s,
  stdout_only: (s) => s.replace("attach(proc.stderr, onStderr);", "// 变异：不接 stderr（还原原缺陷）"),
  no_line_split: (s) => s.replace(/const probe = \(text\) => \{[\s\S]*?\n    \};/,
    "const probe = (text) => { if (done) return; const url = parseReadyUrl(String(text)); if (url) { done = true; resolve(url); } };"),
  no_cap: (s) => s.replace("if (pending.length > 4096) pending = pending.slice(-4096);", "// 变异：去掉 4KB 上限"),
};

if (!MUTS[name]) { console.error("未知变异：" + name); process.exit(2); }
const next = MUTS[name](src);
if (name !== "clean" && next === src) { console.error("变异 " + name + " 没有生效（锚点没匹配上）"); process.exit(3); }
writeFileSync(join(out, "ready.js"), next, "utf8");
console.log(join(out, "ready.js"));
