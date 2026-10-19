'use strict';

function loadJsdom() {
  try {
    return require('jsdom');
  } catch (error) {
    // Do not hide an installed package's loading errors behind another copy.
    if (error.code !== 'MODULE_NOT_FOUND' || !error.message.includes("'jsdom'")) throw error;
  }
  if (process.env.JSDOM_PATH) return require(process.env.JSDOM_PATH);
  throw new Error('找不到 jsdom。请运行 npm ci，或设置 JSDOM_PATH 指向 jsdom 包目录。');
}

module.exports = { loadJsdom };
