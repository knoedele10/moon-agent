'use strict';

const path = require('path');
const { spawnSync } = require('child_process');

// Run real templates offline with simulated API responses.
const scripts = process.argv.includes('--taskcard')
  ? ['verify-ui.cjs']
  : ['verify-autonomy-ui.cjs', 'verify-agent-activity.cjs', 'verify-smooth-ui.cjs', 'verify-evolution-ui.cjs', 'verify-plan-ui.cjs', 'verify-review-ui.cjs'];
for (const script of scripts) {
  const env = script === 'verify-ui.cjs' ? { ...process.env, TASKCARD_ONLY: '1' } : process.env;
  const result = spawnSync(process.execPath, [path.join(__dirname, script)], { stdio: 'inherit', env });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status || 1);
}
