// Chat playground: talks to installed models through the gateway (as the
// signed-in admin), streaming tokens as they arrive.
"use strict";

const chat = {
  models: [],
  messages: [],   // {role, content, node?, ms?, tokens?, error?}
  controller: null, // AbortController of the running request
  loadedAt: 0,
};

const isChatModel = m => !(m.aicluster.capabilities.length === 1 && m.aicluster.capabilities[0] === "embeddings");

async function loadChatModels(force) {
  if (!force && Date.now() - chat.loadedAt < 5000) return;
  chat.loadedAt = Date.now();
  try {
    chat.models = await api("GET", "/api/v1/playground/models");
  } catch (e) {
    if (e instanceof AuthError) throw e;
    chat.models = [];
  }
  renderChatModels();
}

function renderChatModels() {
  const sel = $("#chat-model");
  const prev = sel.value;
  const usable = chat.models.filter(isChatModel);
  sel.innerHTML = usable.length
    ? usable.map(m => `<option value="${esc(m.id)}">${esc(m.id)} · ${m.aicluster.replicas} node${m.aicluster.replicas === 1 ? "" : "s"}</option>`).join("")
    : `<option value="">No chat models ready</option>`;
  if (usable.some(m => m.id === prev)) sel.value = prev;
  sel.disabled = !usable.length;
  $("#chat-send").disabled = !usable.length && !chat.controller;
  if (!chat.messages.length) renderChat();
}

function renderChat() {
  const log = $("#chat-log");
  if (!chat.messages.length) {
    const ready = chat.models.some(isChatModel);
    log.innerHTML = `<div class="chat-empty"><h2>${ready ? "Try a model" : "No chat models are ready"}</h2>
      <p class="muted">${ready ? "Pick a model above and send a message." : `Install one on the <a href="#models">Models</a> page; it appears here once a node has loaded it.`}</p></div>`;
    return;
  }
  log.innerHTML = chat.messages.map((m, i) => `
    <div class="msg msg-${m.role}" data-i="${i}">
      <div class="msg-body"></div>
      ${m.role === "assistant" ? `<div class="msg-meta">${metaText(m)}</div>` : ""}
    </div>`).join("");
  log.querySelectorAll(".msg").forEach(el => {
    const m = chat.messages[+el.dataset.i];
    fillBody(el.querySelector(".msg-body"), m);
    if (m.error) el.classList.add("msg-error");
  });
  log.scrollTop = log.scrollHeight;
}

// User text is shown as typed; assistant replies render as Markdown through
// codekit, which escapes everything, so model output can't inject markup.
function fillBody(el, m) {
  if (m.role === "assistant" && m.content) {
    el.classList.add("md");
    el.innerHTML = codekit.renderMarkdown(m.content);
  } else {
    el.textContent = m.content || (m.role === "assistant" && !m.error && chat.controller ? "…" : "");
  }
}

function metaText(m) {
  if (m.error) return esc(m.error);
  const parts = [];
  if (m.model) parts.push(esc(m.model));
  if (m.node) parts.push(`answered by <strong>${esc(m.node)}</strong>`);
  if (m.ms) parts.push(`${(m.ms / 1000).toFixed(1)} s`);
  if (m.tokens && m.ms) parts.push(`${(m.tokens / (m.ms / 1000)).toFixed(0)} chunks/s`);
  return parts.join(" · ");
}

// updateLast re-renders the streaming reply, at most once per frame.
let lastFrame = 0;
function updateLast() {
  cancelAnimationFrame(lastFrame);
  lastFrame = requestAnimationFrame(updateLastNow);
}

function updateLastNow() {
  const el = $("#chat-log .msg:last-child");
  const m = chat.messages[chat.messages.length - 1];
  if (!el || !m) return renderChat();
  fillBody(el.querySelector(".msg-body"), m);
  const meta = el.querySelector(".msg-meta");
  if (meta) meta.innerHTML = metaText(m);
  if (m.error) el.classList.add("msg-error");
  const log = $("#chat-log");
  if (log.scrollHeight - log.scrollTop - log.clientHeight < 120) log.scrollTop = log.scrollHeight;
}

async function sendChat() {
  if (chat.controller) { chat.controller.abort(); return; }
  const input = $("#chat-input");
  const text = input.value.trim();
  const model = $("#chat-model").value;
  if (!text || !model) return;
  input.value = "";

  chat.messages.push({ role: "user", content: text });
  const reply = { role: "assistant", content: "", model };
  chat.messages.push(reply);
  chat.controller = new AbortController();
  $("#chat-send").textContent = "Stop";
  renderChat();

  const history = chat.messages.filter(m => m !== reply && !m.error).map(m => ({ role: m.role, content: m.content }));
  const system = $("#chat-system").value.trim();
  if (system) history.unshift({ role: "system", content: system });
  const body = { model, messages: history, stream: true, temperature: parseFloat($("#chat-temp").value) || 0.7 };

  const t0 = performance.now();
  try {
    const res = await fetch("/api/v1/playground/chat/completions", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Requested-With": "aicluster" },
      body: JSON.stringify(body),
      signal: chat.controller.signal,
    });
    if (res.status === 401) throw new AuthError();
    reply.node = res.headers.get("X-AICluster-Node") || "";
    if (!res.ok) {
      const data = await res.json().catch(() => null);
      throw new Error(data?.error?.message || `${res.status} ${res.statusText}`);
    }
    await readSSE(res, chunk => {
      const delta = chunk.choices?.[0]?.delta?.content;
      if (delta) {
        reply.content += delta;
        reply.tokens = (reply.tokens || 0) + 1;
        reply.ms = performance.now() - t0;
        updateLast();
      }
    });
    reply.ms = performance.now() - t0;
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    if (e.name === "AbortError") {
      reply.content += reply.content ? " [stopped]" : "[stopped]";
    } else {
      reply.error = e.message;
    }
  } finally {
    chat.controller = null;
    $("#chat-send").textContent = "Send";
    updateLast();
    input.focus();
  }
}

// readSSE parses an OpenAI-style server-sent-event stream.
async function readSSE(res, onChunk) {
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    let i;
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const event = buf.slice(0, i);
      buf = buf.slice(i + 2);
      for (const line of event.split("\n")) {
        const data = line.startsWith("data: ") ? line.slice(6) : null;
        if (!data || data === "[DONE]") continue;
        try { onChunk(JSON.parse(data)); } catch { /* ignore keep-alives */ }
      }
    }
  }
}

hooks.route.push(view => {
  if (view === "chat") {
    loadChatModels(true).catch(e => { if (e instanceof AuthError) showLogin(); });
    setTimeout(() => $("#chat-input").focus(), 0);
  }
});
hooks.refresh.push(async () => {
  if (!$("#view-chat").hidden) await loadChatModels(false);
});
hooks.wire.push(() => {
  $("#chat-form").addEventListener("submit", e => { e.preventDefault(); sendChat(); });
  $("#chat-input").addEventListener("keydown", e => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); sendChat(); }
  });
  $("#chat-new").addEventListener("click", () => {
    chat.controller?.abort();
    chat.messages = [];
    renderChat();
    $("#chat-input").focus();
  });
  $("#chat-settings-toggle").addEventListener("click", e => {
    const box = $("#chat-settings");
    box.hidden = !box.hidden;
    e.currentTarget.setAttribute("aria-expanded", String(!box.hidden));
  });
  renderChat();
});
