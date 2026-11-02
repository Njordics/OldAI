// Memory page: what the cluster remembers for each memory space, its
// backup, and the banner shown after a restore (protocol §18). Uses helpers
// from app.js ($, api, esc, morph, toast, ago...).
"use strict";

const mem = {
  view: null,     // GET /api/v1/memory
  space: "default",
  q: "",
  notice: null,
  noticeAt: 0,
};

const SOURCE = { you: "you", learned: "learned", restored: "restored" };

async function loadMemory() {
  mem.view = await api("GET", `/api/v1/memory?space=${encodeURIComponent(mem.space)}`);
  mem.notice = mem.view.notice || null;
}

function memRow(m) {
  const tier = m.kind === "fact" ? `<span class="pill ${m.tier === "cold" ? "plain" : "s-adopted"}" title="${m.tier === "cold" ? "Not used for 30 days: only brought in when a question clearly matches" : "Brought in whenever it's relevant"}">${m.tier === "cold" ? "Cold" : "Hot"}</span>` : "";
  return `<li class="mem-row">
    <div class="mem-text">${esc(m.text)}
      <div class="muted">${tier} ${m.pinned ? `<span class="pill s-enrolling">Pinned</span>` : ""} ${esc(SOURCE[m.source] || m.source || "")} · ${m.uses ? `used ${m.uses}×, last ${ago(m.last_used_at)}` : `added ${ago(m.created_at)}`}</div>
    </div>
    <div class="mem-actions">
      ${m.kind === "fact" ? `<button class="btn btn-sm btn-ghost" type="button" data-act="mem-pin" data-id="${esc(m.id)}" title="${m.pinned ? "Stop including it in every chat" : "Include it in every chat"}">${m.pinned ? "Unpin" : "Pin"}</button>` : ""}
      <button class="btn btn-sm btn-ghost" type="button" data-act="mem-edit" data-id="${esc(m.id)}">Edit</button>
      <button class="btn btn-sm btn-ghost" type="button" data-act="mem-delete" data-id="${esc(m.id)}">Forget</button>
    </div>
  </li>`;
}

function renderMemory() {
  const v = mem.view;
  if (!v) return;
  $("#mem-on").checked = v.enabled;
  $("#mem-off-note").hidden = v.enabled;
  const sel = $("#mem-space");
  sel.innerHTML = v.spaces.map(sp => `<option value="${esc(sp.name)}" ${sp.name === v.space ? "selected" : ""}>${esc(sp.name)} (${sp.memories})</option>`).join("")
    + `<option value="__new">New space…</option>`;
  const sp = v.spaces.find(x => x.name === v.space);
  $("#mem-space-keys").textContent = sp?.keys?.length ? `Used by: ${sp.keys.join(", ")}` : "No API key uses this space yet; pick it for a key on the Connect page.";

  const q = mem.q.trim().toLowerCase();
  const match = m => !q || m.text.toLowerCase().includes(q);
  const instr = v.memories.filter(m => m.kind === "instruction" && match(m));
  const facts = v.memories.filter(m => m.kind === "fact" && match(m))
    .sort((a, b) => (b.pinned - a.pinned) || (a.tier === b.tier ? 0 : a.tier === "hot" ? -1 : 1) || b.created_at.localeCompare(a.created_at));
  morph($("#mem-instructions"), instr.map(memRow).join("") || `<li class="muted mem-empty">None. Instructions go into every chat, e.g. "Keep answers short" or "Use metric units".</li>`);
  morph($("#mem-facts"), facts.map(memRow).join("") || `<li class="muted mem-empty">${q ? "Nothing matches." : `Nothing yet. Say "remember that…" in a chat, add one here, or just chat: a small model picks out lasting facts.`}</li>`);
  const hot = v.memories.filter(m => m.kind === "fact" && m.tier !== "cold").length;
  $("#mem-counts").textContent = `${hot} hot, ${v.memories.filter(m => m.kind === "fact").length - hot} cold`;

  // Backup.
  const b = v.backup;
  const bsel = $("#mem-backup-node");
  bsel.innerHTML = `<option value="">Automatic (the node with most free disk)</option>` +
    v.nodes.map(n => `<option value="${esc(n.node_id)}" ${n.node_id === b.configured ? "selected" : ""}>${esc(n.name)}${n.state !== "adopted" ? " (offline)" : ""}</option>`).join("");
  let status;
  if (b.error) status = `<span class="bad-text">${esc(b.error)}</span>`;
  else if (b.at && !b.at.startsWith("0001")) status = `${b.count} memories copied to <strong>${esc(b.node_name)}</strong> ${ago(b.at)}${b.generation < b.local_generation ? ` <span class="muted">(newer changes go over within a minute)</span>` : ""}`;
  else status = b.local_generation ? "Not backed up yet." : "Nothing to back up yet.";
  $("#mem-backup-status").innerHTML = status;

  // Learning model.
  const models = [...new Set((typeof ms !== "undefined" ? ms.assignments : []).map(a => a.model_id))].sort();
  const msel = $("#mem-model");
  msel.innerHTML = `<option value="">Automatic (the smallest chat model)</option>` +
    models.map(m => `<option value="${esc(m)}" ${m === v.model ? "selected" : ""}>${esc(m)}</option>`).join("");
  renderMemNotice();
}

