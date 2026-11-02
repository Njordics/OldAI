// AI Cluster dashboard. Plain JS, no build step and no third-party code, so
// it works on an offline LAN and under a strict Content-Security-Policy.
"use strict";

const POLL_MS = 2000;
const $ = (sel, root = document) => root.querySelector(sel);

// Extension points for the other dashboard scripts (models.js).
const hooks = {
  refresh: [], // async () => void, run on every poll
  route: [],   // (view) => void, when the visible tab changes
  drawer: [],  // (node) => html, extra sections in the node drawer
  wire: [],    // () => void, once at startup
};
const VIEWS = ["nodes", "models", "chat", "code", "connect", "settings"];

const state = {
  nodes: [],
  filter: "all",
  search: "",
  selected: null, // node_id shown in the drawer
  detail: null,   // full node record for the drawer
  cluster: null,
  pollTimer: null,
  busy: new Set(), // node IDs with an adopt/release request in flight
};

// ---------- API ----------

class AuthError extends Error {}

async function api(method, path, body) {
  const opts = {
    method,
    credentials: "same-origin",
    headers: { "X-Requested-With": "aicluster" },
  };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { /* non-JSON body */ }
  if (res.status === 401 && path.startsWith("/api/")) throw new AuthError();
  if (!res.ok) {
    const msg = data?.error?.message || text || res.statusText;
    const err = new Error(msg);
    err.code = data?.error?.code;
    throw err;
  }
  return data;
}

// ---------- DOM patching ----------

// morph updates target's children to match html while keeping every element
// that is still there the same DOM node. The dashboard re-renders every poll;
// replacing innerHTML would drop keyboard focus and swallow clicks that land
// mid-refresh.
function morph(target, html) {
  const tpl = document.createElement("template");
  tpl.innerHTML = html;
  morphChildren(target, tpl.content);
}

function morphChildren(cur, next) {
  const want = [...next.childNodes];
  let have = [...cur.childNodes];
  for (let i = 0; i < want.length; i++) {
    const n = want[i];
    const c = have[i];
    if (!c) { cur.append(n); continue; }
    if (c.nodeType !== n.nodeType || c.nodeName !== n.nodeName || keyOf(c) !== keyOf(n)) {
      cur.replaceChild(n, c);
      continue;
    }
    if (c.nodeType === Node.TEXT_NODE || c.nodeType === Node.COMMENT_NODE) {
      if (c.nodeValue !== n.nodeValue) c.nodeValue = n.nodeValue;
      continue;
    }
    syncAttrs(c, n);
    morphChildren(c, n);
  }
  have = [...cur.childNodes];
  for (let i = have.length - 1; i >= want.length; i--) have[i].remove();
}

// Elements with a data-id/data-filter are matched by identity, so a row
// never morphs into a different node's row.
function keyOf(el) {
  return el.nodeType === Node.ELEMENT_NODE ? (el.dataset.id ?? el.dataset.filter ?? "") : "";
}

function syncAttrs(c, n) {
  for (const { name } of [...c.attributes]) {
    // <details open> is the user's choice, not ours.
    if (!n.hasAttribute(name) && !(name === "open" && c.tagName === "DETAILS")) c.removeAttribute(name);
  }
  for (const { name, value } of [...n.attributes]) {
    if (c.getAttribute(name) !== value) c.setAttribute(name, value);
  }
  if (c.tagName === "METER" && c.value !== n.value) c.value = n.value;
  if ("disabled" in c) c.disabled = n.hasAttribute("disabled");
}

// ---------- Formatting ----------

