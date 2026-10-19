'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const path = require('path');
const { root, createGoEnv } = require('./go-runtime.cjs');

function temporaryProject(t) {
  const buildDir = path.join(root, 'build');
  fs.mkdirSync(buildDir, { recursive: true });
  const directory = fs.mkdtempSync(path.join(buildDir, 'tooling-test-'));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  return directory;
}

test('a fresh checkout gets isolated writable Go caches without changing process settings', t => {
  const directory = temporaryProject(t);
  const source = { PATH: process.env.PATH };
  const env = createGoEnv(directory, source);
  assert.deepEqual(source, { PATH: process.env.PATH });
  assert.match(fs.readFileSync(path.join(directory, 'build/go.mod'), 'utf8'), /^module blog-my-build-artifacts/m);
  for (const key of ['GOCACHE', 'GOMODCACHE', 'GOTMPDIR', 'TEMP', 'TMP']) {
    assert.ok(path.relative(directory, env[key]).startsWith(`build${path.sep}`));
    assert.ok(fs.statSync(env[key]).isDirectory());
  }
});

test('explicit cache settings and existing build module are preserved', t => {
  const directory = temporaryProject(t);
  fs.mkdirSync(path.join(directory, 'build'));
  const module = 'module existing-build-probes\n';
  fs.writeFileSync(path.join(directory, 'build/go.mod'), module);
  const source = Object.fromEntries(['GOCACHE', 'GOMODCACHE', 'GOTMPDIR', 'TEMP', 'TMP'].map(name => [name, path.join(directory, name)]));
  assert.deepEqual(createGoEnv(directory, source), source);
  assert.equal(fs.readFileSync(path.join(directory, 'build/go.mod'), 'utf8'), module);
});
