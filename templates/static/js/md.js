// 极简 Markdown 渲染器（模型回复专用）。
//
// 为什么需要它：聊天界面原本把回复当纯文本追加到文本节点里，于是模型写的
// `**加粗**` 会原样显示成带星号的样子，代码围栏、列表、标题同理。模型按 Markdown
// 输出是很自然的行为，缺的是前端这一层渲染。
//
// 安全是硬要求，顺序不能反：**先整体转义 HTML，再套白名单语法**。
// 绝不把模型输出当 HTML 插入——那等于给了它一个 XSS 入口。链接也只允许 http/https。
// 数学公式先由 math.js 换成只含固定标签的占位符，公式正文同样转义；
// 这样 $x^2$ 不会被强调/链接规则拆坏，也不会绕过 HTML 白名单。
(function (root) {
  'use strict';

  // md.js can be checked directly in Node (build/md-check.cjs) without a browser.
  // Load the optional offline math companion when it has not been registered yet.
  if (!root.MATH && typeof require === 'function') {
    try { require('./math.js'); } catch (e) { /* math fallback is optional */ }
  }

  function escapeHtml(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // renderInline 处理行内语法。入参**必须是已转义**的文本。
  function renderInline(s, mathParts) {
    // 行内代码优先：它的内容不该再被当成强调语法。
    s = s.replace(/`([^`\n]+)`/g, function (_, code) {
      return '<code>' + code + '</code>';
    });
    s = s.replace(/\*\*([^*\n]+)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<em>$2</em>');
    // 链接：只认 http/https，其余（javascript:、data: 等）原样留成文本。
    s = s.replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g,
      '<a href="$2" target="_blank" rel="noopener noreferrer">$1</a>');
    // Math placeholders were created before escaping and must be restored last.
    if (mathParts && mathParts.length) {
      s = s.replace(/\u0000M(\d+)\u0000/g, function (_, index) {
        return mathParts[Number(index)] || '';
      });
    }
    return s;
  }

  function render(text) {
    var src = String(text == null ? '' : text);

    // 第一步：把围栏代码块挖出来占位，免得里面的 ** 被当成强调、内容被拆行。
    var blocks = [];
    src = src.replace(/```[ \t]*([a-zA-Z0-9_+-]*)[ \t]*\n?([\s\S]*?)```/g, function (_, lang, code) {
      blocks.push(code.replace(/\n$/, ''));
      return '\u0000B' + (blocks.length - 1) + '\u0000';
    });

    // Math must be protected before HTML escaping and Markdown emphasis. The
    // generated HTML is fixed markup with all formula text escaped by math.js.
    var mathParts = [];
    if (root.MATH && typeof root.MATH.stash === 'function') {
      src = root.MATH.stash(src, function (html) { mathParts.push(html); });
    }

    var lines = escapeHtml(src).split('\n');
    var html = [];
    var list = null; // 'ul' | 'ol' | null
    var para = [];   // 连续普通行的缓冲，合成一个 <p>

    function closeList() {
      if (list) { html.push(list === 'ul' ? '</ul>' : '</ol>'); list = null; }
    }
    function flushPara() {
      if (para.length) { html.push('<p>' + para.join('<br>') + '</p>'); para = []; }
    }

    // 表格：模型写 Markdown 表格的频率很高，以前会原样显示成一排竖线。
    // 只在「表头 + 分隔行」同时成立时才认，避免把正文里偶然的 | 当成表格。
    function splitRow(row) {
      var s = row.trim();
      if (s.charAt(0) === '|') s = s.slice(1);
      if (s.charAt(s.length - 1) === '|') s = s.slice(0, -1);
      return s.split('|').map(function (cell) { return cell.trim(); });
    }
    function isSeparatorRow(row) {
      var cells = splitRow(row);
      if (!cells.length) return false;
      return cells.every(function (cell) { return /^:?-{1,}:?$/.test(cell); });
    }
    function cellAlign(cell) {
      var left = cell.charAt(0) === ':';
      var right = cell.charAt(cell.length - 1) === ':';
      if (left && right) return 'center';
      if (right) return 'right';
      return '';
    }
    function isTableRow(line) {
      return /^\s*\|/.test(line);
    }

    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      var mUl = /^\s*[-*+]\s+(.*)$/.exec(line);
      var mOl = /^\s*\d+[.)]\s+(.*)$/.exec(line);
      var mH = /^\s*(#{1,4})\s+(.*)$/.exec(line);

      // 表格先于列表/标题判断：`| - |` 这样的单元格不该被当成列表项。
      if (isTableRow(line) && i + 1 < lines.length && isSeparatorRow(lines[i + 1])) {
        flushPara();
        closeList();
        var headCells = splitRow(line);
        var aligns = splitRow(lines[i + 1]).map(function (cell) {
          var a = cellAlign(cell);
          return a ? ' style="text-align:' + a + '"' : '';
        });
        var out = ['<div class="md-table"><table><thead><tr>'];
        for (var h = 0; h < headCells.length; h++) {
          out.push('<th' + (aligns[h] || '') + '>' + renderInline(headCells[h], mathParts) + '</th>');
        }
        out.push('</tr></thead><tbody>');
        i += 1; // 跨过分隔行
        while (i + 1 < lines.length && isTableRow(lines[i + 1])) {
          i += 1;
          var cells = splitRow(lines[i]);
          out.push('<tr>');
          for (var c = 0; c < headCells.length; c++) {
            out.push('<td' + (aligns[c] || '') + '>' + renderInline(cells[c] || '', mathParts) + '</td>');
          }
          out.push('</tr>');
        }
        out.push('</tbody></table></div>');
        html.push(out.join(''));
        continue;
      }

      if (mUl) {
        flushPara();
        if (list !== 'ul') { closeList(); html.push('<ul>'); list = 'ul'; }
        html.push('<li>' + renderInline(mUl[1], mathParts) + '</li>');
        continue;
      }
      if (mOl) {
        flushPara();
        if (list !== 'ol') { closeList(); html.push('<ol>'); list = 'ol'; }
        html.push('<li>' + renderInline(mOl[1], mathParts) + '</li>');
        continue;
      }
      if (mH) {
        flushPara();
        closeList();
        // 保留标题层级（# → h2，#### → h5）：以前全部压成 h4，模型写的「## 小节」
        // 和正文一样大，长回答就成了一堵平墙。h1 留给页面标题，不从消息里产出。
        var level = Math.min(mH[1].length + 1, 5);
        html.push('<h' + level + '>' + renderInline(mH[2], mathParts) + '</h' + level + '>');
        continue;
      }
      if (/^\s*$/.test(line)) { flushPara(); closeList(); continue; }

      closeList();
      para.push(renderInline(line, mathParts));
    }
    flushPara();
    closeList();

    // 第二步：把代码块占位换回来（内容单独转义一次）。
    return html.join('').replace(/\u0000B(\d+)\u0000/g, function (_, idx) {
      // 复制按钮只是占位结构，点击由 chat.js 用事件委托处理（md.js 只产出字符串）。
      return '<pre class="md-code"><button class="md-code-copy" data-md-copy="1" type="button" aria-label="复制这段代码" title="复制">复制</button><code>'
        + escapeHtml(blocks[Number(idx)]) + '</code></pre>';
    });
  }

  root.MD = { render: render, escapeHtml: escapeHtml };
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = { render: render, escapeHtml: escapeHtml };
  }
})(typeof window !== 'undefined' ? window : globalThis);
