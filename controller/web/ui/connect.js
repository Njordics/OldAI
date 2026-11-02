// Connect page: gateway address, API keys, and copy-paste setup for common
// tools.
"use strict";

const conn = {
  keys: [],
  snippet: "python",
  loadedAt: 0,
};

function gatewayURL() {
  const urls = state.cluster?.gateway_urls || [];
  const sel = $("#connect-url");
  return sel.value || urls.find(u => new URL(u).hostname === location.hostname) || urls[0] || "http://<controller>:8080/v1";
}

function exampleModel() {
  const chatty = (chat?.models || []).find(m => typeof isChatModel === "function" && isChatModel(m));
  return chatty?.id || ms?.assignments?.[0]?.model_id || "qwen2.5:7b";
}

const SNIPPETS = {
  python: {
    label: "Python",
    note: "pip install openai",
    code: (url, key, model) => `from openai import OpenAI

client = OpenAI(base_url="${url}", api_key="${key}")

reply = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "Write a haiku about home servers."}],
)
print(reply.choices[0].message.content)`,
  },
  js: {
    label: "JavaScript",
    note: "npm install openai",
    code: (url, key, model) => `import OpenAI from "openai";

const client = new OpenAI({ baseURL: "${url}", apiKey: "${key}" });

const reply = await client.chat.completions.create({
  model: "${model}",
  messages: [{ role: "user", content: "Write a haiku about home servers." }],
});
console.log(reply.choices[0].message.content);`,
  },
  curl: {
    label: "curl",
    note: "Add \"stream\": true for token-by-token output.",
    code: (url, key, model) => `curl ${url}/chat/completions \\
  -H "Authorization: Bearer ${key}" \\
  -H "Content-Type: application/json" \\
  -d '{"model": "${model}", "messages": [{"role": "user", "content": "Hello!"}]}'`,
  },
  continue: {
    label: "VS Code (Continue)",
    note: "Add to ~/.continue/config.yaml. Cline, Roo Code and JetBrains AI Assistant take the same base URL and key under \"OpenAI Compatible\".",
    code: (url, key, model) => `models:
  - name: Cluster ${model}
    provider: openai
    model: ${model}
    apiBase: ${url}
    apiKey: ${key}
    roles: [chat, edit, apply]`,
  },
  openwebui: {
    label: "Open WebUI",
    note: "Admin Panel → Settings → Connections → OpenAI API → add a connection. Or set these when starting the container:",
    code: (url, key) => `OPENAI_API_BASE_URL=${url}
OPENAI_API_KEY=${key}`,
  },
  aider: {
    label: "Aider",
    note: "Prefix the model with openai/ so Aider uses the OpenAI-compatible client.",
    code: (url, key, model) => `export OPENAI_API_BASE=${url}
export OPENAI_API_KEY=${key}
aider --model openai/${model}`,
  },
};

function renderSnippet() {
  morph($("#snippet-tabs"), Object.entries(SNIPPETS).map(([k, s]) =>
    `<button class="chip" type="button" role="tab" data-snippet="${k}" aria-pressed="${conn.snippet === k}">${esc(s.label)}</button>`).join(""));
  const s = SNIPPETS[conn.snippet];
  const key = $("#key-secret").textContent || "aic_YOUR_API_KEY";
  $("#snippet").textContent = s.code(gatewayURL(), key, exampleModel());
  $("#snippet-note").textContent = s.note;
}

