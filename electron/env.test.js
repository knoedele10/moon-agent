// 回归：桌面壳必须和网页端一样把 AI 自主工具调用开起来。
//
// 这里钉住的是「打包态」的配置路径：userData/.env（或首次生成的模板）经 parseEnv 解析后，
// 必须得到 AGENT_TOOLS=1 与一个位于用户配置目录下的 AGENT_FS_ROOT。少了这一步，
// 后端代码一行没变，但桌面应用里工具是关的——正是本次要修的缺陷。
const test = require('node:test');
const assert = require('node:assert');
const path = require('node:path');
const fs = require('node:fs');
const os = require('node:os');
const {
  parseEnv,
  envTemplate,
  agentEnvDefaults,
  workspaceDir,
  resolveSkillsDir,
  DEFAULT_AGENT_TOOLS,
} = require('./env');

const USER_DATA = path.join('C:', 'Users', 'tester', 'AppData', 'Roaming', 'blog-my');

test('首次生成的模板里带 AGENT_* 默认值，且沙箱目录在用户配置目录下', () => {
  const text = envTemplate({ secret: 'fixed-secret', userDataDir: USER_DATA });
  const vars = parseEnv(text);

  assert.strictEqual(vars.AGENT_TOOLS, DEFAULT_AGENT_TOOLS);
  assert.strictEqual(vars.AGENT_FS_ROOT, workspaceDir(USER_DATA));
  assert.strictEqual(vars.AGENT_MAX_ITERATIONS, '32');
  assert.strictEqual(vars.AGENT_MAX_TOOL_CALLS, '96');
  // 沙箱必须落在用户目录内，不能是仓库路径或安装路径。
  assert.ok(vars.AGENT_FS_ROOT.startsWith(USER_DATA));
});

test('模板仍保留原有键（APP_SECRET / MySQL / Ollama），secret 原样写入', () => {
  const text = envTemplate({ secret: 'fixed-secret', userDataDir: USER_DATA });
  const vars = parseEnv(text);

  assert.strictEqual(vars.APP_SECRET, 'fixed-secret');
  assert.strictEqual(vars.DB_HOST, '127.0.0.1');
  assert.strictEqual(vars.DB_NAME, 'blog_my');
  assert.strictEqual(vars.OLLAMA_BASE_URL, 'http://127.0.0.1:11434');
});

test('老配置缺 AGENT_* 时，启动前补上桌面默认值（平滑升级）', () => {
  const legacy = parseEnv('APP_SECRET=x\nDB_HOST=127.0.0.1\n');
  const merged = agentEnvDefaults(legacy, USER_DATA);

  assert.strictEqual(merged.AGENT_TOOLS, '1');
  assert.strictEqual(merged.AGENT_FS_ROOT, workspaceDir(USER_DATA));
  assert.strictEqual(merged.APP_SECRET, 'x');
});

test('显式配置优先：AGENT_TOOLS=0 与自定义根目录不被覆盖', () => {
  const merged = agentEnvDefaults(
    { AGENT_TOOLS: '0', AGENT_FS_ROOT: 'D:\\my-sandbox', AGENT_MAX_ITERATIONS: '30' },
    USER_DATA
  );

  assert.strictEqual(merged.AGENT_TOOLS, '0');
  assert.strictEqual(merged.AGENT_FS_ROOT, 'D:\\my-sandbox');
  assert.strictEqual(merged.AGENT_MAX_ITERATIONS, '30');
  assert.strictEqual(merged.AGENT_MAX_TOOL_CALLS, '96');
});

test('空值按缺失处理，且不修改传入对象', () => {
  const input = { AGENT_TOOLS: '   ', AGENT_FS_ROOT: '' };
  const merged = agentEnvDefaults(input, USER_DATA);

  assert.strictEqual(merged.AGENT_TOOLS, '1');
  assert.strictEqual(merged.AGENT_FS_ROOT, workspaceDir(USER_DATA));
  assert.deepStrictEqual(input, { AGENT_TOOLS: '   ', AGENT_FS_ROOT: '' });
});

test('旧版默认预算升级，自定义预算和显式关闭续行保留', () => {
  const upgraded = agentEnvDefaults({ AGENT_MAX_ITERATIONS: '12', AGENT_MAX_TOOL_CALLS: '24' }, USER_DATA);
  assert.strictEqual(upgraded.AGENT_MAX_ITERATIONS, '32');
  assert.strictEqual(upgraded.AGENT_MAX_TOOL_CALLS, '96');
  assert.strictEqual(upgraded.AGENT_LIMIT_RECOVERY_ROUNDS, '2');
  const custom = agentEnvDefaults({ AGENT_MAX_ITERATIONS: '18', AGENT_MAX_TOOL_CALLS: '40', AGENT_LIMIT_RECOVERY_ROUNDS: '0' }, USER_DATA);
  assert.strictEqual(custom.AGENT_MAX_ITERATIONS, '18');
  assert.strictEqual(custom.AGENT_MAX_TOOL_CALLS, '40');
  assert.strictEqual(custom.AGENT_LIMIT_RECOVERY_ROUNDS, '0');
});

test('优先读取用户技能目录，否则读取打包内置技能目录', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'blog-my-skills-'));
  try {
    const userData = path.join(root, 'user');
    const bundle = path.join(root, 'resources');
    fs.mkdirSync(path.join(bundle, 'skills'), { recursive: true });
    assert.strictEqual(resolveSkillsDir(userData, bundle), path.join(bundle, 'skills'));

    fs.mkdirSync(path.join(userData, 'skills'), { recursive: true });
    assert.strictEqual(resolveSkillsDir(userData, bundle), path.join(bundle, 'skills'));

    fs.mkdirSync(path.join(userData, 'skills', 'my-skill'));
    assert.strictEqual(resolveSkillsDir(userData, bundle), path.join(userData, 'skills'));
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