function esc(v) {
  return String(v ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

const STATE_LABEL = {
  adopted: "Online", offline: "Offline", pending: "Pending", enrolling: "Adopting",
  discovered: "Discovered", released: "Released", foreign: "Other cluster",
};

function statePill(s) {
  return `<span class="pill s-${esc(s)}">${esc(STATE_LABEL[s] || s)}</span>`;
}

function isZeroTime(t) { return !t || t.startsWith("0001-"); }

function ago(t) {
  if (isZeroTime(t)) return "—";
  const s = Math.max(0, Math.round((Date.now() - Date.parse(t)) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

function when(t) {
  if (isZeroTime(t)) return "—";
  return `${new Date(t).toLocaleString()} <span class="muted">(${ago(t)})</span>`;
}

function bytes(n) {
  if (!n) return "—";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(n >= 10 || i === 0 ? 0 : 1)} ${u[i]}`;
}

function duration(sec) {
  if (!sec) return "—";
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

const VENDOR = { nvidia: "NVIDIA", amd: "AMD", intel: "Intel" };

// driverRow explains the GPU driver the Controller picked and how far the
// node is with it (protocol §14), or null when there's nothing to say.
function driverRow(n, inv, obs) {
  const d = obs?.driver, want = n.desired?.drivers;
  const name = esc(d?.name || want?.name || "");
  let html;
  if (d) {
    html = {
      installing: `Installing ${name}…<div class="muted">Downloaded through the Controller from Ubuntu's archive.</div>`,
      rebooting: `Rebooting to load ${name}<div class="muted">${esc(d.error || "")}</div>`,
      reboot_required: `${name} is installed<div class="muted">It loads after a reboot. ${esc(d.error || "")}</div>`,
      ready: `${name} <span class="muted">${esc(d.version || "")}</span>`,
      failed: `<span class="bad-text">${name} isn't working</span><div class="muted">${esc(d.error || "")}</div>`,
    }[d.state] || `${name} <span class="muted">${esc(d.state)}</span>`;
  } else if (want) {
    html = `${name} <div class="muted">Picked for this node's card; waiting for the node.</div>`;
  } else if (n.driver_problem) {
    html = `None <div class="muted">${esc(n.driver_problem)}</div>`;
  } else if (inv?.gpus?.some(g => g.vendor === "nvidia")) {
    html = `None yet <div class="muted">The Controller is choosing one.</div>`;
  } else {
    return null;
  }
  return ["GPU driver", html];
}

// networkLabel says which network a node is on (Slave network or home LAN).
function networkLabel(o) {
  const where = o.on_slave_net ? "Slave network" : "Home network";
  const nic = o.iface ? ` · ${esc(o.iface)}` : "";
  const err = o.error ? `<div class="muted">${esc(o.error)}</div>` : "";
  return `${where}${nic}${err}`;
}

function hwLabel(s) {
  if (!s || s === "cpu") return "CPU only";
  const [vendor, rest] = s.split(":");
  const m = /^(\d+)x(.*)$/.exec(rest || "");
  if (!m) return esc(s);
  const mem = m[2] && m[2] !== "?" ? ` · ${m[2].replace("G", " GB")}` : "";
  return `${VENDOR[vendor] || esc(vendor)} ×${m[1]}${mem}`;
}

function nodeName(n) { return n.name || `node-${n.node_id.slice(0, 8)}`; }

function memMeter(h) {
  if (!h || !h.mem_total) return `<span class="muted">—</span>`;
  const used = h.mem_total - h.mem_avail;
  const pct = Math.round(used / h.mem_total * 100);
  return `<meter min="0" max="${h.mem_total}" low="${h.mem_total * .75}" high="${h.mem_total * .9}" optimum="0" value="${used}"></meter><span class="meter-label">${pct}%</span>`;
}

function canAdopt(n) { return ["pending", "discovered", "released"].includes(n.state) && n.agent_state !== "adopted"; }
function canRelease(n) { return ["adopted", "offline", "enrolling"].includes(n.state); }

function nodeNote(n) {
  if (n.state === "pending" && n.pairing_required) return `<div class="note">Needs the pairing code from its screen</div>`;
  if (n.state === "foreign") return `<div class="note">Adopted by another cluster</div>`;
  if (n.returning && n.state !== "adopted") return `<div class="note">Known hardware (reinstalled or reset)</div>`;
  if (n.last_error && n.state !== "adopted") return `<div class="note bad">${esc(n.last_error)}</div>`;
  return "";
}

// ---------- Rendering: nodes ----------

const FILTERS = [
  ["all", "All", () => true],
  ["adopted", "Online", n => n.state === "adopted"],
  ["offline", "Offline", n => n.state === "offline"],
  ["pending", "Waiting", n => ["pending", "discovered", "enrolling"].includes(n.state)],
  ["released", "Released", n => n.state === "released" || n.state === "foreign"],
];

function renderStats() {
  const c = s => state.nodes.filter(n => n.state === s).length;
  const waiting = state.nodes.filter(n => n.state === "pending").length;
  const tiles = [
    ["Nodes", state.nodes.length, ""],
    ["Online", c("adopted"), ""],
    ["Offline", c("offline"), c("offline") ? "bad" : ""],
    ["Waiting for approval", waiting, waiting ? "warn" : ""],
  ];
  morph($("#stats"), tiles.map(([label, v, cls]) =>
    `<div class="stat ${cls}"><div class="stat-label">${label}</div><div class="stat-value">${v}</div></div>`).join(""));
}

function renderPending() {
  const pending = state.nodes.filter(n => n.state === "pending");
  const box = $("#pending");
  box.hidden = pending.length === 0;
  if (!pending.length) return;
  morph(box, `
    <h2>${pending.length === 1 ? "1 node is" : `${pending.length} nodes are`} waiting for approval</h2>
    <div class="pending-list">${pending.map(n => `
      <div class="pending-item">
        <div class="grow">
          <div class="node-name">${esc(nodeName(n))}</div>
          <div class="node-id">${esc(n.node_id.slice(0, 8))} · ${esc(n.addr || "—")} · ${hwLabel(n.hw_summary)}</div>
          ${nodeNote(n)}
        </div>
        <button class="btn btn-sm" data-act="details" data-id="${esc(n.node_id)}">Details</button>
        <button class="btn btn-primary btn-sm" data-act="adopt" data-id="${esc(n.node_id)}" ${state.busy.has(n.node_id) ? "disabled" : ""}>Adopt</button>
      </div>`).join("")}
    </div>`);
}

function renderFilters() {
  morph($("#filters"), FILTERS.map(([key, label, fn]) =>
    `<button class="chip" data-filter="${key}" aria-pressed="${state.filter === key}">${label}<span class="n">${state.nodes.filter(fn).length}</span></button>`).join(""));
}

function visibleNodes() {
  const fn = FILTERS.find(f => f[0] === state.filter)[2];
  const q = state.search.trim().toLowerCase();
  return state.nodes.filter(fn).filter(n => !q ||
    [n.name, n.node_id, n.addr, n.hw_summary].some(v => (v || "").toLowerCase().includes(q)));
}

function renderTable() {
  const rows = visibleNodes();
  $("#empty").hidden = state.nodes.length > 0;
  morph($("#nodes-body"), rows.map(n => {
    const h = n.health || {};
    const live = n.state === "adopted";
    const action = canAdopt(n)
      ? `<button class="btn btn-sm ${n.state === "pending" ? "btn-primary" : ""}" data-act="adopt" data-id="${esc(n.node_id)}" ${state.busy.has(n.node_id) ? "disabled" : ""}>Adopt</button>`
      : "";
    return `
      <tr data-id="${esc(n.node_id)}" class="${state.selected === n.node_id ? "selected" : ""}" tabindex="0">
        <td><div class="node-name">${esc(nodeName(n))}</div><div class="node-id">${esc(n.node_id.slice(0, 8))}</div>${nodeNote(n)}</td>
        <td>${statePill(n.state)}</td>
        <td class="hide-sm mono">${esc(n.addr || "—")}</td>
        <td class="hide-sm">${hwLabel(n.hw_summary)}</td>
        <td class="hide-md num">${live && h.load1 !== undefined ? h.load1.toFixed(2) : `<span class="muted">—</span>`}</td>
        <td class="hide-md">${live ? memMeter(h) : `<span class="muted">—</span>`}</td>
        <td class="num">${ago(n.state === "adopted" || n.state === "offline" ? n.last_heartbeat : n.last_seen)}</td>
        <td class="actions">${action}</td>
      </tr>`;
  }).join("") || (state.nodes.length ? `<tr><td colspan="8" class="muted">No nodes match.</td></tr>` : ""));
}

function renderNodes() {
  renderStats();
  renderPending();
  renderFilters();
  renderTable();
}

// ---------- Rendering: drawer ----------

function kv(pairs) {
  return `<dl class="kv">${pairs.filter(p => p).map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join("")}</dl>`;
}

function renderDrawer() {
  const n = state.detail || state.nodes.find(x => x.node_id === state.selected);
  if (!n) return;
  morph($("#drawer-title"), `${esc(nodeName(n))} ${statePill(n.state)}`);
  $("#drawer-sub").textContent = n.node_id;

  const busy = state.busy.has(n.node_id);
  morph($("#drawer-actions"), [
    canAdopt(n) ? `<button class="btn btn-primary btn-sm" data-act="adopt" data-id="${esc(n.node_id)}" ${busy ? "disabled" : ""}>Adopt</button>` : "",
    canRelease(n) ? `<button class="btn btn-sm" data-act="release" data-id="${esc(n.node_id)}" ${busy ? "disabled" : ""}>Release…</button>` : "",
  ].join("") || `<span class="muted">No actions available in this state.</span>`);
  $("#drawer-actions").hidden = false;

  const h = n.health || {};
  const inv = n.inventory || null;
  const obs = n.observed || null;
  const parts = [];

  if (n.state === "offline") parts.push(`<div class="banner bad">No heartbeat since ${ago(n.last_heartbeat)}. Requests are routed to other nodes until it returns.</div>`);
  if (n.state === "pending" && n.pairing_required) parts.push(`<div class="banner warn">This node was installed with a pairing code. Read the 6-digit code from its screen to adopt it.</div>`);
  if (n.state === "enrolling") parts.push(`<div class="banner info">Certificate issued. Waiting for the node's first heartbeat.</div>`);
  if (n.last_error && n.state !== "adopted") parts.push(`<div class="banner bad">${esc(n.last_error)}</div>`);

  parts.push(`<div class="section"><h3>Overview</h3>${kv([
    ["Address", `<span class="mono">${esc(n.addr || "—")}${n.port ? ":" + n.port : ""}</span>`],
    obs?.network ? ["Network", networkLabel(obs.network)] : null,
    ["Hardware", hwLabel(n.hw_summary)],
    ["Agent", esc(n.agent_version || "—")],
    ["Phase", esc(n.phase || "—")],
    ["Config generation", n.desired_generation ? `${n.observed_generation} of ${n.desired_generation} applied` : "—"],
    ["Adopted", when(n.adopted_at)],
    ["Last heartbeat", when(n.last_heartbeat)],
    ["First seen", when(n.first_seen)],
    n.cert_serial ? ["Certificate", `<span class="mono">${esc(n.cert_serial.slice(0, 16))}…</span>`] : null,
  ])}</div>`);

  if (n.state === "adopted" || n.state === "offline") {
    parts.push(`<div class="section"><h3>Health</h3>${kv([
      ["Uptime", duration(h.uptime_s)],
      ["Load (1 min)", h.load1 !== undefined ? h.load1.toFixed(2) : "—"],
      ["Memory", h.mem_total ? `${memMeter(h)} <span class="muted">${bytes(h.mem_total - h.mem_avail)} of ${bytes(h.mem_total)}</span>` : "—"],
      ["Disk free", bytes(h.disk_free)],
    ])}</div>`);
  }

  if (inv) {
    const cpu = inv.cpu || {};
    parts.push(`<div class="section"><h3>Hardware</h3>${kv([
      ["Host", `${esc(inv.host?.hostname || "—")} <span class="muted">${esc(inv.host?.product || "")}</span>`],
      ["OS", `${esc(inv.host?.os || "—")} <span class="muted">${esc(inv.host?.kernel || "")}</span>`],
      ["CPU", `${esc(cpu.model || "—")}<div class="muted">${cpu.cores || "?"} cores · ${cpu.threads || "?"} threads${cpu.sockets > 1 ? ` · ${cpu.sockets} sockets` : ""}</div>
        ${cpu.flags?.length ? `<div class="flags">${cpu.flags.map(f => `<span class="pill plain">${esc(f)}</span>`).join("")}</div>` : ""}`],
      ["Memory", bytes(inv.memory?.total)],
      ["GPUs", inv.gpus?.length ? inv.gpus.map(g => `<div class="gpu"><strong>${esc(VENDOR[g.vendor] || g.vendor)}</strong> ${esc(g.model || g.pci_id)}
          <div class="muted">${g.integrated ? "built into the processor, shares its memory · " : ""}${g.vram ? bytes(g.vram) + " VRAM · " : ""}${g.driver?.installed ? "driver " + esc(g.driver.version || "loaded") : "no driver yet"}</div></div>`).join("") : "None (CPU inference)"],
      driverRow(n, inv, obs),
      ["Disks", inv.disks?.length ? inv.disks.map(d => `<div class="disk"><span class="mono">${esc(d.mount)}</span> <span class="muted">${bytes(d.free)} free of ${bytes(d.total)}</span></div>`).join("") : "—"],
      ["Network", inv.network?.length ? inv.network.map(i => `<div class="nic"><span class="mono">${esc(i.iface)}</span> ${esc((i.addrs || []).join(", "))}
          <div class="muted">${i.speed_mbps > 0 ? i.speed_mbps + " Mb/s · " : ""}${esc(i.mac)}</div></div>`).join("") : "—"],
    ])}</div>`);
  } else if (n.state !== "foreign") {
    parts.push(`<div class="section"><h3>Hardware</h3><p class="muted">Not reported yet.</p></div>`);
  }

  if (obs?.conditions?.length) {
    parts.push(`<div class="section"><h3>Conditions</h3>${kv(obs.conditions.map(c =>
      [c.type, `${c.status ? "✓" : "✗"} ${esc(c.reason || "")}`]))}</div>`);
  }

  for (const f of hooks.drawer) parts.push(f(n));
  parts.push(`<details class="raw"><summary>Raw record</summary><pre>${esc(JSON.stringify(n, null, 2))}</pre></details>`);
  morph($("#drawer-body"), parts.join(""));
}

async function loadDetail() {
  if (!state.selected) return;
  try {
    state.detail = await api("GET", `/api/v1/nodes/${encodeURIComponent(state.selected)}`);
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    state.detail = null; // node removed (e.g. merged after reinstall)
    if (!state.nodes.some(n => n.node_id === state.selected)) return closeDrawer();
  }
  renderDrawer();
}

function openDrawer(id) {
  if (state.selected !== id) {
    $("#drawer-body").replaceChildren(); // don't carry one node's scroll/open sections to another
    $("#drawer-body").scrollTop = 0;
  }
  state.selected = id;
  state.detail = null;
  $("#drawer").hidden = false;
  $("#scrim").hidden = false;
  renderDrawer();
  renderTable();
  loadDetail();
  $("#drawer-close").focus();
}

function closeDrawer() {
  const prev = state.selected;
  state.selected = null;
  state.detail = null;
  $("#drawer").hidden = true;
  $("#scrim").hidden = true;
  renderTable();
  $(`tr[data-id="${CSS.escape(prev || "")}"]`)?.focus();
}

// ---------- Settings ----------

function renderSettings() {
  const c = state.cluster;
  if (!c) return;
  for (const r of document.querySelectorAll('input[name=mode]')) r.checked = r.value === c.adoption_mode;
  $("#igpu-on").checked = !!c.integrated_gpus;
  $("#cluster-info").innerHTML = [
    ["Cluster ID", `<span class="mono">${esc(c.cluster_id)}</span>`],
    ["CA fingerprint", `<span class="mono">${esc(c.ca_fp)}</span>`],
    ["Controller certificate", `<span class="mono">${esc(c.controller_fp)}</span>`],
    ["Node API endpoints", (c.endpoints || []).map(e => `<div class="mono">${esc(e)}</div>`).join("") || "—"],
    ["Slave network", c.slave_net
      ? `<span class="mono">${esc(c.slave_net.subnet)}</span> on ${esc(c.slave_net.iface)}<div class="muted">The Controller (${esc(c.slave_net.controller_ip)}) is the DHCP and DNS server for adopted nodes. Other devices on your network are not affected.</div>`
      : `Off <div class="muted">Nodes stay on your home network.</div>`],
  ].map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("");
  const pill = $("#mode-pill");
  pill.textContent = c.adoption_mode === "auto" ? "Auto-adopt on" : "Approval required";
  pill.className = `pill ${c.adoption_mode === "auto" ? "s-adopted" : "s-pending"}`;
}

async function setIGPU(on) {
  try {
    await api("PUT", "/api/v1/settings", { integrated_gpus: on });
    state.cluster.integrated_gpus = on;
    toast(on ? "Nodes may now run models on integrated graphics" : "Nodes now leave integrated graphics alone");
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
  renderSettings();
}

async function setMode(mode) {
  try {
    await api("PUT", "/api/v1/settings", { adoption_mode: mode });
    state.cluster.adoption_mode = mode;
    renderSettings();
    toast(mode === "auto" ? "New nodes are now adopted automatically" : "New nodes now wait for approval");
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
    renderSettings();
  }
}

// ---------- Adopt / release ----------

let dialogNode = null;

function openAdopt(id) {
  const n = state.nodes.find(x => x.node_id === id);
  if (!n) return;
  dialogNode = n;
  $("#adopt-name").textContent = nodeName(n);
  $("#adopt-code-field").hidden = !n.pairing_required;
  $("#adopt-code").required = !!n.pairing_required;
  $("#adopt-code").value = "";
  $("#adopt-error").hidden = true;
  $("#adopt-go").disabled = false;
  $("#adopt-dialog").showModal();
  if (n.pairing_required) $("#adopt-code").focus();
}

async function submitAdopt() {
  const n = dialogNode;
  const btn = $("#adopt-go");
  btn.disabled = true;
  btn.textContent = "Adopting…";
  state.busy.add(n.node_id);
  try {
    await api("POST", `/api/v1/nodes/${encodeURIComponent(n.node_id)}/adopt`, { pairing_code: $("#adopt-code").value.trim() });
    $("#adopt-dialog").close();
    toast(`${nodeName(n)} adopted. It shows as online after its first heartbeat.`);
  } catch (e) {
    if (e instanceof AuthError) { $("#adopt-dialog").close(); return showLogin(); }
    const err = $("#adopt-error");
    err.textContent = e.code === "pairing_failed" ? "That pairing code is wrong. After 3 wrong tries the node shows a new code." : e.message;
    err.hidden = false;
    btn.disabled = false;
  } finally {
    btn.textContent = "Adopt";
    state.busy.delete(n.node_id);
    refresh();
  }
}

function openRelease(id) {
  const n = state.nodes.find(x => x.node_id === id) || state.detail;
  if (!n) return;
  dialogNode = n;
  $("#release-name").textContent = nodeName(n);
  $("#release-keep").checked = false;
  $("#release-error").hidden = true;
  $("#release-go").disabled = false;
  $("#release-dialog").showModal();
}

async function submitRelease() {
  const n = dialogNode;
  const btn = $("#release-go");
  btn.disabled = true;
  state.busy.add(n.node_id);
  try {
    await api("POST", `/api/v1/nodes/${encodeURIComponent(n.node_id)}/release`, { keep_models: $("#release-keep").checked });
    $("#release-dialog").close();
    toast(`${nodeName(n)} released`);
  } catch (e) {
    if (e instanceof AuthError) { $("#release-dialog").close(); return showLogin(); }
    $("#release-error").textContent = e.message;
    $("#release-error").hidden = false;
    btn.disabled = false;
  } finally {
    state.busy.delete(n.node_id);
    refresh();
  }
}

// ---------- Toasts ----------

function toast(msg, bad = false) {
  const el = document.createElement("div");
  el.className = `toast${bad ? " bad" : ""}`;
  el.textContent = msg;
  $("#toasts").append(el);
  setTimeout(() => el.remove(), 4000);
}

// ---------- Polling / routing ----------

async function refresh() {
  try {
    const [nodes, cluster] = await Promise.all([api("GET", "/api/v1/nodes"), api("GET", "/api/v1/cluster")]);
    state.nodes = nodes;
    state.cluster = cluster;
    await Promise.all(hooks.refresh.map(f => f()));
    $("#conn").className = "conn";
    $("#conn").title = "Connected";
    renderNodes();
    renderSettings();
    if (state.selected) loadDetail();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    $("#conn").className = "conn down";
    $("#conn").title = `Can't reach the Controller: ${e.message}`;
  }
}

function startPolling() {
  stopPolling();
  state.pollTimer = setInterval(() => { if (!document.hidden) refresh(); }, POLL_MS);
}

function stopPolling() {
  if (state.pollTimer) clearInterval(state.pollTimer);
  state.pollTimer = null;
}

function route() {
  const tab = location.hash.replace("#", "") || "nodes";
  const known = VIEWS.includes(tab) ? tab : "nodes";
  for (const v of VIEWS) $(`#view-${v}`).hidden = v !== known;
  for (const a of document.querySelectorAll(".tabs a")) {
    if (a.dataset.tab === known) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  for (const f of hooks.route) f(known);
}

async function loadTrustInfo() {
  try {
    const t = await api("GET", "/trust-info");
    $("#ca-fp").textContent = t.ca_fp;
  } catch { /* the help text still works without it */ }
}

function showLogin() {
  stopPolling();
  loadTrustInfo();
  closeDialogs();
  $("#app").hidden = true;
  $("#drawer").hidden = true;
  $("#scrim").hidden = true;
  $("#login").hidden = false;
  $("#login-token").focus();
}

function closeDialogs() {
  for (const d of document.querySelectorAll("dialog[open]")) d.close();
}

async function showApp() {
  $("#login").hidden = true;
  $("#app").hidden = false;
  route();
  await refresh();
  startPolling();
}

// ---------- Wiring ----------

function wire() {
  $("#login-form").addEventListener("submit", async e => {
    e.preventDefault();
    const err = $("#login-error");
    err.hidden = true;
    try {
      await api("POST", "/session", { token: $("#login-token").value.trim() });
      $("#login-token").value = "";
      await showApp();
    } catch (ex) {
      err.textContent = ex.message || "Sign-in failed";
      err.hidden = false;
    }
  });

  $("#rotate-code").addEventListener("click", async e => {
    const btn = e.currentTarget;
    btn.disabled = true;
    try {
      const r = await api("POST", "/api/v1/settings/login-code");
      const out = $("#new-code");
      out.textContent = `New code: ${r.login_code}`;
      out.hidden = false;
      toast("New login code issued; the old one no longer works");
    } catch (ex) {
      if (ex instanceof AuthError) return showLogin();
      toast(ex.message, true);
    } finally {
      btn.disabled = false;
    }
  });

  $("#logout").addEventListener("click", async () => {
    try { await api("DELETE", "/session"); } catch { /* signing out anyway */ }
    showLogin();
  });

  document.addEventListener("click", e => {
    const act = e.target.closest("[data-act]");
    if (act) {
      e.stopPropagation();
      actions[act.dataset.act]?.(act.dataset.id, act);
      return;
    }
    const chip = e.target.closest("[data-filter]");
    if (chip) {
      state.filter = chip.dataset.filter;
      renderFilters();
      renderTable();
      return;
    }
    const row = e.target.closest("tr[data-id]");
    if (row) openDrawer(row.dataset.id);
  });

  $("#nodes-body").addEventListener("keydown", e => {
    const row = e.target.closest("tr[data-id]");
    if (row && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); openDrawer(row.dataset.id); }
  });

  $("#search").addEventListener("input", e => { state.search = e.target.value; renderTable(); });
  $("#drawer-close").addEventListener("click", closeDrawer);
  $("#scrim").addEventListener("click", closeDrawer);
  document.addEventListener("keydown", e => {
    if (e.key === "Escape" && !$("#drawer").hidden && !document.querySelector("dialog[open]")) closeDrawer();
  });

  $("#adopt-form").addEventListener("submit", e => {
    if (e.submitter?.value === "go") { e.preventDefault(); submitAdopt(); }
  });
  $("#release-form").addEventListener("submit", e => {
    if (e.submitter?.value === "go") { e.preventDefault(); submitRelease(); }
  });

  for (const r of document.querySelectorAll("input[name=mode]")) {
    r.addEventListener("change", () => setMode(r.value));
  }
  $("#igpu-on").addEventListener("change", e => setIGPU(e.target.checked));

  window.addEventListener("hashchange", route);
  document.addEventListener("visibilitychange", () => { if (!document.hidden && !$("#app").hidden) refresh(); });
}

// Buttons carry data-act="<name>" data-id="<id>"; other scripts add entries.
const actions = {
  adopt: id => openAdopt(id),
  release: id => openRelease(id),
  details: id => openDrawer(id),
};

async function boot() {
  wire();
  for (const f of hooks.wire ?? []) f();
  try {
    await api("GET", "/api/v1/cluster");
    await showApp();
  } catch {
    showLogin();
  }
}

// Deferred scripts all run before DOMContentLoaded, so models.js has
// registered its hooks by the time boot() starts.
document.addEventListener("DOMContentLoaded", boot);
