// 桌面壳的 .env 解析、模板与 agent 默认值（electron/main.js 的配套模块，独立出来是为了可单测）。
//
// 为什么需要这一层：打包态**绝不依赖仓库 .env**——配置来自 userData/.env，由 Electron 解析成
// 环境变量传给 Go 子进程。网页端能用的 AI 自主工具调用，必须在桌面壳这里也补上同样的默认值，
// 否则同一份后端代码在桌面应用里工具是关的（AGENT_TOOLS 缺失 → /api/chat 走旧路径）。
const fs = require('fs');
const path = require('path');

// 桌面版默认开启 AI 自主工具调用，与网页端 .env 的 AGENT_TOOLS=1 对齐。
const DEFAULT_AGENT_TOOLS = '1';
const DEFAULT_AGENT_MAX_ITERATIONS = '32';
const DEFAULT_AGENT_MAX_TOOL_CALLS = '96';

// 文件工具沙箱目录：放在用户配置目录下，随用户走，不依赖安装路径或仓库位置。
function workspaceDir(userDataDir) {
  return path.join(userDataDir, 'workspace');
}

// 技能目录：存放带脚本的技能（如 ppt-master），对模型**只读**。
// 与工作区分开，是为了让“模型能写的地方”和“模型只能读的地方”界限清楚。
function skillsDir(userDataDir) {
  return path.join(userDataDir, 'skills');
}

// resolveSkillsDir 决定用哪个技能目录：
//   1. 用户自己放的（userData/skills）——有内容才用它；旧版可能自动建过空目录；
//   2. 否则用**随应用打包**的那份（resources/skills）——这样像 ppt-master 这种
//      带几百个脚本的技能开箱可用；
//   3. 都没有就用 userData/skills（由主进程创建，用户可以继续往里放）。
function resolveSkillsDir(userDataDir, bundledRoot) {
  const userDir = skillsDir(userDataDir);
  try {
    if (fs.existsSync(userDir) && fs.readdirSync(userDir).length > 0) return userDir;
    if (bundledRoot) {
      const bundled = path.join(bundledRoot, 'skills');
      if (fs.existsSync(bundled)) return bundled;
    }
  } catch (_) { /* 探测失败就回落，不影响启动 */ }
  return userDir;
}

function hasValue(v) {
  return typeof v === 'string' && v.trim() !== '';
}

// 极简 .env 解析：KEY=VALUE，支持 # 注释与成对引号。
function parseEnv(text) {
  const vars = {};
  for (const raw of String(text).split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const i = line.indexOf('=');
    if (i <= 0) continue;
    const key = line.slice(0, i).trim();
    let value = line.slice(i + 1).trim();
    const quoted =
      (value.startsWith('"') && value.endsWith('"')) ||
      (value.startsWith("'") && value.endsWith("'"));
    if (quoted && value.length >= 2) value = value.slice(1, -1);
    vars[key] = value;
  }
  return vars;
}

// 为缺失的 AGENT_* 补上桌面默认值。
//
// 显式配置优先：用户把 AGENT_TOOLS 写成 0（或任何非空值）就照用，包括"主动关掉工具"。
// 旧版完整默认组合 12/24 升级到 32/96；其他自定义数值保留。不重写用户文件。
// 返回新对象，不修改入参。
function agentEnvDefaults(vars, userDataDir, bundledRoot) {
  const out = { ...(vars || {}) };
  if (String(out.AGENT_MAX_ITERATIONS).trim() === '12' && String(out.AGENT_MAX_TOOL_CALLS).trim() === '24') {
    out.AGENT_MAX_ITERATIONS = DEFAULT_AGENT_MAX_ITERATIONS;
    out.AGENT_MAX_TOOL_CALLS = DEFAULT_AGENT_MAX_TOOL_CALLS;
  }
  if (!hasValue(out.AGENT_TOOLS)) out.AGENT_TOOLS = DEFAULT_AGENT_TOOLS;
  if (!hasValue(out.AGENT_FS_ROOT)) out.AGENT_FS_ROOT = workspaceDir(userDataDir);
  if (!hasValue(out.AGENT_SKILLS_DIR)) out.AGENT_SKILLS_DIR = resolveSkillsDir(userDataDir, bundledRoot);
  if (!hasValue(out.AGENT_MAX_ITERATIONS)) out.AGENT_MAX_ITERATIONS = DEFAULT_AGENT_MAX_ITERATIONS;
  if (!hasValue(out.AGENT_MAX_TOOL_CALLS)) out.AGENT_MAX_TOOL_CALLS = DEFAULT_AGENT_MAX_TOOL_CALLS;
  if (!hasValue(out.AGENT_LIMIT_RECOVERY_ROUNDS)) out.AGENT_LIMIT_RECOVERY_ROUNDS = '2';
  return out;
}

