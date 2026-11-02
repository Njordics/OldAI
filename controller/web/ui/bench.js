// Benchmark page: measure how fast each model runs on each node (protocol
// §17). Uses helpers from app.js ($, api, esc, morph, toast, ago...).
"use strict";

const bench = {
  targets: [],
  results: [],
  job: null,
  picked: new Set(), // "model|node"
  loadedAt: 0,
};

const benchKey = t => `${t.model_id}|${t.node_id}`;

async function loadBench() {
  const v = await api("GET", "/api/v1/benchmarks");
  bench.targets = v.targets || [];
  bench.results = v.results || [];
  bench.job = v.job || null;
  bench.loadedAt = Date.now();
  const known = new Set(bench.targets.map(benchKey));
  for (const k of [...bench.picked]) if (!known.has(k)) bench.picked.delete(k);
}

function tps(v) {
  if (!v) return "—";
  return v >= 100 ? Math.round(v).toString() : v.toFixed(1);
}

function renderBench() {
  const running = !!bench.job?.running;
  const rows = bench.targets.map(t => {
    const k = benchKey(t);
    const where = t.pool?.length ? ` <span class="pill plain">split across ${t.pool.length + 1} nodes</span>` : "";
    return `<tr>
      <td><label class="check bench-pick"><input type="checkbox" data-bench-pick="${esc(k)}" ${bench.picked.has(k) ? "checked" : ""} ${running ? "disabled" : ""}> ${esc(t.model_id)}</label></td>
      <td>${esc(t.node_name)}${where}</td>
      <td class="hide-sm">${t.loaded ? "In memory" : `<span class="muted">On disk; loads first</span>`}</td>
      <td class="hide-sm num">${t.slots || 1}</td>
    </tr>`;
  }).join("");
  morph($("#bench-targets"), rows || `<tr><td colspan="4" class="muted">No models are ready on any node yet. Install one on the Models page.</td></tr>`);
  $("#bench-all").checked = bench.targets.length > 0 && bench.picked.size === bench.targets.length;
  $("#bench-all").disabled = running || !bench.targets.length;
  $("#bench-run").disabled = running || bench.picked.size === 0;
  $("#bench-run").textContent = running ? "Running…" : bench.picked.size > 1 ? `Run ${bench.picked.size} benchmarks` : "Run benchmark";

  const j = bench.job;
  const status = $("#bench-status");
  if (running) {
    status.hidden = false;
    status.className = "note";
    status.textContent = `Running ${j.done + 1} of ${j.targets.length}: ${j.current || ""}. Each takes 10 to 60 seconds and keeps that node busy.`;
  } else {
    status.hidden = true;
  }

  const best = Math.max(1, ...bench.results.filter(r => !r.error).map(r => r.gen_tps || 0));
  const res = bench.results.map(r => {
    if (r.error) {
      return `<tr><td class="num">${ago(r.at)}</td><td>${esc(r.model_id)}</td><td>${esc(r.node_name)}</td>
        <td colspan="6" class="bad-text">${esc(r.error)}</td></tr>`;
    }
    const pool = r.pool?.length ? ` <span class="muted">+${r.pool.length}</span>` : "";
    return `<tr>
      <td class="num">${ago(r.at)}</td>
      <td>${esc(r.model_id)}</td>
      <td>${esc(r.node_name)}${pool}</td>
      <td class="bench-bar-cell"><meter class="bench-meter" min="0" max="${best}" value="${r.gen_tps}"></meter><strong>${tps(r.gen_tps)}</strong>${r.approx ? `<span class="muted" title="The runtime didn't report token counts; estimated"> ~</span>` : ""}</td>
      <td class="num">${tps(r.prompt_tps)}</td>
      <td class="num hide-sm">${!r.ttft_ms ? "—" : r.ttft_ms < 1000 ? Math.round(r.ttft_ms) + " ms" : (r.ttft_ms / 1000).toFixed(1) + " s"}</td>
      <td class="num hide-md">${r.parallel > 1 ? `${r.parallel} × <span class="muted">→</span> ${tps(r.total_tps)}` : "1"}</td>
      <td class="num hide-md">${r.cold_start_s ? r.cold_start_s.toFixed(1) + " s" : `<span class="muted">—</span>`}</td>
      <td class="num hide-md">${r.cpu_temp_c ? `<span class="${r.hot ? "bad-text" : ""}">${Math.round(r.cpu_temp_c)} °C</span>` : "—"}</td>
    </tr>`;
  }).join("");
  morph($("#bench-results"), res || `<tr><td colspan="9" class="muted">No results yet.</td></tr>`);
  $("#bench-clear").hidden = !bench.results.length || running;
}

async function runBench() {
  const targets = bench.targets.filter(t => bench.picked.has(benchKey(t))).map(t => ({ model_id: t.model_id, node_id: t.node_id }));
  const parallel = $("#bench-parallel").value === "slots" ? 0 : 1;
  try {
    bench.job = await api("POST", "/api/v1/benchmarks", { targets, parallel });
    renderBench();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

async function clearBench() {
  if (!confirm("Forget every benchmark result?")) return;
  try {
    await api("DELETE", "/api/v1/benchmarks");
    await loadBench();
    renderBench();
  } catch (e) {
    if (e instanceof AuthError) return showLogin();
    toast(e.message, true);
  }
}

hooks.route.push(view => {
  if (view === "bench") loadBench().then(renderBench).catch(e => { if (e instanceof AuthError) showLogin(); else toast(e.message, true); });
});
hooks.refresh.push(async () => {
  if ($("#view-bench").hidden) return;
  // Every poll while a run is going; otherwise every few seconds.
  if (bench.job?.running || Date.now() - bench.loadedAt > 5000) await loadBench();
  renderBench();
});
hooks.wire.push(() => {
  $("#bench-run").addEventListener("click", runBench);
  $("#bench-clear").addEventListener("click", clearBench);
  $("#bench-all").addEventListener("change", e => {
    bench.picked = e.target.checked ? new Set(bench.targets.map(benchKey)) : new Set();
    renderBench();
  });
  $("#bench-targets").addEventListener("change", e => {
    const k = e.target.dataset?.benchPick;
    if (!k) return;
    if (e.target.checked) bench.picked.add(k);
    else bench.picked.delete(k);
    renderBench();
  });
});
