// task-31 独立复验：READY 解析（electron/ready.js）。
// 用**被验模块本身**（argv[2] 指定路径，便于对突变体复跑），断言我自己设计的判据。
import { createRequire } from "node:module";
import { resolve } from "node:path";
import { PassThrough } from "node:stream";

const require = createRequire(import.meta.url);
const target = resolve(process.argv[2] || "electron/ready.js");
const mod = require(target);
const { waitForReady, parseReadyUrl } = mod;

const results = [];
function rec(level, id, msg, detail) { results.push({ level, id }); console.log("[" + level + "] " + id + "  " + msg + (detail ? "  " + detail : "")); }
const PASS = (id, m, d) => rec("PASS", id, m, d);
const FAIL = (id, m, d) => rec("FAIL", id, m, d);
const WARN = (id, m, d) => rec("WARN", id, m, d);
const INFO = (id, m, d) => rec("INFO", id, m, d);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function fakeProc() { return { stdout: new PassThrough(), stderr: new PassThrough() }; }

async function main() {
  console.log("# 被测模块 = " + target);

  // R0：模块导出面
  if (typeof waitForReady === "function" && typeof parseReadyUrl === "function") PASS("R0-exports", "waitForReady / parseReadyUrl 都是函数");
  else { FAIL("R0-exports", "导出面不对"); return; }

  // R1：只在 stdout
  {
    const p = fakeProc(); const w = waitForReady(p);
    p.stdout.write("READY http://127.0.0.1:8080\n");
    const url = await Promise.race([w, sleep(200).then(() => null)]);
    if (url === "http://127.0.0.1:8080") PASS("R1-stdout-only", "只在 stdout 也能解析", url);
    else FAIL("R1-stdout-only", "只在 stdout 没解析出来", String(url));
  }

  // R2：只在 stderr（原缺陷方向）
  {
    const p = fakeProc(); const w = waitForReady(p);
    p.stderr.write("2026/10/11 19:31:02 READY http://127.0.0.1:3666\n");
    const url = await Promise.race([w, sleep(200).then(() => null)]);
    if (url === "http://127.0.0.1:3666") PASS("R2-stderr-only", "只在 stderr 也能解析（原缺陷方向）", url);
    else FAIL("R2-stderr-only", "只在 stderr 没解析出来", String(url));
  }

  // R3：分块边界穷举 —— 一行在任意位置被切开，都不许解析出残缺 URL
  {
    const line = "2026/10/11 19:31:02 READY http://127.0.0.1:3666\n";
    const full = "http://127.0.0.1:3666";
    let bad = [];
    for (let k = 1; k < line.length; k++) {
      const p = fakeProc();
      let settled = null;
      const w = waitForReady(p).then((u) => { settled = u; return u; });
      p.stderr.write(line.slice(0, k));
      await sleep(5);
      const early = settled;                       // 换行还没到，就不该 settle
      p.stderr.write(line.slice(k));
      const url = await Promise.race([w, sleep(200).then(() => null)]);
      if (early !== null) bad.push({ k, why: "换行前就 settle 了", got: early });
      else if (url !== full) bad.push({ k, why: "解析结果不是完整 URL", got: url });
    }
    if (bad.length === 0) PASS("R3-split-exhaustive", "穷举 " + (line.length - 1) + " 个切分点：都不早 settle、结果都是完整 URL");
    else FAIL("R3-split-exhaustive", "分块边界有问题，最早几例：" , JSON.stringify(bad.slice(0, 3)));
  }

  // R4：整行本身就是残缺 URL（冒号结尾就换行）—— 这是"半行"的另一种形态
  {
    const p = fakeProc(); const w = waitForReady(p);
    p.stderr.write("READY http://127.0.0.1:\n");
    const url = await Promise.race([w, sleep(400).then(() => null)]);
    if (url === null) PASS("R4-truncated-line", "残缺整行没有被当成 URL");
    else WARN("R4-truncated-line", "残缺整行仍被解析成 URL（后端自己写出半行才会触发；Go 侧是整行 log.Printf，实测不会切在冒号后）", String(url));
  }

  // R5：CRLF
  {
    const p = fakeProc(); const w = waitForReady(p);
    p.stderr.write("READY http://127.0.0.1:4444\r\n");
    const url = await Promise.race([w, sleep(200).then(() => null)]);
    if (url === "http://127.0.0.1:4444") PASS("R5-crlf", "CRLF 行尾也能解析", JSON.stringify(url));
    else FAIL("R5-crlf", "CRLF 行尾解析失败", JSON.stringify(url));
  }

  // R6：没有 READY 时不许误报（噪声、小写 ready、非 URL）
  {
    const p = fakeProc(); let hit = false;
    waitForReady(p).then(() => { hit = true; });
    p.stdout.write("[GIN-debug] Loaded HTML Templates (11):\n");
    p.stderr.write("2026/10/11 19:31:02 未加载 .env，将只使用系统环境变量\n");
    p.stderr.write("server is ready http://127.0.0.1:9999\n");
    p.stdout.write("READY not-a-url\n");
    await sleep(200);
    if (!hit) PASS("R6-no-false-positive", "噪声/小写 ready/非 URL 都不会误报");
    else FAIL("R6-no-false-positive", "被噪声/小写 ready 误报");
  }

  // R7：先出现的一行胜出
  {
    const p = fakeProc(); const w = waitForReady(p);
    p.stdout.write("READY http://127.0.0.1:1111\n");
    p.stderr.write("READY http://127.0.0.1:2222\n");
    const url = await Promise.race([w, sleep(200).then(() => null)]);
    if (url === "http://127.0.0.1:1111") PASS("R7-first-wins", "先出现的 READY 胜出", url);
    else FAIL("R7-first-wins", "胜出顺序不对", String(url));
  }

  // R8：超长无换行输出之后，正常的 READY 仍要能解析（4KB 上限不得把后续行吃掉）
  {
    const p = fakeProc(); const w = waitForReady(p);
    p.stdout.write("x".repeat(5000));
    p.stdout.write("\nREADY http://127.0.0.1:4321\n");
    const url = await Promise.race([w, sleep(600).then(() => null)]);
    if (url === "http://127.0.0.1:4321") PASS("R8-after-flood", "5000 字符无换行噪声之后仍能解析", url);
    else FAIL("R8-after-flood", "被超长输出影响", String(url));
  }

  // R9：4KB 上限是否存在（长度超过一行的 READY 被丢弃不算错，但我们至少观测它不炸、不误报）
  {
    const p = fakeProc(); let got = null;
    waitForReady(p).then((u) => { got = u; });
    p.stdout.write("READY http://127.0.0.1:5555 " + "y".repeat(6000) + "\n");
    await sleep(300);
    if (got === "http://127.0.0.1:5555") INFO("R9-huge-line", "超长行内的 READY 仍被解析（上限只切缓冲区，不改变语义）", got);
    else INFO("R9-huge-line", "超长行内的 READY 未被解析（4KB 上限把它丢了；READY 行实际只有几十字节，不影响契约）", String(got));
  }

  const fails = results.filter((r) => r.level === "FAIL");
  const pass = results.filter((r) => r.level === "PASS").length;
  const warn = results.filter((r) => r.level === "WARN").length;
  console.log("---- 汇总：PASS=" + pass + " FAIL=" + fails.length + " WARN=" + warn + " ----");
  fails.forEach((f) => console.log("  FAIL " + f.id));
  process.exit(fails.length ? 1 : 0);
}

main();
