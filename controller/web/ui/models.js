// Models page: what's installed where, the catalog, and install/swap/remove.
// Uses helpers from app.js ($, api, esc, morph, bytes, toast, nodeName...).
"use strict";

const CATALOG_TTL_MS = 10000;
const CAPS = [["all", "All"], ["chat", "Chat"], ["code", "Code"], ["reasoning", "Reasoning"], ["vision", "Vision"], ["embeddings", "Embeddings"]];
const PLACEMENT = { gpu: "GPU", hybrid: "GPU + RAM", cpu: "CPU" };

const ms = {
  assignments: [],
  catalog: null,
  catalogAt: 0,
  catalogError: "",
  cap: "all",
  q: "",
  install: null, // {modelId}
  picker: null,  // {mode: "swap"|"add", nodeId, assignment, options, choice}
};

// ---------- Data ----------

async function refreshModels() {
  try {
    ms.assignments = await api("GET", "/api/v1/models");
  } catch (e) {
    if (e instanceof AuthError) throw e;
  }
  if (Date.now() - ms.catalogAt > CATALOG_TTL_MS) {
    try {
      ms.catalog = await api("GET", "/api/v1/catalog");
      ms.catalogError = "";
    } catch (e) {
      if (e instanceof AuthError) throw e;
      ms.catalogError = e.message;
    }
    ms.catalogAt = Date.now();
  }
  renderModels();
}

function variants() {
  const out = [];
  for (const f of ms.catalog?.families || []) for (const v of f.variants) out.push({ family: f, v });
  return out;
}

function variantById(id) {
  return variants().find(x => x.v.id === id);
}

function servingNodes() {
  return state.nodes.filter(n => n.state === "adopted" || n.state === "offline");
}

function assignmentsOn(nodeId) {
  return ms.assignments.filter(a => a.node_id === nodeId);
}

// ---------- Status text ----------

function statusOf(a) {
  const pct = a.bytes_total ? ` · ${Math.floor(a.progress * 100)}%` : "";
  const of = a.bytes_total ? ` <span class="muted">(${bytes(a.bytes_done)} of ${bytes(a.bytes_total)})</span>` : "";
  switch (a.state) {
    case "fetching": return { cls: "s-enrolling", label: "Downloading to Controller" + pct, detail: of, bar: true };
    case "pending": return { cls: "s-discovered", label: "Waiting for node", detail: a.error ? `<span class="note bad">${esc(a.error)}</span>` : "" };
    case "downloading": return { cls: "s-enrolling", label: "Copying to node" + pct, detail: of, bar: true };
    case "installed": return a.residency === "on_demand"
      ? { cls: "s-adopted", label: "On disk", detail: `<span class="muted">loads on first request</span>` }
      : { cls: "s-enrolling", label: "On disk, loading" };
    case "loading": return { cls: "s-enrolling", label: "Loading into memory" };
    case "loaded": return { cls: "s-adopted", label: "Ready" };
    case "offline": return { cls: "s-offline", label: "Node offline" };
    case "failed": return { cls: "s-offline", label: a.stage === "controller" ? "Download failed" : "Failed", detail: `<span class="note bad">${esc(a.error || "")}</span>` };
    default: return { cls: "s-discovered", label: a.state };
  }
}

function fitText(f) {
  if (!f) return "";
  if (!f.fits) return `<span class="note bad">${esc(f.reason)}</span>`;
  return `${esc(f.quant)} · ${PLACEMENT[f.placement] || f.placement} · ${bytes(f.need)}`;
}

// ---------- Installed ----------

function renderInstalled() {
  const groups = new Map();
  for (const a of ms.assignments) {
    if (!groups.has(a.model_id)) groups.set(a.model_id, []);
    groups.get(a.model_id).push(a);
  }
  const copies = copiesOf(ms.assignments);
  const ready = copies.filter(c => c.every(a => a.state === "loaded" || (a.state === "installed" && a.residency === "on_demand"))).length;
  $("#models-summary").textContent = copies.length
    ? `${groups.size} model${groups.size === 1 ? "" : "s"} · ${ready} of ${copies.length} copies ready`
    : "";

  if (!groups.size) {
    morph($("#installed"), `<div class="empty-card"><h2>No models installed yet</h2>
      <p class="muted">${servingNodes().length ? "Pick one from the catalog below and press Install." : "Adopt a node first; models are installed onto nodes."}</p></div>`);
    return;
  }
  morph($("#installed"), [...groups].map(([modelId, as]) => {
    const fam = variantById(modelId)?.family;
    return `<div class="mcard" data-id="grp-${esc(modelId)}">
      <div class="mcard-head">
        <div><div class="mname">${esc(modelId)}</div>
          <div class="muted">${esc(fam?.family || "")}${fam ? " · " + fam.capabilities.map(esc).join(", ") : ""}</div></div>
        <button class="btn btn-sm" data-act="install" data-id="${esc(modelId)}">Add to another node</button>
      </div>
      <div class="placements">${copiesOf(as).map(c => c.length > 1 || c[0].group_id ? poolRow(c) : placementRow(c[0])).join("")}</div>
    </div>`;
  }).join(""));
}