// 首次启动生成的配置模板。secret 由调用方随机生成一次并固定；
// userDataDir 决定默认的 agent 文件沙箱位置。
function envTemplate({ secret, userDataDir }) {
  return [
    '# blog-my 桌面版配置（首次启动自动生成）',
    '# 本文件位于用户配置目录，不随安装包分发；请把真实值填在这里。',
    '',
    '# ---- 服务 ----',
    '# 桌面客户端由 Electron 传入 APP_HOST/APP_PORT，此项仅在不经 Electron 直启时生效。',
    'APP_PORT=8080',
    '# 用户 API Key 的 AES 加密密钥（已自动生成；改动会导致已存 key 无法解密）。',
    'APP_SECRET=' + secret,
    '',
    '# ---- MySQL（必填：后端启动时会连库，连不上会退出）----',
    'DB_HOST=127.0.0.1',
    'DB_PORT=3306',
    'DB_USER=root',
    'DB_PASSWORD=',
    'DB_NAME=blog_my',
    '',
    '# ---- 各厂商默认 Key（留空则前台让用户自己填）----',
    'DEEPSEEK_API_KEY=',
    'OPENAI_API_KEY=',
    'XAI_API_KEY=',
    'MOONSHOT_API_KEY=',
    'DASHSCOPE_API_KEY=',
    'ZHIPU_API_KEY=',
    'ANTHROPIC_API_KEY=',
    'GEMINI_API_KEY=',
    '# 文心是两段式凭证，格式 API_KEY:SECRET_KEY',
    'ERNIE_API_KEY=',
    '',
    '# ---- 本地 RAG（Ollama，可选）----',
    'OLLAMA_BASE_URL=http://127.0.0.1:11434',
    'KB_EMBED_MODEL=bge-m3:latest',
    'KB_VISION_MODEL=qwen2.5vl:7b',
    '',
    '# ---- AI 自主工具调用（agent）----',
    '# 总开关：1/true/on/yes 开启；改成 0 可关闭（关闭后 /api/chat 与旧行为完全一致）。',
    'AGENT_TOOLS=1',
    '# 文件工具沙箱根目录。默认在本机用户配置目录下，模型只能在此目录内读写。',
    'AGENT_FS_ROOT=' + workspaceDir(userDataDir),
    '# 每阶段模型 / 工具预算（0 或缺省时用内置默认 32 / 96）。',
    'AGENT_MAX_ITERATIONS=' + DEFAULT_AGENT_MAX_ITERATIONS,
    'AGENT_MAX_TOOL_CALLS=' + DEFAULT_AGENT_MAX_TOOL_CALLS,
    '# 自主模式达到阶段上限后自动续行的次数；0 关闭，最多 8。',
    'AGENT_LIMIT_RECOVERY_ROUNDS=2',
    '',
  ].join('\n');
}

module.exports = {
  parseEnv,
  envTemplate,
  agentEnvDefaults,
  workspaceDir,
  resolveSkillsDir,
  DEFAULT_AGENT_TOOLS,
  DEFAULT_AGENT_MAX_ITERATIONS,
  DEFAULT_AGENT_MAX_TOOL_CALLS,
};
