  // 「READY <url>」的解析与等待（task-30）。
//
// 关键：Go 的 log 包默认写 **stderr**（main.go:50 的 log.Printf("READY …")），
// 所以只盯 stdout 的壳永远等不到 URL —— 后端明明起来了，窗口却永远不开。
// 这里把两条流都接进同一个探测器，任何一条出现 READY 都算数。
const READY_RE = /READY\s+(http:\/\/\S+)/;

// 注意（task-32 / verifier WARN）：本函数**不要求换行**。
// 直接拿它解析不完整的 chunk 时，\S+ 会把半行 'READY http://127.0.0.1:' 也匹配出来，
// 得到残缺 URL。外部**只应走 waitForReady**（它按行切分后再调用本函数）。
function parseReadyUrl(text) {
  const m = READY_RE.exec(String(text));
  return m ? m[1] : null;
}

// proc 需要 .stdout / .stderr 两个可读流；onStdout / onStderr 是可选转发回调（不吞日志）。
// 返回 Promise<string>：首个 READY 的 URL。两条流都没有 READY 就永不 settle（由调用方处理）。
function waitForReady(proc, handlers = {}) {
  const { onStdout, onStderr } = handlers;
  return new Promise((resolve) => {
    let done = false;
    // 管道会把输出切成任意 chunk，甚至把一行切开；只对「完整行」做匹配，
    // 否则半行 'READY http://127.0.0.1:' 也会被 \S+ 当成 URL 命中（会解析出残缺地址）。
    let pending = '';
    const probe = (text) => {
      if (done) return;
      pending += text;
      let idx = pending.indexOf('\n');
      while (!done && idx >= 0) {
        const line = pending.slice(0, idx);
        pending = pending.slice(idx + 1);
        const url = parseReadyUrl(line);
        if (url) {
          done = true;
          resolve(url);
        }
        idx = pending.indexOf('\n');
      }
      // 没有换行的超长输出不该把内存撑爆。
      if (pending.length > 4096) pending = pending.slice(-4096);
    };
    const attach = (stream, forward) => {
      if (!stream || typeof stream.on !== 'function') return;
      stream.on('data', (chunk) => {
        const text = chunk.toString();
        if (forward) forward(text);
        probe(text);
      });
    };
    attach(proc.stdout, onStdout);
    attach(proc.stderr, onStderr);
  });
}

module.exports = { READY_RE, parseReadyUrl, waitForReady };