// copiesOf groups assignments into copies: a pooled copy is all members of
// one group (leader first); every other assignment is a copy on its own.
function copiesOf(as) {
  const out = [], pools = new Map();
  for (const a of as) {
    if (!a.group_id) { out.push([a]); continue; }
    if (!pools.has(a.group_id)) { pools.set(a.group_id, []); out.push(pools.get(a.group_id)); }
    pools.get(a.group_id).push(a);
  }
  for (const c of out) c.sort((x, y) => (y.role === "leader") - (x.role === "leader") || y.share - x.share);
  return out;
}

function poolRow(members) {
  const leader = members.find(m => m.role === "leader") || members[0];
  const failed = members.find(m => m.state === "failed");
  let st;
  if (failed) st = statusOf(failed);
  else if (members.every(m => m.state === "loaded")) st = { cls: "s-adopted", label: "Ready" };
  else if (["fetching", "downloading", "loading"].includes(leader.state)) st = statusOf(leader);
  else if (members.some(m => m.state === "offline")) st = { cls: "s-offline", label: "A member node is offline" };
  else st = { cls: "s-enrolling", label: "Starting workers" };
  const who = members.map(m => `<strong>${esc(m.node_name || m.node_id.slice(0, 8))}</strong> <span class="muted">${m.role === "leader" ? "leader, " : ""}${Math.round(m.share * 100)}%</span>`).join(" + ");
  const total = members.reduce((n, m) => n + m.need, 0);
  const kinds = [...new Set(members.map(m => PLACEMENT[m.placement] || m.placement))].join(" / ");
  return `<div class="placement pooled" data-id="pool-${esc(leader.group_id)}">
    <div class="pl-node"><span class="pill plain pool-tag">Split across ${members.length} nodes</span>
      <div class="pool-members">${who}</div>
      <div class="muted">${esc(leader.quant)} · ${esc(kinds)} · ${bytes(total)} in total</div></div>
    <div class="pl-state"><span class="pill ${st.cls}">${esc(st.label)}</span>
      ${st.bar ? `<progress max="1" value="${(failed || leader).progress || 0}"></progress>` : ""}${st.detail || ""}</div>
    <div class="pl-actions">
      ${leader.state === "failed" && leader.stage === "controller" ? `<button class="btn btn-sm" data-act="retry" data-id="${esc(leader.id)}">Retry</button>` : ""}
      <button class="btn btn-sm btn-ghost" data-act="unassign" data-id="${esc(leader.id)}">Remove</button>
    </div>
  </div>`;
}

function placementRow(a) {
  const st = statusOf(a);
  return `<div class="placement" data-id="${esc(a.id)}">
    <div class="pl-node"><strong>${esc(a.node_name || a.node_id.slice(0, 8))}</strong>
      <div class="muted">${esc(a.quant)} · ${PLACEMENT[a.placement] || esc(a.placement)} · ${bytes(a.need)}${a.residency === "on_demand" ? " · on demand" : ""}</div></div>
    <div class="pl-state"><span class="pill ${st.cls}">${esc(st.label)}</span>
      ${st.bar ? `<progress max="1" value="${a.progress || 0}"></progress>` : ""}${st.detail || ""}</div>
    <div class="pl-actions">
      ${a.state === "failed" && a.stage === "controller" ? `<button class="btn btn-sm" data-act="retry" data-id="${esc(a.id)}">Retry</button>` : ""}
      <button class="btn btn-sm" data-act="swap" data-id="${esc(a.id)}">Swap…</button>
      <button class="btn btn-sm btn-ghost" data-act="unassign" data-id="${esc(a.id)}">Remove</button>
    </div>
  </div>`;
}

