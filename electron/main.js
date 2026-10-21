// Electron 主进程：启动 Go 后端子进程、解析 READY 行拿到实际地址、加载到 BrowserWindow。
// 退出时负责清理子进程（Windows 用 taskkill /T 杀进程树）。
//
// 路径与配置（task-23）：
//   projectRoot 打包态 = process.resourcesPath（extraResources 放的 bin/ 与 templates/），
//   开发态 = 仓库根。配置来源优先 userData/.env（安装态绝不依赖仓库 .env），缺失则生成
//   带注释的模板并继续启动；解析出的键值以环境变量传给 Go 子进程。
const { app, BrowserWindow, shell, dialog, ipcMain, clipboard } = require('electron');
const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { waitForReady } = require('./ready');
const { parseEnv, envTemplate, agentEnvDefaults } = require('./env');

// 打包态：electron-builder 的 extraResources 把 bin/ 与 templates/ 放在 resources/ 下
// （app.asar 里没有 bin/），所以 projectRoot 要跟着切；开发态仍是仓库根。
const projectRoot = app.isPackaged ? process.resourcesPath : path.resolve(__dirname, '..');
const binName = process.platform === 'win32' ? 'blog-my.exe' : 'blog-my';
const binPath = path.join(projectRoot, 'bin', binName);

// ---- .env 配置来源 ----
// 安装态绝不依赖仓库 .env（也不能把用户密钥打进安装包）：优先 userData/.env，
// 不存在就生成一份带注释的模板并继续启动（模型 key 留空 -> 前台正常降级）。
// 开发态保持原样：仓库根 .env 优先，方便本地开发。
function userEnvPath() {
  return path.join(app.getPath('userData'), '.env');
}


// 返回 { file, vars, generated }。generated=true 表示这次刚写了模板（用于给出更明确的报错提示）。
function resolveEnv() {
  const userEnv = userEnvPath();
  const devEnv = path.join(projectRoot, '.env');
  // 开发态：仓库根 .env 优先，否则会被安装态写在 userData 的配置盖住（改了不生效，很难查）。
  if (!app.isPackaged && fs.existsSync(devEnv)) {
    return { file: devEnv, vars: parseEnv(fs.readFileSync(devEnv, 'utf8')), generated: false };
  }
  if (fs.existsSync(userEnv)) {
    return { file: userEnv, vars: parseEnv(fs.readFileSync(userEnv, 'utf8')), generated: false };
  }
  // 安装态首次启动：生成带注释的模板并继续启动（模型 key 为空 -> 前台正常降级）。
  fs.mkdirSync(path.dirname(userEnv), { recursive: true });
  // APP_SECRET 必须非空，否则 Go 侧拒绝启动（main.go）；随机生成一次并固定下来，
  // 之后改动会导致已保存的用户 API Key 无法解密。
  const secret = crypto.randomBytes(32).toString('hex');
  fs.writeFileSync(userEnv, envTemplate({ secret, userDataDir: app.getPath('userData') }), 'utf8');
  console.log('[electron] 首次启动：已生成默认配置', userEnv);
  return { file: userEnv, vars: parseEnv(fs.readFileSync(userEnv, 'utf8')), generated: true };
}

let serverProc = null;
let mainWindow = null;
let serverUrl = null;
let quitting = false;
let publicTunnelProc = null;
let publicTunnelUrl = '';
let publicTunnelStatus = 'stopped';
let publicTunnelError = '';
let publicTunnelStartPromise = null;

function publicAccessState() {
  return { status: publicTunnelStatus, url: publicTunnelUrl, error: publicTunnelError };
}

function publishPublicAccessState() {
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.send('public-access:state', publicAccessState());
  }
}

function stopPublicAccess() {
  if (publicTunnelProc) {
    const proc = publicTunnelProc;
    publicTunnelProc = null;
    try { proc.kill(); } catch (_) { /* process may already have exited */ }
  }
  publicTunnelUrl = '';
  publicTunnelStatus = 'stopped';
  publicTunnelError = '';
  publishPublicAccessState();
}

