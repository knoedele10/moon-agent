'use strict';
// CloudBase 云函数：/api/health（Day 15 课程要求的"云函数部署"形态）
// 入口签名按 CloudBase Node.js 云函数约定：exports.main(event, context)
exports.main = async function (event, context) {
  return {
    status: 'ok',
    service: 'blog-my',
    version: 'mock-1',
    time: new Date().toISOString()
  };
};