// ---------- Catalog ----------

function renderCatalog() {
  $("#catalog-error").hidden = !ms.catalogError;
  $("#catalog-error").textContent = ms.catalogError ? `Catalog unavailable: ${ms.catalogError}` : "";
  morph($("#cap-filters"), CAPS.map(([k, label]) =>
    `<button class="chip" data-cap="${k}" aria-pressed="${ms.cap === k}">${label}</button>`).join(""));
  if (!ms.catalog) return;

  const nodes = servingNodes();
  const q = ms.q.trim().toLowerCase();
  const fams = ms.catalog.families.filter(f =>
    (ms.cap === "all" || f.capabilities.includes(ms.cap)) &&
    (!q || [f.family, f.id, f.description, ...f.variants.map(v => v.id)].some(s => (s || "").toLowerCase().includes(q))));

  morph($("#catalog"), fams.map(f => `
    <div class="fcard" data-id="fam-${esc(f.id)}">
      <div class="fcard-head">
        <h2>${esc(f.family)}</h2>
        <span class="pill plain" title="${esc(f.license.name || f.license.spdx)}">${esc(f.license.spdx)}${f.license.commercial_use ? "" : " · non-commercial"}</span>
      </div>
      <p class="muted">${esc(f.description)}</p>
      <div class="flags">${f.capabilities.map(c => `<span class="pill plain">${esc(c)}</span>`).join("")}</div>
      <div class="variants">${f.variants.map(v => variantRow(v, nodes)).join("")}</div>
    </div>`).join("") || `<p class="muted">No models match.</p>`);
}

function variantRow(v, nodes) {
  const fit = ms.catalog.fit?.[v.id] || {};
  const fitting = nodes.filter(n => fit[n.node_id]?.fits);
  const onNodes = ms.assignments.filter(a => a.model_id === v.id).length;
  const best = fitting.map(n => fit[n.node_id]).sort((a, b) => ({ gpu: 0, hybrid: 1, cpu: 2 }[a.placement] - { gpu: 0, hybrid: 1, cpu: 2 }[b.placement]))[0];
  let summary;
  if (!nodes.length) summary = `<span class="muted">no nodes yet</span>`;
  else if (!fitting.length && ms.catalog.pools?.[v.id]?.fits) {
    const p = ms.catalog.pools[v.id];
    summary = `<span>Too big for one node · <strong>fits split across ${p.members.length} nodes</strong></span>`;
  } else if (!fitting.length) summary = `<span class="note bad" title="${esc(nodes.map(n => nodeName(n) + ": " + (fit[n.node_id]?.reason || "?")).join("\n"))}">Doesn't fit any node${nodes.length > 1 ? ", even split" : ""}</span>`;
  else summary = `Fits ${fitting.length} of ${nodes.length} node${nodes.length === 1 ? "" : "s"} · best: ${PLACEMENT[best.placement]}`;
  const smallest = Math.min(...v.quants.map(q => q.size));
  return `<div class="variant" data-id="v-${esc(v.id)}">
    <div class="v-name"><strong>${esc(v.id)}</strong>
      <span class="muted">${v.params_b >= 1 ? v.params_b.toFixed(1) + "B" : Math.round(v.params_b * 1000) + "M"} · from ${bytes(smallest)}</span></div>
    <div class="v-fit">${summary}${onNodes ? ` <span class="pill s-adopted">on ${onNodes} node${onNodes === 1 ? "" : "s"}</span>` : ""}</div>
    <button class="btn btn-sm btn-primary" data-act="install" data-id="${esc(v.id)}" ${nodes.length ? "" : "disabled"}>Install</button>
  </div>`;
}

function renderModels() {
  renderInstalled();
  renderCatalog();
}

// ---------- Install dialog ----------

