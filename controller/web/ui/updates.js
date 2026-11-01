// Update notifications: polls the Controller's GitHub update check, shows a
// pop-up when the configured branch has newer commits than this build, and
// the Updates card in Settings.
"use strict";

const UPDATE_POLL_MS = 60000;
const SNOOZE_MS = 24 * 3600 * 1000;

const upd = {
  status: null,
  shownFor: null, // sha the pop-up was already shown for in this page
  polledAt: 0,
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

async function loadUpdates(force) {
  if (!force && Date.now() - upd.polledAt < UPDATE_POLL_MS) return;
  upd.polledAt = Date.now();
  try {
    upd.status = await api("GET", "/api/v1/updates");
  } catch (e) {
    if (e instanceof AuthError) throw e;
    return;
  }
  renderUpdates();
}

// pending is true when there is an update the admin hasn't skipped.
function pending(s) {
  return !!(s && s.enabled && s.available && s.latest && s.latest.sha !== s.dismissed);
}

function renderUpdates() {
  const s = upd.status;
  $("#update-badge").hidden = !pending(s);
  if (s) {
    $("#update-badge").textContent = s.behind > 0 ? `Update available · ${s.behind} new` : "Update available";
  }
  renderUpdateCard();
  if (pending(s) && upd.shownFor !== s.latest.sha && Date.now() > snoozedUntil(s.latest.sha) && !document.querySelector("dialog[open]")) {
    openUpdateDialog();
  }
}

function openUpdateDialog() {
  const s = upd.status;
  if (!s || !s.latest) return;
  upd.shownFor = s.latest.sha;
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
  $("#update-steps").textContent = `cd OldAI            # your checkout of ${s.repo}
git pull
make build          # builds bin/linux-amd64/aiclusterd
sudo install bin/linux-amd64/aiclusterd /usr/local/bin/
sudo systemctl restart aiclusterd`;
  const gh = $("#update-github");
  gh.href = safeURL(s.compare_url || s.latest.url || `https://github.com/${s.repo}/commits/${s.branch}`);
  $("#update-dialog").showModal();
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
  $("#update-info").innerHTML = [
    ["This build", `${esc(cur.version || "")} · ${cur.commit ? `<code>${esc(shortSha(cur.commit))}</code>` : "no commit info"}${cur.modified ? " · local changes" : ""}${!isZeroTime(cur.time) ? ` · ${esc(new Date(cur.time).toLocaleString())}` : ""}`],
    ["Status", state + (s.note && s.enabled && !s.error ? `<div class="muted">${esc(s.note)}</div>` : "")],
    ["Latest on GitHub", s.latest ? `<a href="${esc(safeURL(s.latest.url))}" target="_blank" rel="noopener noreferrer"><code>${esc(shortSha(s.latest.sha))}</code></a> ${esc(s.latest.message)} <span class="muted">(${ago(s.latest.time)})</span>` : "—"],
    ["Last checked", s.checked_at ? ago(s.checked_at) : "—"],
  ].map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join("") +
    (pending(s) ? `<dt></dt><dd><button class="btn btn-sm" type="button" id="update-show">Show what's new</button></dd>` : "");
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
  $("#update-badge").addEventListener("click", openUpdateDialog);
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
  document.addEventListener("click", e => { if (e.target.closest("#update-show")) openUpdateDialog(); });
});