function startPublicAccess() {
  if (publicTunnelStatus === 'connected' && publicTunnelUrl) return Promise.resolve(publicAccessState());
  if (publicTunnelStartPromise) return publicTunnelStartPromise;
  if (!serverUrl) return Promise.reject(new Error('本地服务还没有启动'));

  let localPort;
  try {
    const local = new URL(serverUrl);
    if (!['127.0.0.1', 'localhost'].includes(local.hostname)) {
      return Promise.reject(new Error('本地服务地址无效'));
    }
    localPort = local.port;
  } catch (_) {
    return Promise.reject(new Error('无法读取本地服务端口'));
  }
  if (!localPort) return Promise.reject(new Error('无法读取本地服务端口'));

  publicTunnelStatus = 'connecting';
  publicTunnelUrl = '';
  publicTunnelError = '';
  publishPublicAccessState();

  publicTunnelStartPromise = new Promise((resolve, reject) => {
    let settled = false;
    let output = '';
    let lineBuffer = '';
    let timer;
    let proc;
    const fail = (message) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      publicTunnelProc = null;
      publicTunnelStatus = 'error';
      publicTunnelError = message;
      publishPublicAccessState();
      reject(new Error(message));
    };
    const readOutput = (chunk) => {
      output += chunk.toString();
      lineBuffer += chunk.toString();
      const lines = lineBuffer.split(/\r?\n/);
      lineBuffer = lines.pop() || '';
      for (const line of lines) {
        const match = line.match(/tunneled with .*?(https:\/\/[^\s]+)/i);
        if (!match || settled) continue;
        settled = true;
        clearTimeout(timer);
        publicTunnelUrl = match[1].replace(/[),.]+$/, '');
        publicTunnelStatus = 'connected';
        publicTunnelError = '';
        publishPublicAccessState();
        resolve(publicAccessState());
      }
    };

    try {
      proc = spawn('ssh', [
        '-T',
        '-o', 'StrictHostKeyChecking=accept-new',
        '-o', 'BatchMode=yes',
        '-o', 'ServerAliveInterval=60',
        '-o', 'ServerAliveCountMax=3',
        '-o', 'ExitOnForwardFailure=yes',
        '-R', `80:127.0.0.1:${localPort}`,
        'nokey@localhost.run',
      ], { stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
    } catch (err) {
      fail(`启动 SSH 失败：${err.message}`);
      return;
    }
    publicTunnelProc = proc;
    proc.stdout.on('data', readOutput);
    proc.stderr.on('data', readOutput);
    proc.on('error', (err) => fail(`找不到系统 SSH（OpenSSH）：${err.message}`));
    proc.on('exit', (code) => {
      if (publicTunnelProc === proc) publicTunnelProc = null;
      if (!settled) {
        const detail = output.trim().split(/\r?\n/).filter(Boolean).slice(-1)[0];
        fail(detail || `公网隧道启动失败（退出码 ${code}）`);
      } else if (publicTunnelStatus === 'connected') {
        publicTunnelUrl = '';
        publicTunnelStatus = 'error';
        publicTunnelError = `公网隧道已断开（退出码 ${code}）`;
        publishPublicAccessState();
      }
    });
    timer = setTimeout(() => fail('连接公网隧道超时，请检查网络后重试'), 45000);
  }).finally(() => { publicTunnelStartPromise = null; });

  return publicTunnelStartPromise;
}

