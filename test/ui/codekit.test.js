// Unit tests for the dashboard's codekit (run: node --test test/ui).
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const ck = require("../../controller/web/ui/codekit.js");

const XSS = [
  `<img src=x onerror=alert(1)>`,
  `<script>alert(1)</script>`,
  `[click](javascript:alert(1))`,
  `**<b onmouseover=alert(1)>x</b>**`,
  "`<svg onload=alert(1)>`",
  "```html\n<script>alert(1)</script>\n```",
  `> <iframe src="javascript:alert(1)">`,
  `- <a href="javascript:alert(1)">x</a>`,
  `[x](https://ok.example/"onmouseover="alert(1))`,
];

// Every tag in the output must be one codekit itself produces, with only its
// own attributes; input text can only ever appear escaped.
const TAGS = new Set(["p", "br", "strong", "em", "del", "code", "a", "h3", "h4", "h5", "h6", "ul", "ol", "li", "blockquote", "hr", "div", "span", "pre", "button"]);
const ATTRS = new Set(["href", "target", "rel", "class", "data-lang", "data-cb", "type"]);
function assertSafe(html, src) {
  for (const m of html.matchAll(/<\/?([a-zA-Z0-9]+)([^>]*)>/g)) {
    assert.ok(TAGS.has(m[1].toLowerCase()), `tag <${m[1]}> from ${src}`);
    for (const a of m[2].matchAll(/([^\s=]+)="([^"]*)"/g)) {
      assert.ok(ATTRS.has(a[1]), `attribute ${a[1]} from ${src}`);
      if (a[1] === "href") assert.match(a[2], /^https?:\/\//, src);
    }
    assert.doesNotMatch(m[2].replace(/([^\s=]+)="[^"]*"/g, ""), /\S/, `unquoted attribute in ${m[0]}`);
  }
}

test("markdown never emits markup from its input", () => {
  for (const src of XSS) assertSafe(ck.renderMarkdown(src), src);
});

test("only http(s) links become anchors, opened safely", () => {
  const html = ck.renderMarkdown("see [docs](https://example.com/a?b=1) and [x](ftp://nope)");
  assert.match(html, /<a href="https:\/\/example.com\/a\?b=1" target="_blank" rel="noopener noreferrer">docs<\/a>/);
  assert.doesNotMatch(html, /href="ftp/);
});

test("markdown structure", () => {
  const html = ck.renderMarkdown("# Title\n\nSome **bold** and *it* and `code`.\n\n- a\n- b\n\n1. one\n2. two\n\n> quote\n\n---");
  for (const want of ["<h3>Title</h3>", "<strong>bold</strong>", "<em>it</em>", "<code>code</code>", "<ul><li>a</li><li>b</li></ul>", "<ol><li>one</li><li>two</li></ol>", "<blockquote>quote</blockquote>", "<hr>"]) {
    assert.ok(html.includes(want), `missing ${want} in ${html}`);
  }
});

test("code fences render highlighted with actions, even unclosed", () => {
  const html = ck.renderMarkdown("Here:\n```python\ndef f(x):\n    return x  # done\n```");
  assert.match(html, /data-lang="python"/);
  assert.match(html, /<span class="tok-kw">def<\/span>/);
  assert.match(html, /<span class="tok-com"># done<\/span>/);
  assert.match(html, /data-cb="copy"/);
  const streaming = ck.renderMarkdown("```js\nconst a = 1;");
  assert.match(streaming, /<span class="tok-kw">const<\/span>/);
});

test("highlight output text equals the input (copy relies on it)", () => {
  const samples = {
    js: "const s = `a${b}` // c\nfunction f() { return 1.5e3 < 2 && 'x'; }",
    py: "@dec\ndef g(a, b='q'):\n    \"\"\"doc\"\"\"\n    return None",
    go: "func main() {\n\tfmt.Println(\"hi\") // x\n}",
    html: "<div class=\"a\">&amp; <!-- c --></div>",
    css: ".a { color: #fff; margin: 1.5rem; }",
    json: "{\"a\": [1, true, null, \"s\"]}",
    sql: "SELECT a FROM t WHERE b = 'x' -- c",
  };
  for (const [lang, code] of Object.entries(samples)) {
    const html = ck.highlight(code, lang);
    const text = html.replace(/<[^>]+>/g, "").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, "&");
    assert.equal(text, code, lang);
  }
});

test("language detection", () => {
  assert.equal(ck.langOf("src/app.tsx"), "js");
  assert.equal(ck.langOf("main.go"), "go");
  assert.equal(ck.langOf("Python"), "py");
  assert.equal(ck.langOf("README"), "text");
});

test("codeBlocks extracts fenced code", () => {
  const blocks = ck.codeBlocks("text\n```js\nlet a = 1;\nlet b = 2;\n```\nmore\n```\nplain\n```");
  assert.deepEqual(blocks, [{ lang: "js", code: "let a = 1;\nlet b = 2;" }, { lang: "", code: "plain" }]);
});

test("lineDiff finds minimal edits", () => {
  const d = ck.lineDiff("a\nb\nc\nd", "a\nB\nc\nd\ne");
  assert.deepEqual(d.map(o => o.op + o.text), [" a", "-b", "+B", " c", " d", "+e"]);
  assert.deepEqual(ck.lineDiff("same", "same").map(o => o.op), [" "]);
  const html = ck.diffHTML(ck.lineDiff(Array.from({ length: 20 }, (_, i) => `l${i}`).join("\n"), Array.from({ length: 20 }, (_, i) => i === 10 ? "X" : `l${i}`).join("\n")), "text", 2);
  assert.match(html, /⋯ 8 unchanged lines/);
});