function openInstall(modelId, nodeId) {
  const found = variantById(modelId);
  if (!found) return toast("Catalog not loaded yet", true);
  ms.install = { modelId };
  $("#install-name").textContent = modelId;
  $("#install-desc").textContent = found.family.description;
  $("#install-quant").innerHTML = `<option value="">Best for each node (recommended)</option>` +
    found.v.quants.map(q => `<option value="${esc(q.quant)}">${esc(q.quant)} · ${bytes(q.size)} download</option>`).join("");
  $("#install-replicas").value = 1;
  $("#install-resident").checked = true;
  const fitsAlone = servingNodes().some(n => ms.catalog?.fit?.[modelId]?.[n.node_id]?.fits);
  const poolFits = !!ms.catalog?.pools?.[modelId]?.fits;
  const where = nodeId ? "pick" : !fitsAlone && poolFits ? "pool" : "auto";
  for (const r of document.querySelectorAll("input[name=where]")) r.checked = r.value === where;
  $("#where-pool").hidden = servingNodes().length < 2;
  $("#install-error").hidden = true;
  $("#install-go").disabled = false;
  renderInstallNodes(nodeId ? [nodeId] : []);
  $("#install-dialog").showModal();
}

function renderInstallNodes(preselect) {
  const { modelId } = ms.install;
  const quant = $("#install-quant").value;
  const mode = document.querySelector("input[name=where]:checked").value;
  const pick = mode === "pick";
  const checked = new Set(preselect || [...document.querySelectorAll("#install-nodes input:checked")].map(i => i.value));
  const fit = ms.catalog?.fit?.[modelId] || {};
  const suggestion = ms.catalog?.pools?.[modelId];
  const hint = $("#install-pool-hint");
  hint.hidden = mode !== "pool";
  if (mode === "pool") {
    hint.innerHTML = suggestion?.fits
      ? `Suggested split: ${suggestion.members.map(m => `<strong>${esc(nodeName(state.nodes.find(n => n.node_id === m.node_id) || { node_id: m.node_id }))}</strong> ${Math.round(m.share * 100)}%${m.leader ? " (leader)" : ""}`).join(" + ")} · ${esc(suggestion.quant)}. Tick nodes to choose your own.`
      : "Tick at least two nodes. The model is split in proportion to each node's free memory.";
  }
  const rows = servingNodes().map(n => {
    if (mode === "pool") {
      const has = ms.assignments.some(a => a.node_id === n.node_id && a.model_id === modelId);
      const sug = suggestion?.members?.find(m => m.node_id === n.node_id);
      const ok = !has && n.state === "adopted";
      return `<label class="node-pick ${ok ? "" : "disabled"}">
        <input type="checkbox" value="${esc(n.node_id)}" ${ok ? "" : "disabled"} ${ok && checked.has(n.node_id) ? "checked" : ""}>
        <span class="np-name"><strong>${esc(nodeName(n))}</strong>${n.state === "offline" ? ` <span class="pill s-offline">offline</span>` : ""}</span>
        <span class="np-fit">${has ? `<span class="muted">already has this model</span>` : sug ? `suggested: ${Math.round(sug.share * 100)}% of the layers` : `<span class="muted">${hwLabel(n.hw_summary)}</span>`}</span>
      </label>`;
    }
    let f = fit[n.node_id];
    if (f && quant) {
      const o = f.options.find(o => o.quant === quant);
      f = o ? { ...o, fits: o.fits } : { fits: false, reason: "not available for this node" };
    }
    const has = ms.assignments.some(a => a.node_id === n.node_id && a.model_id === modelId);
    const ok = f?.fits && !has;
    return `<label class="node-pick ${ok ? "" : "disabled"}">
      <input type="checkbox" value="${esc(n.node_id)}" ${ok ? "" : "disabled"} ${ok && checked.has(n.node_id) ? "checked" : ""}>
      <span class="np-name"><strong>${esc(nodeName(n))}</strong>${n.state === "offline" ? ` <span class="pill s-offline">offline</span>` : ""}</span>
      <span class="np-fit">${has ? `<span class="muted">already installed</span>` : fitText(f)}</span>
    </label>`;
  });
  const box = $("#install-nodes");
  box.classList.toggle("picking", pick || mode === "pool");
  box.innerHTML = rows.join("") || `<p class="muted">No adopted nodes.</p>`;
}

