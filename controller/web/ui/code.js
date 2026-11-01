// Code tab: a small workspace (files stored on the Controller), a highlighting
// editor, an AI pane that proposes changes as reviewable diffs, and a
// sandboxed live preview for HTML/CSS/JavaScript.
"use strict";

const SAVE_DELAY_MS = 700;
const AI_HISTORY = 6; // earlier messages sent along for follow-ups

const code = {
  files: [],
  path: null,
  saved: "",      // content as last saved
  saveTimer: null,
  saving: false,
  loaded: false,
  ai: [],         // {role, content, error?, node?}
  aiController: null,
  review: null,   // {path, start, end, original, proposed, lang}
  pvReady: null,
};

const ed = () => $("#editor-text");
const ckLang = () => codekit.langOf(code.path || "");

// ---------- Workspace I/O ----------

async function loadWorkspace() {
  code.files = await api("GET", "/api/v1/workspace/files");
  code.loaded = true;
  renderFiles();
  if (!code.path && code.files.length) {
    const last = safeStorage("get", "aic.code.file");
    await openFile(code.files.some(f => f.path === last) ? last : code.files[0].path);
  }
  renderEditorState();
}

function safeStorage(op, key, val) {
  try {
    if (op === "get") return localStorage.getItem(key);
    localStorage.setItem(key, val);
  } catch { /* storage may be unavailable */ }
  return null;
}

async function openFile(path) {
  await flushSave();
  const f = await api("GET", `/api/v1/workspace/file?path=${encodeURIComponent(path)}`);
  code.path = f.path;
  code.saved = f.content;
  setEditorText(f.content);
  safeStorage("set", "aic.code.file", f.path);
  closeReview();
  renderFiles();
  renderEditorState();
}

function scheduleSave() {
  setStatus("Unsaved");
  clearTimeout(code.saveTimer);
  code.saveTimer = setTimeout(() => flushSave(), SAVE_DELAY_MS);
}

async function flushSave() {
  clearTimeout(code.saveTimer);
  code.saveTimer = null;
  if (!code.path) return;
  const content = ed().value;
  if (content === code.saved) return;
  code.saving = true;
  setStatus("Saving…");
  try {
    const f = await api("PUT", "/api/v1/workspace/file", { path: code.path, content });
    code.saved = content;
    const i = code.files.findIndex(x => x.path === f.path);
    if (i >= 0) code.files[i] = f; else code.files.push(f);
    setStatus(ed().value === code.saved ? "Saved" : "Unsaved");
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    setStatus("Not saved: " + e.message, true);
  } finally {
    code.saving = false;
  }
}

function setStatus(text, bad) {
  const s = $("#code-status");
  s.textContent = text;
  s.classList.toggle("bad", !!bad);
}