function startServer() {
  return new Promise((resolve, reject) => {
    if (!fs.existsSync(binPath)) {
      reject(new Error(`未找到后端二进制: ${binPath}\n请先运行 npm run build:go`));
      return;
    }
    // .env 不再要求放在仓库根：打包态从 userData 读，缺失则自动生成模板后继续启动。
    const cfg = resolveEnv();
    // 桌面默认值：老配置里缺 AGENT_* 时在这里补齐，保证桌面与网页行为一致
    // （模板已含这几项，这一步主要照顾升级前生成的 userData/.env）。
    const vars = agentEnvDefaults(cfg.vars, app.getPath('userData'), projectRoot);
    // 沙箱根目录必须真实存在：write 要在该目录下创建文件，read 也要能列出内容。
    if (vars.AGENT_FS_ROOT) {
      try {
        fs.mkdirSync(vars.AGENT_FS_ROOT, { recursive: true });
      } catch (err) {
        console.error('[electron] 创建 agent 沙箱目录失败:', vars.AGENT_FS_ROOT, err.message);
      }
    }
    // 技能目录（只读）：用户可以把自己的技能文件夹放进去，模型能读、能跑脚本，但不能改。
    if (vars.AGENT_SKILLS_DIR) {
      try {
        fs.mkdirSync(vars.AGENT_SKILLS_DIR, { recursive: true });
      } catch (err) {
        console.error('[electron] 创建技能目录失败:', vars.AGENT_SKILLS_DIR, err.message);
      }
    }
    console.log('[electron] 配置来源:', cfg.file, cfg.generated ? '(首次启动，已生成模板)' : '');
    console.log(
      '[electron] agent 工具:',
      vars.AGENT_TOOLS === '0' ? '关闭' : '开启',
      '| 沙箱:',
      vars.AGENT_FS_ROOT
    );

    // 配置用子进程环境变量传入：Go 侧 godotenv 只从 CWD 找 .env，
    // 而打包态的 CWD 必须是 resources/（templates/ 与 bin/ 在那里）。
    // APP_PORT=0 让 OS 挑一个空闲端口，避免和用户本机其他服务冲突。
    const env = {
      ...process.env,
      ...vars,
      APP_HOST: '127.0.0.1',
      APP_PORT: '0',
    };

    serverProc = spawn(binPath, [], {
      cwd: projectRoot, // 让 Go 找到 .env 和 templates/
      env,
      stdio: ['ignore', 'pipe', 'pipe'],
      windowsHide: true,
    });

    let resolved = false;
    // READY 走哪条流由 Go 的 log 决定：默认是 **stderr**（main.go:50 的 log.Printf）。
    // 所以两条流都要探测：只挂 stdout 会让这个 promise 永不 resolve ——
    // 后端明明起来了，壳却永远拿不到 URL、窗口永远不开（task-30 的致命缺陷）。
    waitForReady(serverProc, {
      onStdout: (text) => process.stdout.write(`[go] ${text}`),
      onStderr: (text) => process.stderr.write(`[go-err] ${text}`),
    }).then((url) => {
      if (resolved) return;
      resolved = true;
      serverUrl = url;
      resolve(serverUrl);
    });
    serverProc.on('error', (err) => {
      if (!resolved) reject(err);
    });
    serverProc.on('exit', (code, signal) => {
      serverProc = null;
      if (!resolved) {
        const hint = cfg.generated
          ? '\n首次启动已生成默认配置：' + cfg.file +
            '\n请填写 MySQL 连接信息（DB_HOST/DB_PORT/DB_USER/DB_PASSWORD/DB_NAME）后重试。'
          : '';
        reject(new Error(`后端进程提前退出 code=${code} signal=${signal}${hint}`));
      } else if (!quitting) {
        // 后端崩了：直接退出应用，让用户重启。
        dialog.showErrorBox('后端已退出', `Go 进程意外退出 (code=${code})，应用将关闭。`);
        app.quit();
      }
    });
  });
}

function stopServer() {
  if (!serverProc) return;
  const proc = serverProc;
  serverProc = null;
  try {
    if (process.platform === 'win32') {
      // Windows 上 child.kill() 不能杀进程树，使用 taskkill 强制清理。
      spawn('taskkill', ['/pid', String(proc.pid), '/T', '/F'], { windowsHide: true });
    } else {
      proc.kill('SIGTERM');
    }
  } catch (_) { /* 进程可能已退出 */ }
}

