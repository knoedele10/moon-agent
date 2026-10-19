// 启动 Electron 的 launcher。
// 需要显式清掉 ELECTRON_RUN_AS_NODE：用户 shell 里可能全局设了它（用于其他场景），
// 留着会让 Electron 退化成纯 Node，require('electron') 只返回路径字符串。
const { spawn } = require('child_process');
const path = require('path');
const electronBin = require('electron'); // 在 Node 下 require('electron') 返回二进制路径

const env = { ...process.env };
delete env.ELECTRON_RUN_AS_NODE;

const child = spawn(electronBin, ['.'], {
  cwd: path.resolve(__dirname, '..'),
  env,
  stdio: 'inherit',
  windowsHide: false,
});
child.on('exit', (code) => process.exit(code ?? 0));