async function createFile(path, content) {
  path = (path || "").trim();
  if (!path) return;
  if (code.files.some(f => f.path === path)) {
    toast(`${path} already exists`, true);
    return openFile(path);
  }
  try {
    await api("PUT", "/api/v1/workspace/file", { path, content: content || "" });
    code.files = await api("GET", "/api/v1/workspace/files");
    await openFile(path);
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

function uniqueName(base, ext) {
  let name = `${base}.${ext}`;
  for (let i = 2; code.files.some(f => f.path === name); i++) name = `${base}-${i}.${ext}`;
  return name;
}

async function renameFile() {
  if (!code.path) return;
  const to = prompt("Rename file to:", code.path);
  if (!to || to.trim() === code.path) return;
  await flushSave();
  try {
    await api("POST", "/api/v1/workspace/rename", { from: code.path, to: to.trim() });
    code.files = await api("GET", "/api/v1/workspace/files");
    await openFile(to.trim());
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

async function deleteFile() {
  if (!code.path || !confirm(`Delete ${code.path}? This can't be undone.`)) return;
  clearTimeout(code.saveTimer);
  try {
    await api("DELETE", `/api/v1/workspace/file?path=${encodeURIComponent(code.path)}`);
    code.files = code.files.filter(f => f.path !== code.path);
    code.path = null;
    setEditorText("");
    if (code.files.length) await openFile(code.files[0].path);
    renderFiles();
    renderEditorState();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

async function uploadFiles(list) {
  for (const file of list) {
    if (file.size > 1 << 20) { toast(`${file.name} is over 1 MB`, true); continue; }
    const text = await file.text();
    if (text.includes("\u0000")) { toast(`${file.name} looks binary; only text files are supported`, true); continue; }
    await createFile(file.name, text);
  }
}

const SAMPLE = {
  "index.html": `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>Hello cluster</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <main>
    <h1>Hello from the cluster</h1>
    <p>Click the button, then open the Assistant and ask it to add a feature.</p>
    <button id="go">Count</button>
    <p id="out">0</p>
  </main>
  <script src="app.js"></script>
</body>
</html>
`,
  "style.css": `body { font-family: system-ui, sans-serif; display: grid; place-items: center; min-height: 90vh; }
main { text-align: center; }
button { font-size: 1.1rem; padding: .5rem 1.2rem; }
#out { font-size: 3rem; margin: .5rem; }
`,
  "app.js": `let count = 0;
document.getElementById("go").addEventListener("click", () => {
  count++;
  document.getElementById("out").textContent = count;
  console.log("clicked", count);
});
`,
};

async function createSample() {
  for (const [p, c] of Object.entries(SAMPLE)) {
    if (!code.files.some(f => f.path === p)) await api("PUT", "/api/v1/workspace/file", { path: p, content: c });
  }
  code.files = await api("GET", "/api/v1/workspace/files");
  await openFile("index.html");
}

// ---------- Rendering ----------

function renderFiles() {
  morph($("#code-filelist"), code.files.map(f => `
    <li data-file="${esc(f.path)}" class="${f.path === code.path ? "active" : ""}">
      <button type="button" class="file-btn" data-open="${esc(f.path)}" title="${esc(f.path)}">
        <span class="file-name">${esc(f.path)}</span><span class="file-size">${bytes(f.size)}</span>
      </button>
    </li>`).join("") || `<li class="muted empty-files">No files</li>`);
}

function renderEditorState() {
  const has = !!code.path;
  $("#code-editor").hidden = !has;
  $("#code-empty").hidden = has || !code.loaded;
  $("#code-filename").textContent = has ? code.path : "No file open";
  for (const id of ["#code-rename", "#code-delete", "#code-run"]) $(id).disabled = !has;
  if (!has) setStatus("");
  updateScope();
}

// ---------- Editor ----------

let hlFrame = 0;

function setEditorText(text) {
  const t = ed();
  t.value = text;
  t.setSelectionRange(0, 0);
  $("#editor-scroll").scrollTo(0, 0);
  refreshHighlight();
  setStatus(text === code.saved ? "Saved" : "Unsaved");
}

function refreshHighlight() {
  cancelAnimationFrame(hlFrame);
  hlFrame = requestAnimationFrame(() => {
    const text = ed().value;
    // Trailing newline keeps the last line's height in the overlay.
    $("#editor-hl").innerHTML = codekit.highlight(text, ckLang()) + "\n";
    const n = text.split("\n").length;
    let nums = "";
    for (let i = 1; i <= n; i++) nums += i + "\n";
    $("#editor-gutter").textContent = nums;
  });
}

// insert replaces the textarea range [start, end) with text through the
// browser's editing commands, so Ctrl+Z still works.
function insert(start, end, text, selectAfter) {
  const t = ed();
  t.focus();
  t.setSelectionRange(start, end);
  const ok = document.execCommand && document.execCommand("insertText", false, text);
  if (!ok) {
    t.setRangeText(text, start, end, "end");
    t.dispatchEvent(new Event("input"));
  }
  if (selectAfter) t.setSelectionRange(...selectAfter);
}

const INDENT = "  ";
const COMMENT = { js: "//", go: "//", c: "//", css: null, py: "#", sh: "#", yaml: "#", sql: "--" };

function lineBounds(text, start, end) {
  const ls = text.lastIndexOf("\n", start - 1) + 1;
  let le = text.indexOf("\n", end > start && text[end - 1] === "\n" ? end - 1 : end);
  if (le < 0) le = text.length;
  return [ls, le];
}

function onEditorKey(e) {
  const t = ed();
  const { selectionStart: s, selectionEnd: en, value: v } = t;
  const mod = e.ctrlKey || e.metaKey;
  if (mod && e.key === "s") { e.preventDefault(); flushSave(); return; }
  if (mod && e.key === "Enter") { e.preventDefault(); runPreview(); return; }
  if (e.key === "Escape") { t.blur(); return; }
  if (e.key === "Tab") {
    e.preventDefault();
    if (s === en && !e.shiftKey) return insert(s, en, INDENT);
    const [ls, le] = lineBounds(v, s, en);
    const lines = v.slice(ls, le).split("\n");
    const out = lines.map(l => e.shiftKey ? l.replace(/^( {1,2}|\t)/, "") : INDENT + l).join("\n");
    return insert(ls, le, out, [ls, ls + out.length]);
  }
  if (e.key === "Enter" && !mod && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    const ls = v.lastIndexOf("\n", s - 1) + 1;
    const indent = v.slice(ls, s).match(/^[ \t]*/)[0];
    const before = v.slice(ls, s).trimEnd();
    const extra = /[{([:]$/.test(before) ? INDENT : "";
    const closing = /^[\s]*[})\]]/.test(v.slice(en, v.indexOf("\n", en) < 0 ? v.length : v.indexOf("\n", en)));
    if (extra && closing && /[{([]$/.test(before)) {
      const ins = "\n" + indent + extra + "\n" + indent;
      return insert(s, en, ins, [s + 1 + indent.length + extra.length, s + 1 + indent.length + extra.length]);
    }
    return insert(s, en, "\n" + indent + extra);
  }
  // A closing bracket typed on a blank, indented line steps back one level.
  if ("})]".includes(e.key) && e.key.length === 1 && !mod && !e.altKey && s === en) {
    const ls = v.lastIndexOf("\n", s - 1) + 1;
    const lead = v.slice(ls, s);
    if (lead.length >= INDENT.length && /^[ \t]+$/.test(lead)) {
      e.preventDefault();
      return insert(ls, s, lead.slice(0, lead.length - (lead.endsWith("\t") ? 1 : INDENT.length)) + e.key);
    }
  }
  if (mod && e.key === "/") {
    const mark = COMMENT[ckLang()];
    if (!mark) return;
    e.preventDefault();
    const [ls, le] = lineBounds(v, s, en);
    const lines = v.slice(ls, le).split("\n");
    const allCommented = lines.every(l => !l.trim() || l.trimStart().startsWith(mark));
    const out = lines.map(l => {
      if (!l.trim()) return l;
      if (allCommented) return l.replace(new RegExp("^(\\s*)" + mark.replace(/[/]/g, "\\/") + " ?"), "$1");
      return l.replace(/^(\s*)/, `$1${mark} `);
    }).join("\n");
    return insert(ls, le, out, [ls, ls + out.length]);
  }
}

// ensureCaretVisible scrolls the editor so the caret stays in view.
function ensureCaretVisible() {
  const t = ed();
  const sc = $("#editor-scroll");
  const before = t.value.slice(0, t.selectionEnd);
  const line = before.split("\n").length - 1;
  const col = before.length - before.lastIndexOf("\n") - 1;
  const cs = getComputedStyle(t);
  const lh = parseFloat(cs.lineHeight) || 20;
  const pad = parseFloat(cs.paddingTop) || 0;
  const cw = charWidth();
  const gutter = $("#editor-gutter").offsetWidth;
  const top = pad + line * lh, left = gutter + parseFloat(cs.paddingLeft) + col * cw;
  if (top < sc.scrollTop) sc.scrollTop = top - pad;
  else if (top + lh > sc.scrollTop + sc.clientHeight) sc.scrollTop = top + lh - sc.clientHeight + pad;
  if (left < sc.scrollLeft + gutter) sc.scrollLeft = Math.max(0, left - gutter - 40);
  else if (left + cw > sc.scrollLeft + sc.clientWidth) sc.scrollLeft = left + cw - sc.clientWidth + 40;
}

let cachedCharWidth = 0;
function charWidth() {
  if (!cachedCharWidth) {
    const c = document.createElement("canvas").getContext("2d");
    c.font = getComputedStyle(ed()).font;
    cachedCharWidth = c.measureText("0000000000").width / 10 || 8;
  }
  return cachedCharWidth;
}

// ---------- Assistant ----------

const QUICK = {
  explain: { text: "Explain what this code does, step by step. Mention anything risky or surprising.", edit: false },
  fix: { text: "Find and fix bugs in this code.", edit: true },
  refactor: { text: "Refactor this code for clarity and maintainability without changing its behaviour.", edit: true },
  comment: { text: "Add concise comments and docstrings where they help. Do not change behaviour.", edit: true },
  tests: { text: "Write unit tests for this code using the usual test framework for the language. Return the test file only.", edit: false, newFile: true },
};

function selectionInfo() {
  const t = ed();
  if (!code.path) return null;
  const { selectionStart: s, selectionEnd: e, value: v } = t;
  if (e > s && v.slice(s, e).trim()) {
    const l1 = v.slice(0, s).split("\n").length;
    const l2 = l1 + v.slice(s, e).split("\n").length - 1;
    return { start: s, end: e, text: v.slice(s, e), label: `lines ${l1}–${l2} of ${code.path}`, partial: true };
  }
  return { start: 0, end: v.length, text: v, label: code.path, partial: false };
}

function updateScope() {
  const sel = selectionInfo();
  $("#ai-scope").textContent = sel ? `Context: ${sel.label}` : "No file open: general questions only";
}

function pickAIModel() {
  const sel = $("#ai-model");
  const prev = sel.value || safeStorage("get", "aic.code.model");
  const usable = (chat.models || []).filter(isChatModel);
  sel.innerHTML = usable.length
    ? usable.map(m => `<option value="${esc(m.id)}">${esc(m.id)}${m.aicluster.capabilities.includes("code") ? " · code" : ""}</option>`).join("")
    : `<option value="">No chat models ready</option>`;
  sel.disabled = !usable.length;
  const best = usable.find(m => m.id === prev) || usable.find(m => m.aicluster.capabilities.includes("code")) || usable[0];
  if (best) sel.value = best.id;
}

const AI_ACTIONS = [["review", "Review as change"], ["insert", "Insert at cursor"], ["newfile", "Save as new file"], ["copy", "Copy"]];

function renderAI() {
  const log = $("#ai-log");
  if (!code.ai.length) {
    log.innerHTML = `<div class="ai-empty"><p><strong>Ask about your code.</strong></p>
      <p class="muted">The assistant sees the open file, or just your selection. Proposed changes open as a diff you can accept or reject.</p></div>`;
    return;
  }
  log.innerHTML = code.ai.map((m, i) => `<div class="ai-msg ai-${m.role}${m.error ? " msg-error" : ""}" data-i="${i}">
    ${m.role === "user" ? `<div class="ai-user-text"></div>` : `<div class="md">${m.error ? esc(m.error) : codekit.renderMarkdown(m.content || "…", { codeActions: AI_ACTIONS })}</div>
      ${m.node ? `<div class="msg-meta">answered by <strong>${esc(m.node)}</strong></div>` : ""}`}
  </div>`).join("");
  log.querySelectorAll(".ai-user .ai-user-text").forEach(el => { el.textContent = code.ai[+el.closest(".ai-msg").dataset.i].display; });
  log.scrollTop = log.scrollHeight;
}

let aiFrame = 0;
function renderAISoon() {
  cancelAnimationFrame(aiFrame);
  aiFrame = requestAnimationFrame(renderAI);
}

function systemPrompt(sel) {
  const lang = sel ? (codekit.LANG_NAMES[ckLang()] || "text") : "";
  return [
    "You are a coding assistant built into a code editor.",
    sel ? `The user is working on \`${code.path}\` (${lang}).` : "",
    "When you change code, reply with the complete new version of the code you were given (the whole file, or the whole selection) in ONE fenced code block with a language tag, then at most three short bullet points on what changed.",
    "When asked to explain, answer in Markdown and do not rewrite the code.",
    "Keep answers concise.",
  ].filter(Boolean).join(" ");
}

async function askAI(instruction, opts = {}) {
  if (code.aiController) return;
  const model = $("#ai-model").value;
  if (!model) return toast("No chat model is ready. Install one on the Models page.", true);
  await flushSave();
  const sel = selectionInfo();
  const lang = codekit.langOf(code.path || "");
  const fence = (code.path || "").split(".").pop() || "";
  let content = instruction;
  if (sel) content += `\n\n${sel.partial ? `Selected code (${sel.label})` : `File \`${code.path}\``}:\n\`\`\`${fence}\n${sel.text}\n\`\`\``;

  const display = sel ? `${instruction}\n— ${sel.label}` : instruction;
  const history = code.ai.filter(m => !m.error).slice(-AI_HISTORY).map(m => ({ role: m.role, content: m.content }));
  code.ai.push({ role: "user", content, display });
  const reply = { role: "assistant", content: "" };
  code.ai.push(reply);
  renderAI();

  code.aiController = new AbortController();
  $("#ai-send").textContent = "Stop";
  const snapshot = sel ? { path: code.path, start: sel.start, end: sel.end, original: sel.text, partial: sel.partial, lang } : null;
  try {
    const res = await fetch("/api/v1/playground/chat/completions", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Requested-With": "aicluster" },
      body: JSON.stringify({ model, stream: true, temperature: 0.2, messages: [{ role: "system", content: systemPrompt(sel) }, ...history, { role: "user", content }] }),
      signal: code.aiController.signal,
    });
    if (res.status === 401) throw new AuthError();
    reply.node = res.headers.get("X-AICluster-Node") || "";
    if (!res.ok) {
      const data = await res.json().catch(() => null);
      throw new Error(data?.error?.message || `${res.status} ${res.statusText}`);
    }
    await readSSE(res, chunk => {
      const d = chunk.choices?.[0]?.delta?.content;
      if (d) { reply.content += d; renderAISoon(); }
    });
    if (opts.edit && snapshot) {
      const block = codekit.codeBlocks(reply.content)[0];
      if (block) proposeChange(snapshot, block.code);
    } else if (opts.newFile) {
      const block = codekit.codeBlocks(reply.content)[0];
      if (block) toast("Use “Save as new file” under the code to keep it");
    }
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    if (e.name === "AbortError") reply.content += reply.content ? "\n\n[stopped]" : "[stopped]";
    else reply.error = e.message;
  } finally {
    code.aiController = null;
    $("#ai-send").textContent = "Send";
    renderAI();
  }
}

// ---------- Review (diff) ----------

function proposeChange(snapshot, proposed) {
  if (!code.path || snapshot.path !== code.path) {
    toast("That change was for a different file; open it first", true);
    return;
  }
  const current = ed().value;
  // If the code moved since the request, re-anchor on the original text.
  let start = snapshot.start, end = snapshot.end;
  if (current.slice(start, end) !== snapshot.original) {
    const at = current.indexOf(snapshot.original);
    if (at < 0) return toast("The code changed since you asked; ask again", true);
    start = at;
    end = at + snapshot.original.length;
  }
  if (!snapshot.partial && !proposed.endsWith("\n") && snapshot.original.endsWith("\n")) proposed += "\n";
  const ops = codekit.lineDiff(snapshot.original, proposed);
  if (!ops.some(o => o.op !== " ")) return toast("The assistant returned the code unchanged");
  code.review = { path: code.path, start, end, original: snapshot.original, proposed, lang: snapshot.lang };
  const added = ops.filter(o => o.op === "+").length, removed = ops.filter(o => o.op === "-").length;
  $("#review-title").textContent = `Proposed change to ${snapshot.partial ? "selection in " : ""}${code.path}  (+${added} −${removed})`;
  $("#review-diff").innerHTML = codekit.diffHTML(ops, snapshot.lang);
  $("#code-review").hidden = false;
  $("#code-review").scrollIntoView({ block: "nearest" });
}

function acceptReview() {
  const r = code.review;
  if (!r || r.path !== code.path) return closeReview();
  if (ed().value.slice(r.start, r.end) !== r.original) {
    toast("The file changed under the proposal; ask again", true);
    return closeReview();
  }
  insert(r.start, r.end, r.proposed, [r.start, r.start + r.proposed.length]);
  closeReview();
  toast("Change applied (Ctrl+Z to undo)");
}

function closeReview() {
  code.review = null;
  $("#code-review").hidden = true;
}

// ---------- Code block actions ----------

async function onCodeBlockAction(btn) {
  const block = btn.closest(".codeblock");
  const text = block.querySelector("code").textContent;
  const tag = block.dataset.lang || "";
  switch (btn.dataset.cb) {
    case "copy":
      try { await navigator.clipboard.writeText(text); toast("Copied"); } catch { toast("Copy failed", true); }
      return;
    case "review": {
      const sel = selectionInfo();
      if (!sel) return toast("Open a file first", true);
      return proposeChange({ path: code.path, start: sel.start, end: sel.end, original: sel.text, partial: sel.partial, lang: ckLang() }, text);
    }
    case "insert": {
      if (!code.path) return toast("Open a file first", true);
      const t = ed();
      return insert(t.selectionStart, t.selectionEnd, text);
    }
    case "newfile":
    case "open": {
      if (!code.loaded) await loadWorkspace();
      const name = prompt("Save as:", uniqueName(btn.dataset.cb === "open" ? "snippet" : "new", codekit.extFor(tag)));
      if (!name) return;
      location.hash = "#code";
      return createFile(name, text.endsWith("\n") ? text : text + "\n");
    }
  }
}

// ---------- Preview ----------

async function buildPreviewHTML() {
  await flushSave();
  const lang = ckLang();
  const getFile = async p => (await api("GET", `/api/v1/workspace/file?path=${encodeURIComponent(p)}`)).content;
  let html;
  if (lang === "html") html = ed().value;
  else if (code.files.some(f => f.path === "index.html") && (lang === "js" || lang === "css")) html = await getFile("index.html");
  else if (lang === "js") html = `<!doctype html><html><head><meta charset="utf-8"></head><body><script>\n${ed().value.replace(/<\/script/gi, "<\\/script")}\n<\/script></body></html>`;
  else return null;
  // Inline workspace files referenced by <script src> and <link rel=stylesheet>.
  const names = new Set(code.files.map(f => f.path));
  const scripts = [...html.matchAll(/<script\b[^>]*\bsrc=["']([^"']+)["'][^>]*>\s*<\/script>/gi)];
  for (const m of scripts) {
    const p = m[1].replace(/^\.\//, "");
    if (names.has(p)) html = html.replace(m[0], `<script>\n${(await getFile(p)).replace(/<\/script/gi, "<\\/script")}\n<\/script>`);
  }
  const links = [...html.matchAll(/<link\b[^>]*\bhref=["']([^"']+)["'][^>]*>/gi)].filter(m => /stylesheet/i.test(m[0]));
  for (const m of links) {
    const p = m[1].replace(/^\.\//, "");
    if (names.has(p)) html = html.replace(m[0], `<style>\n${(await getFile(p)).replace(/<\/style/gi, "<\\/style")}\n</style>`);
  }
  return html;
}

async function runPreview() {
  if (!code.path) return;
  let html;
  try {
    html = await buildPreviewHTML();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    return toast(e.message, true);
  }
  if (html == null) {
    return toast("Run works for HTML, CSS and JavaScript. Copy other languages to a machine with their runtime.", true);
  }
  $("#code-preview").hidden = false;
  $("#pv-console").innerHTML = "";
  // A fresh frame per run, so nothing leaks between runs.
  const host = $("#pv-host");
  host.innerHTML = "";
  const frame = document.createElement("iframe");
  // Isolation comes from /preview's own CSP "sandbox" header (opaque origin,
  // no network, no cookies), which browsers apply like the iframe attribute.
  // The attribute itself is left off: some embedded browsers refuse to load
  // sandboxed frames at all. Every other Controller response forbids framing,
  // so the frame can't be steered to an unsandboxed page.
  frame.title = "Preview of " + code.path;
  frame.referrerPolicy = "no-referrer";
  frame.src = "/preview";
  code.pvReady = { frame, html };
  host.append(frame);
}

function onPreviewMessage(e) {
  const pv = code.pvReady;
  if (!pv || e.source !== pv.frame.contentWindow || !e.data || e.data.__aicPreview !== 1) return;
  if (e.data.ready) {
    pv.frame.contentWindow.postMessage({ type: "run", html: pv.html }, "*");
    return;
  }
  const line = document.createElement("div");
  line.className = `pv-line pv-${["log", "info", "warn", "error"].includes(e.data.level) ? e.data.level : "log"}`;
  line.textContent = String(e.data.text).slice(0, 2000);
  const c = $("#pv-console");
  c.append(line);
  while (c.childElementCount > 500) c.firstChild.remove();
  c.scrollTop = c.scrollHeight;
}

// ---------- Panes (narrow screens) ----------

function showPane(name) {
  $("#code-layout").dataset.pane = name;
  for (const b of document.querySelectorAll("[data-pane]")) {
    if (b.tagName === "BUTTON") b.setAttribute("aria-pressed", String(b.dataset.pane === name));
  }
}

// ---------- Wiring ----------

hooks.route.push(view => {
  if (view !== "code") { flushSave(); return; }
  const work = [code.loaded ? Promise.resolve() : loadWorkspace(), loadChatModels(true).then(pickAIModel)];
  Promise.all(work).catch(e => { if (e instanceof AuthError) showLogin(); else toast(e.message, true); });
  renderAI();
});
hooks.refresh.push(async () => {
  if (!$("#view-code").hidden && !code.aiController) { await loadChatModels(false); pickAIModel(); }
});
hooks.wire.push(() => {
  const t = ed();
  t.addEventListener("input", () => { refreshHighlight(); scheduleSave(); updateScope(); });
  t.addEventListener("keydown", onEditorKey);
  t.addEventListener("keyup", () => { ensureCaretVisible(); updateScope(); });
  t.addEventListener("mouseup", updateScope);
  t.addEventListener("select", updateScope);
  $("#code-new").addEventListener("click", () => createFile(prompt("New file name (e.g. main.py, src/app.js):", "")));
  $("#code-new2").addEventListener("click", () => createFile(prompt("New file name (e.g. main.py, src/app.js):", "")));
  $("#code-sample").addEventListener("click", () => createSample().catch(e => toast(e.message, true)));
  $("#code-upload").addEventListener("change", e => { uploadFiles([...e.target.files]); e.target.value = ""; });
  $("#code-rename").addEventListener("click", renameFile);
  $("#code-delete").addEventListener("click", deleteFile);
  $("#code-run").addEventListener("click", runPreview);
  $("#pv-rerun").addEventListener("click", runPreview);
  $("#pv-close").addEventListener("click", () => { $("#code-preview").hidden = true; $("#pv-host").innerHTML = ""; code.pvReady = null; });
  $("#review-accept").addEventListener("click", acceptReview);
  $("#review-reject").addEventListener("click", closeReview);
  $("#ai-model").addEventListener("change", e => safeStorage("set", "aic.code.model", e.target.value));
  $("#ai-clear").addEventListener("click", () => { code.aiController?.abort(); code.ai = []; renderAI(); });
  $("#ai-form").addEventListener("submit", e => {
    e.preventDefault();
    if (code.aiController) return code.aiController.abort();
    const text = $("#ai-input").value.trim();
    if (!text) return;
    $("#ai-input").value = "";
    askAI(text, { edit: /\b(change|fix|add|remove|rename|refactor|rewrite|convert|update|implement|make)\b/i.test(text) });
  });
  $("#ai-input").addEventListener("keydown", e => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); $("#ai-form").requestSubmit(); }
  });
  window.addEventListener("message", onPreviewMessage);
  window.addEventListener("beforeunload", () => {
    if (code.path && ed().value !== code.saved) {
      fetch("/api/v1/workspace/file", {
        method: "PUT", keepalive: true, credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-Requested-With": "aicluster" },
        body: JSON.stringify({ path: code.path, content: ed().value }),
      });
    }
  });
  document.addEventListener("click", e => {
    const open = e.target.closest("[data-open]");
    if (open) { openFile(open.dataset.open).catch(err => toast(err.message, true)); showPane("editor"); return; }
    const q = e.target.closest("[data-ai]");
    if (q) { const a = QUICK[q.dataset.ai]; askAI(a.text, a); return; }
    const cb = e.target.closest("[data-cb]");
    if (cb) { onCodeBlockAction(cb); return; }
    const pane = e.target.closest("button[data-pane]");
    if (pane) showPane(pane.dataset.pane);
  });
});