function renderMemNotice() {
  const n = mem.notice;
  const el = $("#mem-banner");
  if (!n) { el.hidden = true; return; }
  el.hidden = false;
  const when = new Date(n.saved_at).toLocaleString();
  if (n.kind === "restored") {
    el.className = "banner warn mem-banner";
    morph(el, `<div><strong>Memory was restored from backup.</strong> ${esc(n.reason ? n.reason[0].toUpperCase() + n.reason.slice(1) : "")}. ${n.count} memories came back from the copy on ${esc(n.node_name)}, saved ${esc(when)}${n.before ? `; the Controller had ${n.before} before` : ""}. Anything changed after that copy is not included.</div>
      <div class="mem-banner-actions"><a class="btn btn-sm" href="#memory">See memory</a><button class="btn btn-sm btn-ghost" type="button" data-act="mem-dismiss">Dismiss</button></div>`);
  } else {
    el.className = "banner info mem-banner";
    morph(el, `<div><strong>A memory backup from an earlier Controller was found</strong> on ${esc(n.node_name)}: ${n.count} memories, saved ${esc(when)}. This Controller has none. Restore it if it's yours.</div>
      <div class="mem-banner-actions"><button class="btn btn-sm btn-primary" type="button" data-act="mem-restore" data-id="${esc(n.node_id)}">Restore</button><button class="btn btn-sm btn-ghost" type="button" data-act="mem-dismiss">Not now</button></div>`);
  }
}

async function memCall(fn, ok) {
  try {
    await fn();
    if (ok) toast(ok);
    await loadMemory();
    renderMemory();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

function memByID(id) { return mem.view?.memories.find(m => m.id === id); }

async function addMemory(kind) {
  const input = $(kind === "instruction" ? "#mem-add-instruction" : "#mem-add-fact");
  const text = input.value.trim();
  if (!text) return;
  await memCall(() => api("POST", "/api/v1/memory", { space: mem.space, kind, text }), "Remembered");
  input.value = "";
}

actions["mem-edit"] = id => {
  const m = memByID(id);
  if (!m) return;
  const text = prompt("Edit memory", m.text);
  if (text === null || !text.trim() || text.trim() === m.text) return;
  memCall(() => api("PUT", `/api/v1/memory/${encodeURIComponent(id)}`, { text: text.trim(), kind: m.kind, pinned: m.pinned }), "Saved");
};
actions["mem-pin"] = id => {
  const m = memByID(id);
  if (m) memCall(() => api("PUT", `/api/v1/memory/${encodeURIComponent(id)}`, { text: m.text, kind: m.kind, pinned: !m.pinned }));
};
actions["mem-delete"] = id => memCall(() => api("DELETE", `/api/v1/memory/${encodeURIComponent(id)}`), "Forgotten");
actions["mem-dismiss"] = () => {
  mem.notice = null;
  renderMemNotice();
  api("POST", "/api/v1/memory/notice/dismiss").catch(() => {});
};
actions["mem-restore"] = id => {
  if (!confirm("Replace this Controller's memory with the backup?")) return;
  memCall(() => api("POST", "/api/v1/memory/restore", { node_id: id }), "Memory restored");
};

hooks.route.push(view => {
  if (view === "memory") loadMemory().then(renderMemory).catch(e => { if (e instanceof AuthError) showLogin(); else toast(e.message, true); });
});
hooks.refresh.push(async () => {
  if (!$("#view-memory").hidden) {
    // Typing in the add boxes shouldn't be disturbed: only redraw lists.
    await loadMemory();
    renderMemory();
    return;
  }
  // Elsewhere, only the banner matters; check it every half minute.
  if (Date.now() - mem.noticeAt > 30000) {
    mem.noticeAt = Date.now();
    try {
      mem.notice = (await api("GET", "/api/v1/memory/notice")).notice || null;
    } catch (e) {
      if (e instanceof AuthError) throw e;
    }
    renderMemNotice();
  }
});
hooks.wire.push(() => {
  $("#mem-on").addEventListener("change", e => memCall(() => api("PUT", "/api/v1/memory/settings", { enabled: e.target.checked }),
    e.target.checked ? "Memory is on" : "Memory is off: chats get none, and nothing new is learned"));
  $("#mem-space").addEventListener("change", e => {
    let v = e.target.value;
    if (v === "__new") {
      v = (prompt("Name of the new memory space (lowercase letters, digits, . _ -), e.g. alex") || "").trim().toLowerCase();
      if (!/^[a-z0-9][a-z0-9._-]{0,39}$/.test(v)) { renderMemory(); if (v) toast("Use lowercase letters, digits, . _ -", true); return; }
    }
    mem.space = v;
    memCall(async () => {});
  });
  $("#mem-search").addEventListener("input", e => { mem.q = e.target.value; renderMemory(); });
  $("#mem-instruction-form").addEventListener("submit", e => { e.preventDefault(); addMemory("instruction"); });
  $("#mem-fact-form").addEventListener("submit", e => { e.preventDefault(); addMemory("fact"); });
  $("#mem-forget-all").addEventListener("click", () => {
    if (!confirm(`Forget everything in the "${mem.space}" memory space? The backup keeps the previous copy until the next one replaces it.`)) return;
    memCall(() => api("POST", "/api/v1/memory/forget-space", { space: mem.space }), "Forgotten");
  });
  $("#mem-backup-node").addEventListener("change", e => memCall(() => api("PUT", "/api/v1/memory/settings", { backup_node: e.target.value }), "Backup node saved"));
  $("#mem-backup-now").addEventListener("click", () => memCall(() => api("POST", "/api/v1/memory/backup"), "Backed up"));
  $("#mem-model").addEventListener("change", e => memCall(() => api("PUT", "/api/v1/memory/settings", { model: e.target.value }), "Saved"));
});
