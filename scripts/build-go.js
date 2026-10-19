// 跨平台 Go 构建脚本：Windows 输出 blog-my.exe，其他平台输出 blog-my。
const fs = require('fs');
const path = require('path');
const { root, runGo } = require('./lib/go-runtime.cjs');
const binDir = path.join(root, 'bin');
fs.mkdirSync(binDir, { recursive: true });

const name = process.platform === 'win32' ? 'blog-my.exe' : 'blog-my';
const out = path.join(binDir, name);

// Go compiles the entry point and imported packages in one build.
runGo(['build', '-trimpath', '-o', out, './cmd/blog-my']);
console.log(`[build:go] ${out}`);