function renderConnect() {
  const urls = state.cluster?.gateway_urls || [];
  const sel = $("#connect-url");
  const prev = sel.value;
  sel.innerHTML = urls.length ? urls.map(u => `<option>${esc(u)}</option>`).join("") : `<option value="">Gateway is not running</option>`;
  if (urls.includes(prev)) sel.value = prev;
  else sel.value = urls.find(u => new URL(u).hostname === location.hostname) || urls[0] || "";
  $("#connect-example-model").textContent = exampleModel();

  morph($("#keys-list"), conn.keys.length ? `<table class="keys">
    <thead><tr><th>Name</th><th>Key</th><th class="hide-sm">Models</th><th class="hide-sm">Limit</th><th class="hide-sm">Memory</th><th>Last used</th><th><span class="sr-only">Actions</span></th></tr></thead>
    <tbody>${conn.keys.map(k => `<tr data-key="${esc(k.id)}">
      <td><strong>${esc(k.name)}</strong><div class="muted">created ${ago(k.created_at)}</div></td>
      <td class="mono">aic_${esc(k.prefix)}…</td>
      <td class="hide-sm">${k.models.length ? k.models.map(esc).join(", ") : `<span class="muted">all</span>`}</td>
      <td class="hide-sm">${k.rpm ? `${k.rpm}/min` : `<span class="muted">none</span>`}</td>
      <td class="hide-sm"><button class="btn btn-sm btn-ghost" type="button" data-act="key-memory" data-id="${esc(k.id)}" title="Change the memory space">${k.memory ? esc(k.memory) : `<span class="muted">none</span>`}</button></td>
      <td>${ago(k.last_used_at)}</td>
      <td class="actions"><button class="btn btn-sm btn-ghost" type="button" data-act="revoke-key" data-id="${esc(k.id)}">Revoke</button></td>
    </tr>`).join("")}</tbody></table>`
    : `<p class="muted">No keys yet. Create one below for each app or person, so you can revoke them separately.</p>`);

  const installed = [...new Set((ms?.assignments || []).map(a => a.model_id))].sort();
  const box = $("#key-models");
  const checked = new Set([...box.querySelectorAll("input:checked")].map(i => i.value));
  box.innerHTML = installed.length
    ? installed.map(m => `<label class="check"><input type="checkbox" value="${esc(m)}" ${checked.has(m) ? "checked" : ""}> ${esc(m)}</label>`).join("")
    : `<p class="muted">No models installed yet.</p>`;
  renderSnippet();
}

async function loadKeys() {
  conn.keys = await api("GET", "/api/v1/keys");
  conn.loadedAt = Date.now();
}

async function createKey() {
  const err = $("#key-error");
  err.hidden = true;
  const all = $("#key-all").checked;
  const models = all ? [] : [...$("#key-models").querySelectorAll("input:checked")].map(i => i.value);
  if (!all && !models.length) {
    err.textContent = "Pick at least one model, or allow all models.";
    err.hidden = false;
    return;
  }
  try {
    const k = await api("POST", "/api/v1/keys", {
      name: $("#key-name").value.trim(), models, rpm: parseInt($("#key-rpm").value, 10) || 0,
      memory: $("#key-memory").value.trim().toLowerCase(),
    });
    $("#key-secret").textContent = k.key;
    $("#key-reveal").hidden = false;
    $("#key-name").value = "";
    await loadKeys();
    renderConnect();
    $("#key-reveal").scrollIntoView({ block: "nearest" });
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    err.textContent = e.message;
    err.hidden = false;
  }
}

async function revokeKey(id) {
  const k = conn.keys.find(x => x.id === id);
  if (!k || !confirm(`Revoke the key "${k.name}"? Apps using it stop working immediately.`)) return;
  try {
    await api("DELETE", `/api/v1/keys/${encodeURIComponent(id)}`);
    toast(`Revoked "${k.name}"`);
    await loadKeys();
    renderConnect();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

async function copyFrom(sel) {
  const el = $(sel);
  const text = el.tagName === "SELECT" ? el.value : el.textContent;
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied");
  } catch {
    toast("Copy failed; select the text and copy it manually", true);
  }
}

actions["revoke-key"] = id => revokeKey(id);
actions["key-memory"] = async id => {
  const k = conn.keys.find(x => x.id === id);
  const v = prompt(`Memory space for "${k?.name}" (empty for none)`, k?.memory || "");
  if (v === null) return;
  try {
    await api("PUT", `/api/v1/keys/${encodeURIComponent(id)}/memory`, { space: v.trim().toLowerCase() });
    await loadKeys();
    renderConnect();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
};
hooks.route.push(view => {
  if (view === "connect") {
    loadKeys().then(renderConnect).catch(e => { if (e instanceof AuthError) showLogin(); });
    if (typeof loadChatModels === "function") loadChatModels(true).then(renderSnippet).catch(() => {});
  }
});
hooks.refresh.push(async () => {
  if ($("#view-connect").hidden) return;
  if (Date.now() - conn.loadedAt > 10000) await loadKeys();
  renderConnect();
});
hooks.wire.push(() => {
  $("#key-form").addEventListener("submit", e => { e.preventDefault(); createKey(); });
  $("#key-all").addEventListener("change", e => { $("#key-models").hidden = e.target.checked; });
  $("#connect-url").addEventListener("change", renderSnippet);
  document.addEventListener("click", e => {
    const c = e.target.closest("[data-copy]");
    if (c) { copyFrom(c.dataset.copy); return; }
    const t = e.target.closest("[data-snippet]");
    if (t) { conn.snippet = t.dataset.snippet; renderSnippet(); }
  });
});
