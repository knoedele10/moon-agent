'use strict';

const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');

const root = path.resolve(__dirname, '../..');

function createGoEnv(projectRoot = root, sourceEnv = process.env) {
  const buildDir = path.join(projectRoot, 'build');
  fs.mkdirSync(buildDir, { recursive: true });
  // Keep downloaded modules and generated Go probes out of the main module.
  try {
    fs.writeFileSync(path.join(buildDir, 'go.mod'), 'module blog-my-build-artifacts\n', { flag: 'wx' });
  } catch (error) {
    if (error.code !== 'EEXIST') throw error;
  }

  const env = { ...sourceEnv };
  const defaults = {
    GOCACHE: path.join(buildDir, 'gocache'),
    GOMODCACHE: path.join(buildDir, 'gomodcache'),
    GOTMPDIR: path.join(buildDir, 'gotmp'),
    TEMP: path.join(buildDir, 'tmp'),
    TMP: path.join(buildDir, 'tmp'),
  };
  for (const [name, directory] of Object.entries(defaults)) {
    if (!env[name]) env[name] = directory;
    fs.mkdirSync(env[name], { recursive: true });
  }
  return env;
}

function runGo(args) {
  const result = spawnSync('go', args, { cwd: root, env: createGoEnv(), stdio: 'inherit' });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status || 1);
}

module.exports = { root, createGoEnv, runGo };