async function submitInstall() {
  const btn = $("#install-go");
  const err = $("#install-error");
  err.hidden = true;
  const mode = document.querySelector("input[name=where]:checked").value;
  const pick = mode === "pick";
  const body = {
    model_id: ms.install.modelId,
    quant: $("#install-quant").value,
    residency: $("#install-resident").checked ? "resident" : "on_demand",
  };
  if (mode === "pool") {
    body.pool = true;
    const nodes = [...document.querySelectorAll("#install-nodes input:checked")].map(i => i.value);
    if (nodes.length === 1) {
      err.textContent = "Tick at least two nodes, or none to let the Controller choose.";
      err.hidden = false;
      return;
    }
    if (nodes.length) body.nodes = nodes;
  } else if (pick) {
    body.nodes = [...document.querySelectorAll("#install-nodes input:checked")].map(i => i.value);
    if (!body.nodes.length) {
      err.textContent = "Tick at least one node.";
      err.hidden = false;
      return;
    }
  } else {
    body.replicas = Math.max(1, parseInt($("#install-replicas").value, 10) || 1);
  }
  btn.disabled = true;
  try {
    const out = await api("POST", "/api/v1/models/install", body);
    $("#install-dialog").close();
    const names = out.map(a => nodeName(state.nodes.find(n => n.node_id === a.node_id) || { node_id: a.node_id })).join(", ");
    toast(body.pool ? `Splitting ${body.model_id} across ${names}` : `Installing ${body.model_id} on ${names}`);
    ms.catalogAt = 0;
    refresh();
  } catch (e) {
    if (e instanceof AuthError) { $("#install-dialog").close(); return showLogin(); }
    err.textContent = e.message;
    err.hidden = false;
    btn.disabled = false;
  }
}

// ---------- Swap / add-to-node picker ----------

async function openPicker(mode, id) {
  let nodeId, assignment = null, options;
  try {
    if (mode === "swap") {
      assignment = ms.assignments.find(a => a.id === id);
      if (!assignment) return;
      nodeId = assignment.node_id;
      options = await api("GET", `/api/v1/assignments/${encodeURIComponent(id)}/swap-options`);
    } else {
      nodeId = id;
      const fit = ms.catalog?.fit || {};
      options = variants().map(({ v }) => ({ model_id: v.id, ...(fit[v.id]?.[nodeId] || { fits: false, reason: "unknown" }) }));
    }
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    return toast(e.message, true);
  }
  const node = state.nodes.find(n => n.node_id === nodeId);
  const onNode = new Set(assignmentsOn(nodeId).map(a => a.model_id));
  ms.picker = {
    mode, nodeId, assignment, choice: null,
    options: options.filter(o => !onNode.has(o.model_id) || o.model_id === assignment?.model_id)
      .filter(o => o.model_id !== assignment?.model_id),
  };
  $("#swap-dialog h2").innerHTML = mode === "swap"
    ? `Swap model on <span>${esc(nodeName(node || { node_id: nodeId }))}</span>`
    : `Install a model on <span>${esc(nodeName(node || { node_id: nodeId }))}</span>`;
  $("#swap-dialog p.muted").hidden = mode !== "swap";
  $("#swap-from").textContent = assignment ? `${assignment.model_id} (${assignment.quant})` : "";
  $("#swap-go").textContent = mode === "swap" ? "Swap" : "Install";
  $("#swap-search").value = "";
  $("#swap-error").hidden = true;
  $("#swap-go").disabled = true;
  renderPicker();
  $("#swap-dialog").showModal();
  $("#swap-search").focus();
}

function renderPicker() {
  const p = ms.picker;
  const q = $("#swap-search").value.trim().toLowerCase();
  const rows = p.options
    .filter(o => !q || o.model_id.toLowerCase().includes(q) || (variantById(o.model_id)?.family.family || "").toLowerCase().includes(q))
    .sort((a, b) => (b.fits - a.fits) || a.model_id.localeCompare(b.model_id))
    .map(o => {
      const fam = variantById(o.model_id)?.family;
      return `<label class="swap-opt ${o.fits ? "" : "disabled"}">
        <input type="radio" name="swap-choice" value="${esc(o.model_id)}" ${o.fits ? "" : "disabled"} ${p.choice === o.model_id ? "checked" : ""}>
        <span class="np-name"><strong>${esc(o.model_id)}</strong> <span class="muted">${esc(fam?.capabilities.join(", ") || "")}</span></span>
        <span class="np-fit">${fitText(o)}</span>
      </label>`;
    });
  $("#swap-list").innerHTML = rows.join("") || `<p class="muted">No models match.</p>`;
}

