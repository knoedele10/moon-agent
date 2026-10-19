'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const beforeBuild = require('./build-policy.cjs');

test('packaging skips collection only when there are no runtime dependencies', async (t) => {
  const appDir = fs.mkdtempSync(path.join(os.tmpdir(), 'blog-my-packaging-'));
  t.after(() => fs.rmSync(appDir, { recursive: true, force: true }));
  for (const [metadata, expected] of [
    [{ devDependencies: { jsdom: '29.1.1', electron: '44.4.5' } }, false],
    [{ dependencies: { runtime: '1.0.0' } }, true],
    [{ optionalDependencies: { native: '1.0.0' } }, true],
  ]) {
    fs.writeFileSync(path.join(appDir, 'package.json'), JSON.stringify(metadata));
    assert.equal(await beforeBuild({ appDir }), expected);
  }
});
