// Offline math fallback for chat replies.
//
// This is deliberately a small formatter, not a LaTeX engine. It supports the
// common shorthand used in model answers: $...$ / $$...$$, ^, _, \frac{}{},
// \sqrt{}, a small Greek/symbol table, and a few spacing commands.
//
// It does not support matrices, \begin{...} environments, aligned equations,
// macros, colors, chemistry, font selection, complex limits, or arbitrary TeX.
// Formulas are parsed into escaped text plus fixed HTML tags, so the fallback
// cannot turn formula content into an XSS payload.
//
// To move to a real renderer later, load a local KaTeX bundle before md.js and
// replace `stash` in md.js with KaTeX's renderToString pipeline. No CDN or
// network resource is used here.
(function (root) {
  'use strict';

  var greek = {
    alpha: 'α',
    beta: 'β',
    gamma: 'γ',
    delta: 'δ',
    epsilon: 'ε',
    zeta: 'ζ',
    eta: 'η',
    theta: 'θ',
    iota: 'ι',
    kappa: 'κ',
    lambda: 'λ',
    mu: 'μ',
    nu: 'ν',
    pi: 'π',
    xi: 'ξ',
    rho: 'ρ',
    sigma: 'σ',
    tau: 'τ',
    upsilon: 'υ',
    phi: 'φ',
    chi: 'χ',
    psi: 'ψ',
    omega: 'ω',
    Gamma: 'Γ',
    Delta: 'Δ',
    Theta: 'Θ',
    Lambda: 'Λ',
    Xi: 'Ξ',
    Pi: 'Π',
    Sigma: 'Σ',
    Phi: 'Φ',
    Psi: 'Ψ',
    Omega: 'Ω'
  };

  var symbols = {
    times: '×',
    cdot: '·',
    div: '÷',
    pm: '±',
    mp: '∓',
    le: '≤',
    leq: '≤',
    ge: '≥',
    geq: '≥',
    ne: '≠',
    neq: '≠',
    approx: '≈',
    infinity: '∞',
    infty: '∞',
    sum: '∑',
    int: '∫',
    to: '→',
    rightarrow: '→',
    Rightarrow: '⇒',
    left: '(',
    right: ')'
  };

  var spacing = {
    ',': '\u2009',
    ':': '\u205f',
    ';': '\u2005',
    '!': '',
    quad: '\u2003',
    qquad: '\u2003\u2003'
  };

  function escapeHtml(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function readGroup(src, start) {
    if (src.charAt(start) !== '{') return null;
    var depth = 0;
    for (var i = start; i < src.length; i++) {
      var ch = src.charAt(i);
      if (ch === '\\') {
        i++;
        continue;
      }
      if (ch === '{') depth++;
      if (ch === '}') {
        depth--;
        if (depth === 0) {
          return { value: src.slice(start + 1, i), next: i + 1 };
        }
      }
    }
    return null;
  }

  function readScriptArg(src, start) {
    var group = readGroup(src, start);
    if (group) return { value: group.value, next: group.next };
    if (start >= src.length) return null;
    return { value: src.charAt(start), next: start + 1 };
  }

  function renderExpression(src) {
    var text = String(src == null ? '' : src);
    var out = '';
    var i = 0;

    while (i < text.length) {
      var ch = text.charAt(i);

      if (ch === '\\') {
        var command = /^[a-zA-Z]+/.exec(text.slice(i + 1));
        if (command) {
          var name = command[0];
          var nameEnd = i + 1 + name.length;

          if (name === 'frac') {
            var numerator = readGroup(text, nameEnd);
            var denominator = numerator ? readGroup(text, numerator.next) : null;
            if (numerator && denominator) {
              out += '<span class="math-frac"><span class="math-num">' +
                renderExpression(numerator.value) +
                '</span><span class="math-bar"></span><span class="math-den">' +
                renderExpression(denominator.value) + '</span></span>';
              i = denominator.next;
              continue;
            }
          }

          if (name === 'sqrt') {
            var radicand = readGroup(text, nameEnd);
            if (radicand) {
              out += '<span class="math-sqrt"><span class="math-radical">√</span>' +
                '<span class="math-radicand">' + renderExpression(radicand.value) + '</span></span>';
              i = radicand.next;
              continue;
            }
          }

          if (greek[name]) {
            out += greek[name];
            i = nameEnd;
            continue;
          }
          if (symbols[name]) {
            out += symbols[name];
            i = nameEnd;
            continue;
          }
          if (spacing[name] !== undefined) {
            out += spacing[name];
            i = nameEnd;
            continue;
          }

          out += escapeHtml('\\' + name);
          i = nameEnd;
          continue;
        }

        var escaped = text.charAt(i + 1);
        if (escaped === '\\' || escaped === '{' || escaped === '}' ||
            escaped === '_' || escaped === '^' || escaped === '%' ||
            escaped === '$' || escaped === '#') {
          out += escapeHtml(escaped);
          i += 2;
          continue;
        }
        out += escapeHtml('\\');
        i++;
        continue;
      }

      if (ch === '^' || ch === '_') {
        var script = readScriptArg(text, i + 1);
        if (script) {
          out += ch === '^' ? '<sup>' : '<sub>';
          out += renderExpression(script.value);
          out += ch === '^' ? '</sup>' : '</sub>';
          i = script.next;
          continue;
        }
        out += escapeHtml(ch);
        i++;
        continue;
      }

      var plainStart = i;
      while (i < text.length && text.charAt(i) !== '\\' &&
             text.charAt(i) !== '^' && text.charAt(i) !== '_') {
        i++;
      }
      out += escapeHtml(text.slice(plainStart, i));
    }

    return out;
  }

  function renderFormula(source, block) {
    var className = block ? 'math-block' : 'math-inline';
    return '<span class="math ' + className + '" role="math">' +
      renderExpression(source) + '</span>';
  }

  function isEscaped(src, index) {
    var slashes = 0;
    for (var i = index - 1; i >= 0 && src.charAt(i) === '\\'; i--) slashes++;
    return slashes % 2 === 1;
  }

  function findClosing(src, start, block) {
    for (var i = start; i < src.length; i++) {
      if (!block && src.charAt(i) === '\n') return -1;
      if (src.charAt(i) !== '$' || isEscaped(src, i)) continue;
      if (block) {
        if (src.charAt(i + 1) === '$') return i;
        continue;
      }
      return i;
    }
    return -1;
  }

  // Replace formulas with placeholders. The caller may escape the surrounding
  // Markdown text and restore the returned HTML placeholders afterwards.
  function stash(text, onFormula) {
    var src = String(text == null ? '' : text);
    var out = '';
    var i = 0;
    var seq = 0;

    while (i < src.length) {
      if (src.charAt(i) !== '$' || isEscaped(src, i)) {
        out += src.charAt(i);
        i++;
        continue;
      }

      var block = src.charAt(i + 1) === '$';
      var openLen = block ? 2 : 1;
      var close = findClosing(src, i + openLen, block);
      if (close === -1) {
        out += src.charAt(i);
        i++;
        continue;
      }

      var raw = src.slice(i + openLen, close);
      if (!raw) {
        out += src.charAt(i);
        i++;
        continue;
      }
      var html = renderFormula(raw, block);
      var token = '\u0000M' + seq + '\u0000';
      seq++;
      if (typeof onFormula === 'function') onFormula(html, token);
      out += token;
      i = close + openLen;
    }

    return out;
  }

  function render(text) {
    var parts = [];
    var stashed = stash(text, function (html) {
      parts.push(html);
    });
    return escapeHtml(stashed).replace(/\u0000M(\d+)\u0000/g, function (_, index) {
      return parts[Number(index)] || '';
    });
  }

  root.MATH = {
    render: render,
    stash: stash,
    renderFormula: renderFormula,
    escapeHtml: escapeHtml
  };

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = root.MATH;
  }
})(typeof window !== 'undefined' ? window : globalThis);
