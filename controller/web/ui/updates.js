// Updates: polls the Controller's GitHub update check, shows a pop-up when
// the configured branch has newer commits than this build, runs the rolling
// update (Controller first, then each node), and fills the Updates card in
// Settings.
"use strict";

const UPDATE_POLL_MS = 60000;
const ROLLOUT_POLL_MS = 3000;
const SNOOZE_MS = 24 * 3600 * 1000;

const upd = {
  status: null,
  shownFor: null, // sha the pop-up was already shown for in this page
  polledAt: 0,
  following: false, // the dialog shows rollout progress
  restarting: false, // the Controller didn't answer during a rollout
  timer: null,
};

function shortSha(s) { return (s || "").slice(0, 7); }

// Links come from GitHub's API: only ever allow plain https URLs.
function safeURL(u) { return /^https:\/\/[^\s"'<>]+$/.test(u || "") ? u : "#"; }

function snoozedUntil(sha) {
  try { return +(localStorage.getItem("aic.update.snooze." + sha) || 0); } catch { return 0; }
}

function snooze(sha) {
  try { localStorage.setItem("aic.update.snooze." + sha, String(Date.now() + SNOOZE_MS)); } catch { /* storage unavailable */ }
}

function rolloutActive(r) { return !!r && ["downloading", "controller", "slaves"].includes(r.phase); }

async function loadUpdates(force) {
  if (!force && Date.now() - upd.polledAt < UPDATE_POLL_MS) return;
  upd.polledAt = Date.now();
  const before = upd.status;
  try {
    upd.status = await api("GET", "/api/v1/updates");
    upd.restarting = false;
  } catch (e) {
    if (e instanceof AuthError) throw e;
    // While the Controller installs its update it is briefly unreachable.
    if (rolloutActive(before?.rollout)) {
      upd.restarting = true;
      renderUpdates();
      scheduleRolloutPoll();
    }
    return;
  }
  // The new Controller serves new dashboard code: load it.
  if (before && before.current.commit && upd.status.current.commit && before.current.commit !== upd.status.current.commit) {
    try { sessionStorage.setItem("aic.update.follow", upd.following ? "1" : ""); } catch { /* storage unavailable */ }
    location.reload();
    return;
  }
  renderUpdates();
  scheduleRolloutPoll();
}

function scheduleRolloutPoll() {
  clearTimeout(upd.timer);
  if (upd.restarting || rolloutActive(upd.status?.rollout)) {
    upd.timer = setTimeout(() => loadUpdates(true).catch(() => {}), ROLLOUT_POLL_MS);
  }
}

// pending is true when there is an update the admin hasn't skipped.
function pending(s) {
  return !!(s && s.enabled && s.available && s.latest && s.latest.sha !== s.dismissed);
}

function renderUpdates() {
  const s = upd.status;
  const badge = $("#update-badge");
  const r = s?.rollout;
  if (rolloutActive(r) || upd.restarting) {
    badge.hidden = false;
    badge.textContent = upd.restarting ? "Updating · Controller restarting" : `Updating · ${rolloutCount(r)}`;
  } else {
    badge.hidden = !pending(s);
    if (s) badge.textContent = s.behind > 0 ? `Update available · ${s.behind} new` : "Update available";
  }
  renderUpdateCard();
  if ($("#update-dialog").open && upd.following) {
    renderProgress($("#update-progress"));
    $("#update-title").textContent = progressTitle();
  }
  if (!rolloutActive(r) && pending(s) && upd.shownFor !== s.latest.sha && Date.now() > snoozedUntil(s.latest.sha) && !document.querySelector("dialog[open]")) {
    openUpdateDialog();
  }
}

function rolloutCount(r) {
  if (!r) return "";
  if (r.phase === "downloading") return "downloading";
  if (r.phase === "controller") return "Controller";
  const done = r.nodes.filter(n => n.state !== "waiting" && n.state !== "updating").length;
  return `${done}/${r.nodes.length} nodes`;
}

const NODE_STATE = {
  waiting: ["Waiting", "muted"],
  updating: ["Updating", "s-enrolling"],
  updated: ["Updated", "s-adopted"],
  failed: ["Failed", "s-offline"],
  skipped: ["Skipped", "s-pending"],
};

// renderProgress shows the current or last rollout in el.
function renderProgress(el) {
  const r = upd.status?.rollout;
  if (!r && !upd.restarting) { el.hidden = true; return; }
  el.hidden = false;
  const phase = r?.phase;
  const ctrlState = upd.restarting || phase === "controller" ? ["Installing · restarting", "s-enrolling"]
    : phase === "downloading" ? ["Downloading", "s-enrolling"]
    : phase === "failed" && !r.nodes.length ? ["Failed", "s-offline"]
    : ["Updated", "s-adopted"];
  let head;
  if (upd.restarting) head = `<p>The Controller is installing the update and restarting. This page reconnects by itself.</p>`;
  else if (phase === "done") head = `<p class="note good">${esc(r.message)}</p>`;
  else if (phase === "failed") head = `<p class="note bad">${esc(r.error)}</p>`;
  else head = `<p>${esc(r.message || "Updating")}…</p>`;
  const rows = [`<li><span>Controller</span><span class="pill ${ctrlState[1]}">${ctrlState[0]}</span></li>`]
    .concat((r?.nodes || []).map(n => {
      const [label, cls] = NODE_STATE[n.state] || [n.state, "muted"];
      return `<li><span>${esc(n.name)}${n.detail ? `<span class="muted"> · ${esc(n.detail)}</span>` : ""}</span><span class="pill ${cls}">${label}</span></li>`;
    }));
  if (phase === "controller" || phase === "downloading") rows.push(`<li class="muted"><span>Nodes are updated one at a time once the Controller is back.</span></li>`);
  el.innerHTML = `${head}<ul class="update-steps-list">${rows.join("")}</ul>` +
    (r ? `<p class="muted">Version <code>${esc(shortSha(r.target))}</code> · started ${ago(r.started_at)}</p>` : "");
}

function openUpdateDialog() {
  const s = upd.status;
  if (!s) return;
  if (rolloutActive(s.rollout) || upd.restarting) return followRollout();
  if (!s.latest) return;
  upd.shownFor = s.latest.sha;
  upd.following = false;
  $("#update-title").textContent = "Update available";
  $("#update-offer").hidden = false;
  $("#update-progress").hidden = true;
  $("#update-apply-error").hidden = true;
  for (const id of ["#update-skip", "#update-later", "#update-github"]) $(id).hidden = false;
  $("#update-close").hidden = true;
  const cur = s.current;
  const built = cur.commit ? `<code>${esc(shortSha(cur.commit))}</code>${!isZeroTime(cur.time) ? ` from ${esc(new Date(cur.time).toLocaleDateString())}` : ""}` : "an unknown commit";
  const count = s.behind > 0 ? `<strong>${s.behind} new commit${s.behind === 1 ? "" : "s"}</strong>` : "Newer code";
  $("#update-summary").innerHTML = `${count} on <strong>${esc(s.repo)}</strong> <code>${esc(s.branch)}</code> since this Controller was built (${built}).` +
    (cur.modified ? ` <span class="note">This build also has local changes.</span>` : "");
  const commits = s.commits.length ? s.commits : [s.latest];
  $("#update-commits").innerHTML = commits.map(c => `<li>
      <span class="uc-msg">${esc(c.message || "(no message)")}</span>
      <span class="uc-meta"><a href="${esc(safeURL(c.url))}" target="_blank" rel="noopener noreferrer"><code>${esc(shortSha(c.sha))}</code></a> · ${esc(c.author || "")} · ${ago(c.time)}</span>
    </li>`).join("") + (s.behind > commits.length ? `<li class="muted">…and ${s.behind - commits.length} more</li>` : "");
  $("#update-apply").hidden = !s.can_apply;
  $("#update-apply").disabled = false;
  $("#update-apply").textContent = "Update Controller and Slaves now";
  $("#update-apply-hint").hidden = !s.can_apply;
  $("#update-manual").hidden = s.can_apply;
  $("#update-blocked").textContent = s.apply_blocked || "";
  $("#update-steps").textContent = `cd OldAI            # your checkout of ${s.repo}
git pull
make build          # builds bin/linux-amd64/aiclusterd
sudo install bin/linux-amd64/aiclusterd /usr/local/bin/
sudo systemctl restart aiclusterd`;
  $("#update-github").href = safeURL(s.compare_url || s.latest.url || `https://github.com/${s.repo}/commits/${s.branch}`);
  if (!$("#update-dialog").open) $("#update-dialog").showModal();
}

function progressTitle() {
  const phase = upd.status?.rollout?.phase;
  if (upd.restarting) return "Updating the cluster";
  return phase === "done" ? "Cluster updated" : phase === "failed" ? "Update stopped" : "Updating the cluster";
}

// followRollout turns the dialog into the progress view.
function followRollout() {
  upd.following = true;
  $("#update-title").textContent = progressTitle();
  $("#update-offer").hidden = true;
  $("#update-apply-error").hidden = true;
  for (const id of ["#update-skip", "#update-later", "#update-github", "#update-apply"]) $(id).hidden = true;
  $("#update-close").hidden = false;
  renderProgress($("#update-progress"));
  if (!$("#update-dialog").open) $("#update-dialog").showModal();
}

async function applyUpdate() {
  const s = upd.status;
  const btn = $("#update-apply");
  const err = $("#update-apply-error");
  err.hidden = true;
  btn.disabled = true;
  btn.textContent = "Starting…";
  try {
    upd.status = await api("POST", "/api/v1/updates/apply", { commit: s.latest.sha });
    followRollout();
    renderUpdates();
    scheduleRolloutPoll();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    err.textContent = e.message;
    err.hidden = false;
    btn.disabled = false;
    btn.textContent = "Update Controller and Slaves now";
  }
}

function renderUpdateCard() {
  const s = upd.status;
  if (!s) return;
  const cur = s.current;
  let state;
  if (!s.enabled) state = `<span class="muted">${esc(s.note || "Off")}</span>`;
  else if (s.error) state = `<span class="note bad">${esc(s.error)}</span>`;
  else if (s.available) state = `<span class="pill s-enrolling">${s.behind > 0 ? `${s.behind} commit${s.behind === 1 ? "" : "s"} behind` : "Newer code available"}</span>${s.dismissed === s.latest?.sha ? ` <span class="muted">(skipped)</span>` : ""}`;
  else if (s.checked_at) state = `<span class="pill s-adopted">Up to date</span>`;
  else state = `<span class="muted">Not checked yet</span>`;
  const busy = rolloutActive(s.rollout);
  $("#update-info").innerHTML = [
    ["This build", `${esc(cur.version || "")} · ${cur.commit ? `<code>${esc(shortSha(cur.commit))}</code>` : "no commit info"}${cur.modified ? " · local changes" : ""}${!isZeroTime(cur.time) ? ` · ${esc(new Date(cur.time).toLocaleString())}` : ""}`],
    ["Status", state + (s.note && s.enabled && !s.error ? `<div class="muted">${esc(s.note)}</div>` : "")],
    ["Latest on GitHub", s.latest ? `<a href="${esc(safeURL(s.latest.url))}" target="_blank" rel="noopener noreferrer"><code>${esc(shortSha(s.latest.sha))}</code></a> ${esc(s.latest.message)} <span class="muted">(${ago(s.latest.time)})</span>` : "—"],
    ["Last checked", s.checked_at ? ago(s.checked_at) : "—"],
    ["In-place updates", s.can_apply ? "Available" : `<span class="muted">${esc(s.apply_blocked || "Unavailable")}</span>`],
  ].map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("") +
    (busy ? `<dt></dt><dd><button class="btn btn-sm" type="button" id="update-follow">Show progress</button></dd>`
      : s.available ? `<dt></dt><dd><button class="btn btn-sm${s.can_apply ? " btn-primary" : ""}" type="button" id="update-show">${s.can_apply ? "Update now…" : "Show what's new"}</button></dd>`
      : "");
  renderProgress($("#update-rollout"));
}

async function loadUpdateSettings() {
  try {
    const st = await api("GET", "/api/v1/settings");
    $("#update-on").checked = !!st.update_check;
    $("#update-repo").value = st.update_repo || "";
    $("#update-branch").value = st.update_branch || "";
  } catch (e) {
    if (e instanceof AuthError) showLogin();
  }
}

async function saveUpdateSettings() {
  const err = $("#update-form-error");
  err.hidden = true;
  try {
    await api("PUT", "/api/v1/settings", {
      update_check: $("#update-on").checked,
      update_repo: $("#update-repo").value.trim(),
      update_branch: $("#update-branch").value.trim(),
    });
    toast($("#update-on").checked ? "Update checks saved" : "Update checks turned off");
    await checkUpdatesNow();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    err.textContent = e.message;
    err.hidden = false;
  }
}

async function checkUpdatesNow() {
  const btn = $("#update-check");
  btn.disabled = true;
  btn.textContent = "Checking…";
  try {
    upd.status = await api("POST", "/api/v1/updates/check");
    upd.polledAt = Date.now();
    upd.shownFor = null; // an explicit check may show the pop-up again
    renderUpdates();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  } finally {
    btn.disabled = false;
    btn.textContent = "Check now";
  }
}

hooks.refresh.push(() => loadUpdates(false));
hooks.route.push(view => { if (view === "settings") { loadUpdateSettings(); loadUpdates(true); } });
hooks.wire.push(() => {
  // Reopen the progress view after the reload into the new dashboard.
  try {
    if (sessionStorage.getItem("aic.update.follow")) {
      sessionStorage.removeItem("aic.update.follow");
      loadUpdates(true).then(() => { if (upd.status?.rollout) followRollout(); }).catch(() => {});
    }
  } catch { /* storage unavailable */ }
  $("#update-badge").addEventListener("click", openUpdateDialog);
  $("#update-apply").addEventListener("click", applyUpdate);
  $("#update-close").addEventListener("click", () => { upd.following = false; $("#update-dialog").close(); });
  $("#update-later").addEventListener("click", () => {
    snooze(upd.status.latest.sha);
    $("#update-dialog").close();
    toast("We'll remind you tomorrow");
  });
  $("#update-skip").addEventListener("click", async () => {
    try {
      await api("POST", "/api/v1/updates/dismiss", { commit: upd.status.latest.sha });
      upd.status.dismissed = upd.status.latest.sha;
      $("#update-dialog").close();
      renderUpdates();
      toast("Skipped; you'll hear about the next update");
    } catch (e) {
      if (e instanceof AuthError) return showLogin();
      toast(e.message, true);
    }
  });
  $("#update-form").addEventListener("submit", e => { e.preventDefault(); saveUpdateSettings(); });
  $("#update-check").addEventListener("click", checkUpdatesNow);
  document.addEventListener("click", e => {
    if (e.target.closest("#update-show")) { upd.shownFor = null; openUpdateDialog(); }
    if (e.target.closest("#update-follow")) followRollout();
  });
});