async function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1280,
    height: 800,
    minWidth: 960,
    minHeight: 600,
    autoHideMenuBar: true,
    backgroundColor: '#0f1115',
    title: '多模型对话',
    show: false, // 等 ready-to-show 再显示，避免白屏闪烁
    center: true,
    webPreferences: {
      contextIsolation: true,
      preload: path.join(__dirname, 'preload.js'),
      nodeIntegration: false,
      sandbox: true,
      spellcheck: false,
    },
  });

  mainWindow.once('ready-to-show', () => {
    mainWindow.show();
    mainWindow.focus();
    console.log('[electron] window shown, bounds =', mainWindow.getBounds());
  });

  mainWindow.webContents.on('did-fail-load', (_e, code, desc, url) => {
    console.error('[electron] did-fail-load', code, desc, url);
  });
  mainWindow.webContents.on('render-process-gone', (_e, details) => {
    console.error('[electron] render-process-gone', details);
  });

  // 应用内只允许访问本地后端；其他链接一律丢给系统浏览器。
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    if (serverUrl && url.startsWith(serverUrl)) return { action: 'allow' };
    shell.openExternal(url);
    return { action: 'deny' };
  });
  mainWindow.webContents.on('will-navigate', (event, url) => {
    if (serverUrl && !url.startsWith(serverUrl)) {
      event.preventDefault();
      shell.openExternal(url);
    }
  });

  mainWindow.on('closed', () => { mainWindow = null; });
  console.log('[electron] loading', serverUrl);
  await mainWindow.loadURL(serverUrl);
  console.log('[electron] loadURL resolved');
}

// 页面请求「选一个文件夹」：用系统原生目录选择框，返回绝对路径（取消则 null）。
// 只做选择，不落盘、不改配置——保存工作区仍然走 /api/agent/settings。
ipcMain.handle('pick-directory', async () => {
  if (!mainWindow) return null;
  const res = await dialog.showOpenDialog(mainWindow, {
    title: '选择 AI 的工作目录',
    properties: ['openDirectory', 'createDirectory'],
  });
  if (res.canceled || !res.filePaths || res.filePaths.length === 0) return null;
  return res.filePaths[0];
});

ipcMain.handle('public-access:get-state', () => publicAccessState());
ipcMain.handle('public-access:start', () => startPublicAccess());
ipcMain.handle('public-access:stop', () => {
  stopPublicAccess();
  return publicAccessState();
});
ipcMain.handle('public-access:copy', () => {
  if (!publicTunnelUrl) return false;
  clipboard.writeText(publicTunnelUrl);
  return true;
});

// 单实例：避免双击两次启两个 Go 进程占用同一份 MySQL 连接。
const gotLock = app.requestSingleInstanceLock();
if (!gotLock) {
  app.quit();
} else {
  app.on('second-instance', () => {
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore();
      mainWindow.focus();
    }
  });

  app.whenReady().then(async () => {
    try {
      await startServer();
      await createWindow();
    } catch (err) {
      console.error(err);
      dialog.showErrorBox('启动失败', String(err.message || err));
      app.quit();
    }
  });

  // 这是后端进程的壳，关窗即退出（包括 macOS）。
  app.on('window-all-closed', () => app.quit());
}

// 关闭时给渲染进程留一点时间，把「关闭补齐记忆」的 beacon 发出去，再杀后端。
// 不给这个宽限期的话，taskkill /F 会抢在请求到达之前把 Go 进程干掉，补齐就永远不生效。
let quitFlushTimer = null;
app.on('before-quit', () => {
  quitting = true;
  stopPublicAccess();
  if (quitFlushTimer) return; // before-quit 可能触发多次
  quitFlushTimer = setTimeout(stopServer, 1500);
});
process.on('exit', () => {
  if (publicTunnelProc) {
    try { publicTunnelProc.kill(); } catch (_) { /* process may already have exited */ }
  }
  stopServer();
});
