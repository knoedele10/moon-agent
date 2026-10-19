// 回归（task-30）：断言**壳那一侧**能从 READY 行解析出 URL。
// 只断言「日志里有 READY」是不够的——那正是那个致命缺陷逃过验收的原因。
//
// task-32：所有等待都套超时。waitForReady 在「两条流都没有 READY」时永不 settle，
// 变异回原缺陷（不接 stderr）时若不套超时，套件会**挂住**而不是**变红**——
// 挂起在 CI 里不是失败信号。这里用 Promise.race 让同类回归在数秒内失败。
const test = require('node:test');
const assert = require('node:assert');
const { PassThrough } = require('node:stream');
const { parseReadyUrl, waitForReady } = require('./ready');

// 1s 足够：正常用例是纯内存流，毫秒级返回；超时只用于把「永不 settle」变成「变红」。
const TIMEOUT_MS = 1000;

// 超时即断言失败：错误信息直接说明「promise 未 settle = 原缺陷的表现」。
function withTimeout(promise, label, ms = TIMEOUT_MS) {
  let timer;
  const guard = new Promise((_, reject) => {
    timer = setTimeout(() => {
      reject(new Error('超时 ' + ms + 'ms 仍未解析出 URL：' + label + '（promise 未 settle，这正是原缺陷的表现）'));
    }, ms);
  });
  return Promise.race([promise, guard]).finally(() => clearTimeout(timer));
}

function fakeProc() {
  return { stdout: new PassThrough(), stderr: new PassThrough() };
}

test('READY 只在 stderr 时也要能解析（Go log 默认写 stderr）', async () => {
  const proc = fakeProc();
  const p = waitForReady(proc);
  proc.stderr.write('2026/10/11 19:31:02 READY http://127.0.0.1:3666\n');
  assert.strictEqual(await withTimeout(p, 'READY 在 stderr'), 'http://127.0.0.1:3666');
});

test('READY 只在 stdout 时也要能解析', async () => {
  const proc = fakeProc();
  const p = waitForReady(proc);
  proc.stdout.write('READY http://127.0.0.1:8080\n');
  assert.strictEqual(await withTimeout(p, 'READY 在 stdout'), 'http://127.0.0.1:8080');
});

test('一行被管道切成两个 chunk 也能解析出完整 URL', async () => {
  const proc = fakeProc();
  const p = waitForReady(proc);
  proc.stderr.write('2026/10/11 19:31:02 READY http://127.0.0.1:');
  proc.stderr.write('3666\n');
  assert.strictEqual(await withTimeout(p, '切分 chunk'), 'http://127.0.0.1:3666');
});

test('两条流都没有 READY 时不误报（GIN 噪音 / 未加载 .env 不算）', async () => {
  const proc = fakeProc();
  let hit = false;
  const p = waitForReady(proc).then(() => { hit = true; });
  proc.stdout.write('[GIN-debug] Loaded HTML Templates (11):\n');
  proc.stderr.write('2026/10/11 19:31:02 未加载 .env，将只使用系统环境变量\n');
  await new Promise((r) => setTimeout(r, 80));
  assert.strictEqual(hit, false, '不该被非 READY 输出唤醒');
  assert.strictEqual(parseReadyUrl('nothing here'), null);
  await assert.rejects(withTimeout(p, '没有 READY', 300), /超时/);
});

test('转发回调两条流都收得到（不吞日志）', async () => {
  const proc = fakeProc();
  const seen = { out: '', err: '' };
  const p = waitForReady(proc, {
    onStdout: (t) => { seen.out += t; },
    onStderr: (t) => { seen.err += t; },
  });
  proc.stdout.write('gin noise\n');
  proc.stderr.write('READY http://127.0.0.1:3666\n');
  await withTimeout(p, '转发回调');
  assert.match(seen.out, /gin noise/);
  assert.match(seen.err, /READY/);
});

test('自检：没有 READY 时超时守卫必须在数秒内判失败（不许挂住）', async () => {
  const proc = fakeProc();
  const started = Date.now();
  await assert.rejects(withTimeout(waitForReady(proc), '永不 settle', 300), /超时 300ms/);
  assert.ok(Date.now() - started < 3000, '超时守卫本身不该拖时间');
});
