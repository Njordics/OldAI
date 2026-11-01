// codekit: safe Markdown, syntax highlighting and line diffs for the Chat and
// Code tabs. No dependencies, no eval, no innerHTML of untrusted input: every
// function escapes its input first and only then adds markup of its own.
// Also loads under Node for unit tests (test/ui/codekit.test.js).
"use strict";

const codekit = (() => {
  function escHTML(s) {
    return String(s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  // ---------- Languages ----------

  const EXT = {
    js: "js", mjs: "js", cjs: "js", jsx: "js", ts: "js", tsx: "js",
    py: "py", go: "go", rs: "c", c: "c", h: "c", cpp: "c", cc: "c", hpp: "c", java: "c", cs: "c", kt: "c", swift: "c", php: "c",
    html: "html", htm: "html", xml: "html", svg: "html", vue: "html",
    css: "css", scss: "css", json: "json", sh: "sh", bash: "sh", zsh: "sh", ps1: "sh",
    sql: "sql", yaml: "yaml", yml: "yaml", toml: "yaml", ini: "yaml", md: "md", txt: "text", rb: "py",
  };
  const ALIAS = {
    javascript: "js", typescript: "js", node: "js", python: "py", golang: "go", rust: "c", "c++": "c", csharp: "c", "c#": "c",
    java: "c", kotlin: "c", shell: "sh", console: "sh", powershell: "sh", xml: "html", markdown: "md", plaintext: "text",
  };
  const LANG_NAMES = { js: "JavaScript", py: "Python", go: "Go", c: "C-family", html: "HTML", css: "CSS", json: "JSON", sh: "Shell", sql: "SQL", yaml: "YAML", md: "Markdown", text: "Text" };

  function langOf(nameOrTag) {
    const s = String(nameOrTag || "").toLowerCase().trim();
    if (ALIAS[s]) return ALIAS[s];
    if (EXT[s]) return EXT[s];
    const ext = s.includes(".") ? s.split(".").pop() : "";
    return EXT[ext] || (LANG_NAMES[s] ? s : "text");
  }

  const words = s => new Set(s.split(" "));
  const KW = {
    js: words("break case catch class const continue debugger default delete do else export extends finally for from function if import in instanceof let new of return static super switch this throw try typeof var void while with yield async await interface type enum implements private public protected readonly as"),
    py: words("and as assert async await break class continue def del elif else except finally for from global if import in is lambda nonlocal not or pass raise return try while with yield match case self"),
    go: words("break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var"),
    c: words("auto break case char const continue default do double else enum extern float for goto if int long register return short signed sizeof static struct switch typedef union unsigned void volatile while class public private protected new delete this throw try catch namespace using template virtual override fn let mut impl pub use mod match loop trait where self Self crate import package extends implements interface final abstract var val fun when string bool"),
    sh: words("if then else elif fi for while do done case esac in function return export local echo exit cd set unset source"),
    sql: words("select from where and or not insert into values update set delete create table index drop alter add primary key foreign references join left right inner outer on group by order having limit offset as distinct union all null is like in between case when then else end"),
  };
  const LIT = words("true false null undefined None True False nil NaN Infinity");

  // Each rule: [sticky regex, class]. Earlier rules win.
  const STR = /"(?:[^"\\\n]|\\.)*"?|'(?:[^'\\\n]|\\.)*'?/y;
  const NUM = /\b(?:0[xX][\da-fA-F_]+|\d[\d_]*(?:\.\d+)?(?:[eE][+-]?\d+)?)\b/y;
  const IDENT = /[A-Za-z_$][\w$]*/y;
  const RULES = {
    js: [[/\/\/[^\n]*/y, "com"], [/\/\*[\s\S]*?(?:\*\/|$)/y, "com"], [/`(?:[^`\\]|\\[\s\S])*`?/y, "str"], [STR, "str"], [NUM, "num"]],
    go: [[/\/\/[^\n]*/y, "com"], [/\/\*[\s\S]*?(?:\*\/|$)/y, "com"], [/`[^`]*`?/y, "str"], [STR, "str"], [NUM, "num"]],
    c: [[/\/\/[^\n]*/y, "com"], [/\/\*[\s\S]*?(?:\*\/|$)/y, "com"], [/#\s*\w+/y, "kw"], [STR, "str"], [NUM, "num"]],
    py: [[/#[^\n]*/y, "com"], [/(?:[rbfu]{0,2})(?:"""[\s\S]*?(?:"""|$)|'''[\s\S]*?(?:'''|$))/yi, "str"], [/(?:[rbfu]{0,2})(?:"(?:[^"\\\n]|\\.)*"?|'(?:[^'\\\n]|\\.)*'?)/yi, "str"], [/@[\w.]+/y, "fn"], [NUM, "num"]],
    sh: [[/#[^\n]*/y, "com"], [STR, "str"], [/\$\{?[\w@#?*!-]+\}?/y, "attr"], [NUM, "num"]],
    sql: [[/--[^\n]*/y, "com"], [/\/\*[\s\S]*?(?:\*\/|$)/y, "com"], [STR, "str"], [NUM, "num"]],
    css: [[/\/\*[\s\S]*?(?:\*\/|$)/y, "com"], [STR, "str"], [/#[\da-fA-F]{3,8}\b/y, "num"], [/-?\d*\.?\d+(?:px|em|rem|%|vh|vw|s|ms|deg|fr)?\b/y, "num"], [/[\w-]+(?=\s*:[^:{;]*[;}])/y, "attr"], [/@[\w-]+/y, "kw"]],
    json: [[/"(?:[^"\\\n]|\\.)*"(?=\s*:)/y, "attr"], [STR, "str"], [NUM, "num"]],
    yaml: [[/#[^\n]*/y, "com"], [/^[ \t-]*[\w.-]+(?=\s*[:=])/my, "attr"], [STR, "str"], [NUM, "num"], [/^\s*\[[^\]\n]*\]/my, "kw"]],
    html: [[/<!--[\s\S]*?(?:-->|$)/y, "com"], [/<\/?[A-Za-z][\w:-]*/y, "tag"], [/\/?>/y, "tag"], [/[\w:-]+(?==)/y, "attr"], [/"[^"]*"?|'[^']*'?/y, "str"], [/&[#\w]+;/y, "num"]],
  };

  // highlight returns HTML: escaped code with <span class="tok-*"> around tokens.
  function highlight(code, lang) {
    code = String(code);
    lang = langOf(lang);
    const rules = RULES[lang];
    if (!rules || code.length > 400000) return escHTML(code);
    const kw = KW[lang] || new Set();
    const caseless = lang === "sql";
    let out = "";
    let plain = "";
    let i = 0;
    const flush = () => { if (plain) { out += escHTML(plain); plain = ""; } };
    const emit = (text, cls) => { flush(); out += `<span class="tok-${cls}">${escHTML(text)}</span>`; };
    outer: while (i < code.length) {
      for (const [re, cls] of rules) {
        re.lastIndex = i;
        const m = re.exec(code);
        if (m && m[0].length) {
          emit(m[0], cls);
          i += m[0].length;
          continue outer;
        }
      }
      if (lang !== "html" && lang !== "css" && lang !== "json" && lang !== "yaml") {
        IDENT.lastIndex = i;
        const m = IDENT.exec(code);
        if (m) {
          const w = m[0];
          const after = code.slice(i + w.length).match(/^\s*\(/);
          if (kw.has(caseless ? w.toLowerCase() : w)) emit(w, "kw");
          else if (LIT.has(w)) emit(w, "lit");
          else if (after) emit(w, "fn");
          else plain += w;
          i += w.length;
          continue;
        }
      } else if (lang === "json" || lang === "yaml") {
        IDENT.lastIndex = i;
        const m = IDENT.exec(code);
        if (m) {
          if (LIT.has(m[0])) emit(m[0], "lit");
          else plain += m[0];
          i += m[0].length;
          continue;
        }
      }
      plain += code[i++];
    }
    flush();
    return out;
  }

  // ---------- Markdown ----------

  function inline(text) {
    // Protect code spans, escape everything, then add emphasis and links.
    const codes = [];
    let s = String(text).replace(/`([^`\n]+)`/g, (_, c) => `\u0000${codes.push(c) - 1}\u0000`);
    s = escHTML(s);
    s = s.replace(/\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g, (_, t, u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${t}</a>`);
    s = s.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/__([^_\n]+)__/g, "<strong>$1</strong>");
    s = s.replace(/(^|[^\w*])\*([^*\n]+)\*(?!\w)/g, "$1<em>$2</em>");
    s = s.replace(/(^|[^\w])_([^_\n]+)_(?!\w)/g, "$1<em>$2</em>");
    s = s.replace(/~~([^~\n]+)~~/g, "<del>$1</del>");
    return s.replace(/\u0000(\d+)\u0000/g, (_, n) => `<code>${escHTML(codes[+n])}</code>`);
  }

  const DEFAULT_ACTIONS = [["copy", "Copy"], ["open", "Open in Code"]];

  function codeBlock(code, tag, actions) {
    const lang = langOf(tag);
    const label = tag ? escHTML(tag) : LANG_NAMES[lang] || "code";
    const buttons = (actions || DEFAULT_ACTIONS).map(([act, text]) =>
      `<button type="button" class="btn btn-sm btn-ghost" data-cb="${escHTML(act)}">${escHTML(text)}</button>`).join("");
    return `<div class="codeblock" data-lang="${escHTML(tag || lang)}"><div class="cb-bar"><span>${label}</span>` +
      `<span class="cb-actions">${buttons}</span></div>` +
      `<pre><code class="hl">${highlight(code, lang)}</code></pre></div>`;
  }

  // renderMarkdown turns model output into safe HTML. Unclosed code fences
  // (mid-stream) render as code so streaming looks right. opts.codeActions
  // lists [action, label] buttons for code blocks.
  function renderMarkdown(src, opts) {
    const actions = opts && opts.codeActions;
    const lines = String(src).replace(/\r\n?/g, "\n").split("\n");
    const out = [];
    let para = [];
    let list = null; // {ordered, items: []}
    const flushPara = () => { if (para.length) { out.push(`<p>${inline(para.join("\n")).replace(/\n/g, "<br>")}</p>`); para = []; } };
    const flushList = () => {
      if (list) {
        const tag = list.ordered ? "ol" : "ul";
        out.push(`<${tag}>${list.items.map(it => `<li>${inline(it).replace(/\n/g, "<br>")}</li>`).join("")}</${tag}>`);
        list = null;
      }
    };
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      const fence = line.match(/^\s*(```+|~~~+)\s*([\w+#.-]*)/);
      if (fence) {
        flushPara(); flushList();
        const body = [];
        let j = i + 1;
        for (; j < lines.length && !lines[j].trim().startsWith(fence[1]); j++) body.push(lines[j]);
        out.push(codeBlock(body.join("\n"), fence[2], actions));
        i = j;
        continue;
      }
      if (!line.trim()) { flushPara(); flushList(); continue; }
      const h = line.match(/^(#{1,6})\s+(.*)$/);
      if (h) { flushPara(); flushList(); const n = Math.min(h[1].length + 2, 6); out.push(`<h${n}>${inline(h[2])}</h${n}>`); continue; }
      if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { flushPara(); flushList(); out.push("<hr>"); continue; }
      const q = line.match(/^\s*>\s?(.*)$/);
      if (q) { flushPara(); flushList(); out.push(`<blockquote>${inline(q[1])}</blockquote>`); continue; }
      const li = line.match(/^\s*(?:([-*+])|(\d+)[.)])\s+(.*)$/);
      if (li) {
        flushPara();
        const ordered = !li[1];
        if (!list || list.ordered !== ordered) { flushList(); list = { ordered, items: [] }; }
        list.items.push(li[3]);
        continue;
      }
      if (list && /^\s{2,}\S/.test(line)) { list.items[list.items.length - 1] += "\n" + line.trim(); continue; }
      flushList();
      para.push(line);
    }
    flushPara(); flushList();
    return out.join("");
  }

  // codeBlocks extracts fenced blocks: [{lang, code}].
  function codeBlocks(src) {
    const out = [];
    const re = /^[ \t]*(```+|~~~+)[ \t]*([\w+#.-]*)[^\n]*\n([\s\S]*?)(?:^[ \t]*\1[ \t]*$|(?![\s\S]))/gm;
    let m;
    while ((m = re.exec(String(src).replace(/\r\n?/g, "\n")))) out.push({ lang: m[2], code: m[3].replace(/\n$/, "") });
    return out;
  }

  // ---------- Diff ----------

  // lineDiff returns [{op: " " | "+" | "-", text}] via LCS; very large inputs
  // fall back to "replace everything".
  function lineDiff(a, b) {
    const A = String(a).split("\n"), B = String(b).split("\n");
    let p = 0;
    while (p < A.length && p < B.length && A[p] === B[p]) p++;
    let s = 0;
    while (s < A.length - p && s < B.length - p && A[A.length - 1 - s] === B[B.length - 1 - s]) s++;
    const a2 = A.slice(p, A.length - s), b2 = B.slice(p, B.length - s);
    const out = A.slice(0, p).map(text => ({ op: " ", text }));
    if (a2.length * b2.length > 4e6) {
      a2.forEach(text => out.push({ op: "-", text }));
      b2.forEach(text => out.push({ op: "+", text }));
    } else {
      const n = a2.length, m = b2.length;
      const L = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
      for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--)
        L[i][j] = a2[i] === b2[j] ? L[i + 1][j + 1] + 1 : Math.max(L[i + 1][j], L[i][j + 1]);
      let i = 0, j = 0;
      while (i < n && j < m) {
        if (a2[i] === b2[j]) { out.push({ op: " ", text: a2[i] }); i++; j++; }
        else if (L[i + 1][j] >= L[i][j + 1]) out.push({ op: "-", text: a2[i++] });
        else out.push({ op: "+", text: b2[j++] });
      }
      while (i < n) out.push({ op: "-", text: a2[i++] });
      while (j < m) out.push({ op: "+", text: b2[j++] });
    }
    A.slice(A.length - s).forEach(text => out.push({ op: " ", text }));
    return out;
  }

  // diffHTML renders a diff with unchanged runs collapsed to `context` lines.
  function diffHTML(ops, lang, context = 3) {
    const keep = ops.map(() => false);
    ops.forEach((o, i) => {
      if (o.op !== " ") for (let k = Math.max(0, i - context); k <= Math.min(ops.length - 1, i + context); k++) keep[k] = true;
    });
    let html = "", skipped = 0;
    ops.forEach((o, i) => {
      if (!keep[i]) { skipped++; return; }
      if (skipped) { html += `<div class="d-skip">⋯ ${skipped} unchanged line${skipped === 1 ? "" : "s"}</div>`; skipped = 0; }
      const cls = o.op === "+" ? "d-add" : o.op === "-" ? "d-del" : "d-ctx";
      html += `<div class="${cls}"><span class="d-op">${o.op === " " ? "&nbsp;" : o.op}</span>${highlight(o.text, lang) || "&nbsp;"}</div>`;
    });
    if (skipped) html += `<div class="d-skip">⋯ ${skipped} unchanged line${skipped === 1 ? "" : "s"}</div>`;
    return html;
  }

  // extFor suggests a file extension for a code-fence language tag.
  const EXT_FOR = {
    python: "py", py: "py", javascript: "js", js: "js", typescript: "ts", ts: "ts", jsx: "jsx", tsx: "tsx", html: "html",
    css: "css", json: "json", bash: "sh", sh: "sh", shell: "sh", zsh: "sh", powershell: "ps1", sql: "sql", go: "go",
    golang: "go", rust: "rs", java: "java", c: "c", cpp: "cpp", "c++": "cpp", csharp: "cs", "c#": "cs", yaml: "yaml",
    yml: "yaml", toml: "toml", markdown: "md", md: "md", xml: "xml", kotlin: "kt", swift: "swift", php: "php", ruby: "rb",
  };
  function extFor(tag) {
    return EXT_FOR[String(tag || "").toLowerCase()] || "txt";
  }

  return { escHTML, langOf, LANG_NAMES, highlight, renderMarkdown, codeBlocks, lineDiff, diffHTML, extFor };
})();

if (typeof module !== "undefined") module.exports = codekit;
