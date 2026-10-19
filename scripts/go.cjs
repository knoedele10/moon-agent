'use strict';

const { runGo } = require('./lib/go-runtime.cjs');
const [command = 'test', ...args] = process.argv.slice(2);
if (!['test', 'vet'].includes(command)) {
  console.error('Usage: node scripts/go.cjs <test|vet> [Go flags]');
  process.exit(1);
}
runGo([command, './internal/...', './cmd/...', ...args]);
