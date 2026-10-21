// 前端构建：把设计原型（单一真源）复制成 Wails 资产
// 原型内置示意数据 → 浏览器双击可看；打包进 exe 后自动走 window.go 真后端
const fs = require('fs');
const src = fs.readFileSync('design/ui-prototype.html', 'utf8');
const out = src
  .replace('<title>3z9wOpt v1.1.0 · UI 原型</title>', '<title>无名大叔系统优化工具箱 v1.1.0</title>');
fs.mkdirSync('frontend/dist', { recursive: true });
fs.writeFileSync('frontend/dist/index.html', out, 'utf8');
console.log('frontend/dist/index.html', (out.length / 1024).toFixed(1) + ' KB');