async function submitPicker() {
  const p = ms.picker;
  const btn = $("#swap-go");
  btn.disabled = true;
  try {
    if (p.mode === "swap") {
      await api("POST", `/api/v1/assignments/${encodeURIComponent(p.assignment.id)}/swap`, { model_id: p.choice });
      toast(`Swapping ${p.assignment.model_id} for ${p.choice}`);
    } else {
      await api("POST", "/api/v1/models/install", { model_id: p.choice, nodes: [p.nodeId] });
      toast(`Installing ${p.choice}`);
    }
    $("#swap-dialog").close();
    ms.catalogAt = 0;
    refresh();
  } catch (e) {
    if (e instanceof AuthError) { $("#swap-dialog").close(); return showLogin(); }
    $("#swap-error").textContent = e.message;
    $("#swap-error").hidden = false;
    btn.disabled = false;
  }
}

// ---------- Remove / retry ----------

async function unassign(id) {
  const a = ms.assignments.find(x => x.id === id);
  if (!a) return;
  const members = a.group_id ? ms.assignments.filter(x => x.group_id === a.group_id) : [a];
  const msg = a.group_id
    ? `Remove the copy of ${a.model_id} split across ${members.map(m => m.node_name).join(", ")}? It stops on all of them.`
    : `Remove ${a.model_id} from ${a.node_name}? It is unloaded now and deleted from the node's disk after a day.`;
  if (!confirm(msg)) return;
  try {
    await api("DELETE", `/api/v1/assignments/${encodeURIComponent(id)}`);
    toast(a.group_id ? `Removed the split copy of ${a.model_id}` : `Removed ${a.model_id} from ${a.node_name}`);
    ms.catalogAt = 0;
    refresh();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

async function retry(id) {
  try {
    await api("POST", `/api/v1/assignments/${encodeURIComponent(id)}/retry`);
    toast("Retrying download");
    refresh();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

// ---------- Node drawer section ----------

function drawerModels(n) {
  if (!["adopted", "offline"].includes(n.state)) return "";
  const as = assignmentsOn(n.node_id);
  return `<div class="section"><h3>Models</h3>
    ${as.length ? `<div class="placements compact">${as.map(a => {
      const st = statusOf(a);
      return `<div class="placement" data-id="${esc(a.id)}">
        <div class="pl-node"><strong>${esc(a.model_id)}</strong><div class="muted">${esc(a.quant)} · ${PLACEMENT[a.placement] || ""}${a.group_id ? ` · split: ${a.role === "leader" ? "leader" : "worker"}, ${Math.round(a.share * 100)}% of layers` : ""}</div></div>
        <div class="pl-state"><span class="pill ${st.cls}">${esc(st.label)}</span>${st.bar ? `<progress max="1" value="${a.progress || 0}"></progress>` : ""}</div>
        <div class="pl-actions">${a.group_id ? "" : `<button class="btn btn-sm" data-act="swap" data-id="${esc(a.id)}">Swap…</button>`}
          <button class="btn btn-sm btn-ghost" data-act="unassign" data-id="${esc(a.id)}">Remove</button></div>
      </div>`;
    }).join("")}</div>` : `<p class="muted">No models on this node.</p>`}
    <p><button class="btn btn-sm btn-primary" data-act="add-to-node" data-id="${esc(n.node_id)}">Install a model…</button></p>
  </div>`;
}

// ---------- Wiring ----------

hooks.refresh.push(refreshModels);
hooks.drawer.push(drawerModels);
Object.assign(actions, {
  install: id => openInstall(id),
  swap: id => openPicker("swap", id),
  "add-to-node": id => openPicker("add", id),
  unassign: id => unassign(id),
  retry: id => retry(id),
});
hooks.wire.push(() => {
  document.addEventListener("click", e => {
    const chip = e.target.closest("[data-cap]");
    if (chip) { ms.cap = chip.dataset.cap; renderCatalog(); }
  });
  $("#catalog-search").addEventListener("input", e => { ms.q = e.target.value; renderCatalog(); });
  $("#install-quant").addEventListener("change", () => renderInstallNodes());
  for (const r of document.querySelectorAll("input[name=where]")) r.addEventListener("change", () => renderInstallNodes());
  $("#install-form").addEventListener("submit", e => {
    if (e.submitter?.value === "go") { e.preventDefault(); submitInstall(); }
  });
  $("#swap-search").addEventListener("input", renderPicker);
  $("#swap-list").addEventListener("change", e => {
    if (e.target.name === "swap-choice") { ms.picker.choice = e.target.value; $("#swap-go").disabled = false; }
  });
  $("#swap-form").addEventListener("submit", e => {
    if (e.submitter?.value === "go") { e.preventDefault(); if (ms.picker?.choice) submitPicker(); }
  });
});
