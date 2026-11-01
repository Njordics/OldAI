# AI Cluster — Protocol Specification v1

Status: **draft for review** (build step 1)

Conventions:
- All payloads are JSON (UTF-8), `snake_case`, timestamps RFC 3339 UTC, sizes in **bytes**, temperatures in °C, ratios 0.0–1.0.
- Every message carries `"v": 1`. Receivers ignore unknown fields and reject unknown major versions with `426 Upgrade Required`.
- IDs: `node_id` and `cluster_id` are UUIDv4; `job_id` is ULID.
- Errors: `{"v":1,"error":{"code":"<snake_case>","message":"…","retryable":bool}}` with a matching HTTP status.
- Machine-readable JSON Schemas for every message live in `/common/schema/` and are the source of truth; this document explains them.

---

## 1. Identity and keys

| Item | Where | Notes |
|---|---|---|
| `node_id` | `/var/lib/aicluster/identity.json` | Generated at first boot. Survives reboots; a reinstall creates a new one. |
| `hw_fingerprint` | computed | `sha256(dmi.product_uuid + "\n" + sorted(physical MACs).join(","))`, hex. Used only to recognise reinstalled hardware. |
| Bootstrap key | `/var/lib/aicluster/bootstrap.key` | Ed25519, self-signed cert, CN=`node_id`. Used for the pre-adoption TLS listener. |
| Node key | `/var/lib/aicluster/node.key` | ECDSA P-256, generated during enrollment; CSR sent to Controller. |
| Node cert | `/var/lib/aicluster/node.crt` | Issued by the cluster CA. EKU serverAuth + clientAuth. URI SAN `aicluster://node/<node_id>`. 30-day lifetime. |
| Pinned CA | `/var/lib/aicluster/ca.crt` | Written once at enrollment; replaced only through release/factory-reset or a signed CA rollover (§5.6). |
| Controller cert | Controller | Issued by the same CA. URI SAN `aicluster://controller/<cluster_id>`. |

**Peer verification rule (both sides, post-adoption):** chain verifies to the pinned CA, not revoked, and the URI SAN matches the expected role (and `node_id` where known). Hostnames and IPs are **not** checked.

---

## 2. Discovery

### 2.1 mDNS (primary)

Slave registers `_aicluster-slave._tcp.local.` on port **7450**.
Instance name: `aic-<first 8 hex of node_id>`.

TXT records (each ≤ 255 bytes):

| Key | Example | Meaning |
|---|---|---|
| `v` | `1` | protocol major |
| `id` | `7c0e…` | full node_id |
| `st` | `unadopted` \| `enrolling` \| `adopted` | agent state |
| `fp` | `sha256:ab12…` | sha256 of the current TLS server cert (bootstrap cert before adoption, node cert after) |
| `cl` | `5d1f…` \| empty | cluster_id once adopted |
| `hw` | `nvidia:1x24G` \| `amd:2x16G` \| `cpu` | GPU summary, for display only |
| `pc` | `1` \| `0` | pairing code required |
| `ag` | `1.0.3` | agent version |

Controller registers `_aicluster-ctrl._tcp.local.` on port **7443** with TXT `v=1`, `cl=<cluster_id>`, `fp=sha256:<controller cert>`. Adopted Slaves browse this to re-find the Controller after IP changes; they still authenticate it by CA pin (§1).

### 2.2 UDP beacon (fallback)

- Slave → `255.255.255.255:47800` and to each interface's directed broadcast.
- Interval: **5 s** while `unadopted`/`enrolling`, **30 s** while `adopted`, with ±20 % jitter.
- Payload (≤ 1200 bytes, single datagram):

```json
{
  "v": 1,
  "kind": "slave_beacon",
  "node_id": "7c0e9a52-…",
  "state": "unadopted",
  "port": 7450,
  "fp": "sha256:ab12…",
  "cluster_id": null,
  "hw": "nvidia:1x24G",
  "pairing_required": false,
  "agent_version": "1.0.3",
  "addrs": ["192.168.1.42"],
  "ts": "2026-11-01T10:00:00Z"
}
```

- Controller → `255.255.255.255:47800` every 30 s: `{"v":1,"kind":"controller_beacon","cluster_id":"…","port":7443,"fp":"sha256:…","addrs":[…]}`.
- Beacons are **unauthenticated hints**. They only trigger a TLS connection; nothing is trusted until the handshake verifies.

### 2.3 Registry semantics (Controller)

A sighting = `(node_id, addr, port, fp, state, ts, source=mdns|beacon)`. The registry keeps the latest sighting per `node_id`. An `unadopted` node not seen for 5 min is removed from the "discovered" list (not from the DB if it was ever adopted).

---

## 3. Adoption handshake

Pre-adoption listener: Slave `:7450`, TLS 1.3 with the bootstrap cert, **no client cert required**, only `/v1/enroll/*` and `/v1/info` routes enabled.

The Controller connects to the announced address and **aborts if the server cert fingerprint ≠ announced `fp`**.

### 3.1 `GET /v1/enroll/hello`

```json
{
  "v": 1,
  "node_id": "7c0e…",
  "hw_fingerprint": "e3b0…",
  "nonce_s": "base64(32 bytes)",
  "pairing": "none",            // "none" | "cpace"
  "inventory": { …§6.1… },
  "agent_version": "1.0.3"
}
```

### 3.2 Pairing (only when `pairing == "cpace"`)

The Slave shows a 6-digit code on its console (rotated every 10 min and after 3 failed attempts).
CPace (draft-irtf-cfrg-cpace, Ristretto255/SHA-512) is run with:
- password = pairing code typed into Controller CLI/UI
- channel identifier = `"aicluster-v1" || nonce_s || fp_bootstrap`

```
POST /v1/enroll/pake/start  {"v":1, "ya": "base64", "nonce_c": "base64"}   → {"v":1, "yb": "base64"}
POST /v1/enroll/pake/confirm {"v":1, "tag_c": "base64"}                     → {"v":1, "tag_s": "base64"}
```

The resulting shared key `K` is used to MAC the CSR exchange below (`mac` fields). Without pairing, `mac` fields are omitted.

### 3.3 `POST /v1/enroll/csr`

Request:
```json
{
  "v": 1,
  "cluster_id": "5d1f…",
  "ca_cert_pem": "-----BEGIN CERTIFICATE-----…",
  "nonce_c": "base64(32 bytes)",
  "nonce_s": "base64(echo)",
  "mac": "base64(HMAC-SHA256(K, 'csr-req'||cluster_id||sha256(ca_cert)||nonce_s||nonce_c))"
}
```
Response:
```json
{
  "v": 1,
  "csr_pem": "-----BEGIN CERTIFICATE REQUEST-----…",
  "mac": "base64(HMAC-SHA256(K, 'csr-resp'||sha256(csr_der)||nonce_c))"
}
```
The Slave enters `enrolling`, locks to this `cluster_id` + `nonce_c` for 120 s, and returns `409 enrollment_in_progress` to anyone else.

### 3.4 `POST /v1/enroll/complete`

```json
{
  "v": 1,
  "node_cert_pem": "…",
  "ca_chain_pem": "…",
  "controller": {
    "cluster_id": "5d1f…",
    "endpoints": ["https://192.168.1.10:7443"],
    "cert_fp": "sha256:…"
  },
  "node_name": "gpu-box-1",
  "heartbeat_interval_s": 5,
  "desired_state": { …§5.1…, "generation": 0 },
  "mac": "base64(HMAC-SHA256(K, 'complete'||sha256(body without mac)))"
}
```

Slave actions, atomically (write to temp dir, fsync, rename):
1. Verify the cert matches its CSR key and chains to `ca_cert_pem`.
2. Persist cert, CA pin, controller info. State → `adopted`.
3. Restart `:7450` in **mTLS-only** mode (§4). Close enrollment routes.
4. Send the first heartbeat (§5.3) within 5 s.

Response `200 {"v":1,"ok":true}` is sent before step 3.

The Controller considers adoption complete on the **first valid mTLS heartbeat**. If none arrives within 60 s, it retries enrollment from §3.1 (the Slave, still `adopted` with a cert from this cluster, answers `/v1/info` with `state=adopted` and the Controller simply waits for the heartbeat instead).

### 3.5 Errors

| Code | When |
|---|---|
| `already_adopted` (409) | Slave is adopted by a different cluster. |
| `enrollment_in_progress` (409) | Another Controller holds the 120 s lock. |
| `pairing_required` (403) | CSR requested without completing CPace. |
| `pairing_failed` (403) | Wrong code; counts towards rotation. |
| `bad_mac` (403) | MAC mismatch. |

---

## 4. Command API (Controller → Slave)

Base URL: `https://<slave>:7450`, mTLS, client cert must carry `aicluster://controller/<pinned cluster_id>`.

| Method & path | Purpose |
|---|---|
| `GET /v1/info` | node_id, state, versions (also available pre-adoption, unauthenticated, minus inventory details) |
| `GET /v1/inventory` | full inventory (§6.1); `?refresh=1` re-probes hardware |
| `PUT /v1/desired-state` | replace desired state (§5.1). Idempotent. `409 stale_generation` if `generation` < current. Returns `202 {"accepted_generation": n}` |
| `GET /v1/observed-state` | full observed state (§5.2), same as in heartbeat but with per-step detail |
| `POST /v1/ops/drain` | stop accepting new inference; `{"timeout_s":120}` → `202 {job_id}` |
| `POST /v1/ops/undrain` | resume |
| `POST /v1/ops/reboot` | `{"reason": "…"}` → `202 {job_id}` |
| `POST /v1/ops/release` | `{"keep_models": true}` → `202 {job_id}`; see §5.5 |
| `POST /v1/ops/rotate-ca` | CA rollover (§5.6) |
| `GET /v1/jobs/{job_id}` | `{"job_id","kind","state":"queued|running|succeeded|failed","progress":0.0-1.0,"message","started_at","finished_at"}` |
| `GET /v1/logs?unit=agent|runtime&since=…&follow=1` | NDJSON stream of journald entries |
| `ANY /v1/infer/{runtime}/{path…}` | reverse proxy to local runtime, e.g. `/v1/infer/ollama/v1/chat/completions`. Streams are passed through unbuffered. |
| `GET /metrics` | Prometheus (also on `:9464` for scrapers) |
| `GET /v1/rpc-tunnel/{group}` | **Node-to-node**, not Controller: only the leader of pool `{group}` (client cert `aicluster://node/<leader>`). Without `Upgrade` returns `{"epoch"}` of the local rpc-server; with `Upgrade: aicluster-rpc` answers `101` and splices to it. |

Pooled models add to `DesiredModel`: `role` (`leader` \| `worker`) and `pool` `{group_id, leader, members[{node_id, endpoint, share, leader}], layers, parallel}`; `ObservedModel` echoes `role`. The gateway reaches a pool through the leader's `/v1/infer/llamacpp:<assignment_id>/v1/...`.

All mutating ops return a `job_id`. Jobs survive agent restarts (persisted in `/var/lib/aicluster/jobs/`).

---

## 5. State model

### 5.1 Desired state (Controller → Slave)

```json
{
  "v": 1,
  "generation": 7,
  "node_name": "gpu-box-1",
  "allow_reboot": false,
  "drivers": { "target": "auto" },                  // "auto" | "nvidia-550" | "rocm-6.2" | "intel" | "cpu"
  "agent":   { "version": "1.0.4" },
  "os":      { "apply_updates": false, "package_set_version": "2026.10.2" },
  "runtimes": [
    { "kind": "ollama", "version": "0.9.1",
      "env": { "OLLAMA_NUM_PARALLEL": "4", "OLLAMA_MAX_LOADED_MODELS": "3" } },
    { "kind": "whisper", "version": "1.1.0" }
  ],
  "models": [
    { "model_id": "qwen2.5:7b-instruct", "variant": "q5_K_M", "runtime": "ollama",
      "source": { "type": "ollama", "ref": "library/qwen2.5:7b-instruct-q5_K_M", "digest": "sha256:…" },
      "residency": "resident", "ctx": 8192, "parallel": 4 },
    { "model_id": "llama3.1:70b", "variant": "Q4_K_M", "runtime": "llamacpp",
      "source": { "type": "blob", "path": "hf/bartowski/…/Meta-Llama-3.1-70B-Instruct-Q4_K_M.gguf", "sha256": "…", "size": 42520413344 },
      "residency": "on_demand", "ctx": 8192, "gpu_layers": 48, "parallel": 1 }
  ],
  "gc_grace_s": 86400
}
```

Reconcile order: agent → drivers → os → runtimes → models. A failed step blocks later steps and is reported in `conditions`.

### 5.2 Observed state (in heartbeat and `/v1/observed-state`)

```json
{
  "observed_generation": 7,
  "phase": "ready",                // "provisioning" | "ready" | "degraded" | "draining"
  "reboot_required": false,
  "conditions": [
    { "type": "DriversReady",  "status": true,  "reason": "", "since": "…" },
    { "type": "RuntimesReady", "status": true,  "reason": "", "since": "…" },
    { "type": "ModelsReady",   "status": false, "reason": "pulling llama3.1:70b", "since": "…" }
  ],
  "versions": { "agent": "1.0.4", "os": "2026.10.2", "kernel": "6.8.0-48", "driver": "nvidia 550.120",
                "runtimes": { "ollama": "0.9.1" } },
  "models": [
    { "model_id": "qwen2.5:7b-instruct", "variant": "q5_K_M", "runtime": "ollama",
      "state": "loaded",           // "pending" | "downloading" | "verifying" | "installed" | "loading" | "loaded" | "failed"
      "progress": 1.0, "bytes_done": 5444000000, "bytes_total": 5444000000,
      "vram_used": 6100000000, "error": null }
  ]
}
```

### 5.3 Heartbeat (Slave → Controller)

`POST https://<controller>:7443/v1/nodes/{node_id}/heartbeat`, mTLS, every `heartbeat_interval_s` (default 5 s) ±10 % jitter. Full observed state is included when it changed or every 60 s; otherwise `observed` is omitted and only `seq` + `health` are sent.

```json
{
  "v": 1,
  "node_id": "7c0e…",
  "seq": 18234,
  "boot_id": "c1d2…",
  "ts": "2026-11-01T10:00:05Z",
  "addrs": ["192.168.1.42"],
  "health": {
    "uptime_s": 86400,
    "load1": 2.31,
    "cpu_util": 0.42,
    "mem_total": 68719476736, "mem_avail": 30000000000,
    "disk_free": 812000000000,
    "gpus": [
      { "index": 0, "uuid": "GPU-…", "util": 0.87, "mem_used": 20100000000, "mem_total": 25769803776,
        "temp_c": 71, "power_w": 280, "throttled": false }
    ],
    "inference": { "in_flight": 3, "slots": 4, "queue": 0, "tokens_per_s_1m": 412.5, "draining": false }
  },
  "observed": { …§5.2… }
}
```

Response (piggy-backed control):
```json
{
  "v": 1,
  "desired_generation": 8,
  "cert_renew": false,
  "controller_endpoints": ["https://192.168.1.10:7443"],
  "heartbeat_interval_s": 5
}
```
If `desired_generation` > the Slave's current, the Slave fetches it with `GET /v1/nodes/{id}/desired-state` (covers the case where the Controller's push failed). If `cert_renew` is true, or the cert is past 2/3 of its lifetime, the Slave calls §5.4.

Controller liveness: no valid heartbeat for **3 × interval** → `offline`. A `boot_id` change → node rebooted; Controller re-sends desired state.

### 5.4 Certificate renewal

`POST /v1/nodes/{node_id}/renew` (mTLS with current cert) `{"v":1,"csr_pem":"…"}` → `{"v":1,"node_cert_pem":"…","ca_chain_pem":"…"}`. The Slave may keep its key (new CSR, same key) or rotate; default rotates. If the cert has already **expired**, the Slave falls back to the enrollment flow with `hello.reenroll=true`; auto mode accepts it if `hw_fingerprint` and `node_id` match the DB record.

### 5.5 Release / factory reset

`POST /v1/ops/release` → job:
1. Drain (timeout 60 s).
2. Stop runtimes; remove models unless `keep_models`.
3. Delete node key/cert/CA pin/controller info; generate **new bootstrap key** (keeps `node_id`).
4. State → `unadopted`; restart listener in pre-adoption mode; resume 5 s beacons.

Controller revokes the cert and deletes the node's assignments. Local escape hatch: `aicluster-agent factory-reset` on the console (requires physical/console access, does the same steps, and also regenerates `node_id`).

### 5.6 CA rollover

Controller generates new CA, cross-signs it with the old one, then for each node: `POST /v1/ops/rotate-ca {"new_ca_pem","cross_cert_pem"}` → Slave verifies the cross-signature against the pinned CA, pins both, renews its cert under the new CA, and drops the old pin when the Controller's heartbeat response indicates `ca_rotation_complete`.

---

## 6. Hardware inventory

### 6.1 Schema

```json
{
  "v": 1,
  "collected_at": "…",
  "host": { "hostname": "gpu-box-1", "os": "Ubuntu 24.04.1 LTS", "kernel": "6.8.0-48-generic",
            "product": "ASUS PRIME X670", "product_uuid": "…" },
  "cpu": { "model": "AMD Ryzen 9 7950X", "vendor": "amd", "sockets": 1, "cores": 16, "threads": 32,
           "flags": ["avx2", "avx512f", "avx512_vnni"] },
  "memory": { "total": 68719476736 },
  "disks": [ { "mount": "/var/lib/aicluster", "total": 2000000000000, "free": 1800000000000, "rotational": false } ],
  "gpus": [
    { "index": 0, "vendor": "nvidia", "model": "NVIDIA GeForce RTX 4090", "pci_id": "10de:2684",
      "vram": 25769803776, "compute": { "api": "cuda", "capability": "8.9" },
      "driver": { "installed": true, "version": "550.120" } }
  ],
  "accel_summary": { "best": "cuda", "total_vram": 25769803776, "gpu_count": 1 },
  "network": [ { "iface": "enp5s0", "mac": "…", "speed_mbps": 2500, "addrs": ["192.168.1.42/24"] } ]
}
```

### 6.2 Detection sources

| Field | Source |
|---|---|
| CPU | `/proc/cpuinfo`, `lscpu -J` |
| RAM | `/proc/meminfo` |
| Disk | `statfs` on model dir |
| GPU (pre-driver) | PCI scan of `/sys/bus/pci/devices/*` class `0x03xx`, vendor `10de`/`1002`/`8086`; model names from the bundled `pci.ids`; VRAM estimated from a bundled table until the driver is present |
| GPU (post-driver) | NVIDIA: NVML via `nvidia-smi --query-gpu … --format=csv`; AMD: `amd-smi`/`rocm-smi --json`; Intel: `xpu-smi` / sysfs `lmem` |
| CUDA capability | NVML `cuda_compute_capability`; ROCm `gfx` target from `rocminfo` |
| Network speed | `/sys/class/net/*/speed`, physical interfaces only |

---

## 7. Catalog entry schema

```yaml
# /catalog/qwen2.5.yaml
id: qwen2.5
family: Qwen 2.5
license: { spdx: Apache-2.0, url: https://…, commercial_use: true }
capabilities: [chat, code, tools]
priority: 80                    # higher = preferred by fit-all / spread
tags: [general]
variants:
  - id: qwen2.5:7b-instruct
    params_b: 7.6
    arch: { layers: 28, kv_heads: 4, head_dim: 128, ctx_max: 32768 }
    ctx_default: 8192
    sources:
      - runtime: ollama
        quants:
          - { quant: q4_K_M, ref: library/qwen2.5:7b-instruct-q4_K_M, size: 4683087232 }
          - { quant: q5_K_M, ref: library/qwen2.5:7b-instruct-q5_K_M, size: 5444831744 }
          - { quant: q8_0,   ref: library/qwen2.5:7b-instruct-q8_0,   size: 8098525184 }
      - runtime: vllm
        quants:
          - { quant: bf16, hf: Qwen/Qwen2.5-7B-Instruct, revision: main, size: 15231233024 }
          - { quant: awq,  hf: Qwen/Qwen2.5-7B-Instruct-AWQ, revision: main, size: 5580000000 }
    min: { vram: 5000000000, ram: 8000000000 }   # sanity floor; fit uses computed numbers
aliases: { "qwen": "qwen2.5:7b-instruct" }
```

Quant preference order (best first): `bf16/fp16 > q8_0 > q6_K > q5_K_M > awq/gptq > q4_K_M > q3_K_M`. Catalog validated against `/catalog/schema.json` in CI and at load.

---

## 8. Controller node API (Slave → Controller, `:7443`, mTLS)

| Method & path | Purpose |
|---|---|
| `POST /v1/nodes/{id}/heartbeat` | §5.3 |
| `GET /v1/nodes/{id}/desired-state` | pull fallback |
| `POST /v1/nodes/{id}/renew` | §5.4 |
| `POST /v1/nodes/{id}/events` | async events: `{"kind":"model_failed|driver_failed|reboot|oom|thermal_throttle", "detail":{…}}` |
| `GET /v2/…` | OCI distribution API (read-only) — pull-through cache of `registry.ollama.ai`, used by the Slave's loopback shim |
| `GET /blobs/{path}` | GGUF / safetensors / wheel files from the model cache; supports `Range`; response header `X-Content-SHA256` |
| `GET /apt/{…}` | apt proxy (Ubuntu archive, NVIDIA CUDA repo, ROCm repo) |
| `GET /repo/{…}` | signed aicluster apt repo (agent, runtimes, whisper) |

Cache semantics: on a miss, the Controller fetches upstream once (concurrent requests for the same blob are coalesced) while streaming to the requester. In offline mode, a miss returns `404 not_cached`.

---

## 9. Public API (gateway, `:8080`)

OpenAI-compatible, `Authorization: Bearer <api key>`:

- `GET /v1/models` — union of models that are `loaded` or `installed` on at least one `ready` node, plus aliases. Extra field `aicluster: {replicas, resident, capabilities}`.
- `POST /v1/chat/completions`, `POST /v1/completions` (incl. `stream: true`, tools when the runtime supports it)
- `POST /v1/embeddings`
- `POST /v1/audio/transcriptions` (whisper runtime)

Routing:
1. Resolve `model` via aliases → `model_id`. Unknown → `404 model_not_found`.
2. Candidates = nodes `ready`, not draining, with the model `loaded` (resident first), then `installed` (on_demand; incurs load).
3. Score = `in_flight / slots` (ties: lower GPU util, then lower recent p50 latency). Pick lowest.
4. All candidates at capacity → enqueue in a per-model FIFO (`max_queue` default 64, `queue_timeout` default 60 s) → `429 queue_full` / `503 queue_timeout` with `Retry-After`.
5. Failover: connect error, TLS error, or 502/503 **before the first response byte** → mark the node suspect for 10 s and retry on the next candidate (max 2 retries). Never retry after bytes are streamed.

API keys: `aic_` + 40 base32 characters (200 random bits), created via CLI (`aicluster keys create`) or the dashboard's Connect page and shown once. Stored as SHA-256 (a slow password hash adds nothing for a random secret this long, and the gateway checks the key on every request); the first 8 characters are kept to look the row up and to display. Optional per-key `models` allow-list and `rpm` limit. Errors use OpenAI's `{"error":{"message","type","code"}}` shape (`missing_api_key`, `invalid_api_key`, `model_not_allowed`, `model_not_found`, `rate_limit_exceeded`, `queue_full`, `queue_timeout`, `no_healthy_node`).

Code tab workspace (files kept in the Controller's database, so they follow the admin to any browser and are covered by backups; max 1 MB per file, 500 files; paths are relative, no `..`):

```
GET    /api/v1/workspace/files              list (no contents)
GET    /api/v1/workspace/file?path=a/b.js   one file with content
PUT    /api/v1/workspace/file  {path, content}
POST   /api/v1/workspace/rename  {from, to}
DELETE /api/v1/workspace/file?path=...
GET    /api/v1/workspace/zip                everything as a .zip
GET    /preview                             sandbox page for running HTML/CSS/JS (own CSP: sandbox, no network)
```

Update notifications (the Controller compares the git commit it was built from with the configured GitHub branch):

```
GET  /api/v1/updates             {enabled, repo, branch, current{version,commit,time,modified}, latest, available,
                                  behind, commits[] (newest first), compare_url, checked_at, error, note, dismissed}
POST /api/v1/updates/check       check now
POST /api/v1/updates/dismiss     {commit}   "skip this version"; the pop-up returns for anything newer
PUT  /api/v1/settings            {update_check: bool, update_repo: "owner/name", update_branch: "main"}
```

The dashboard's chat playground calls the same routing code through `POST /api/v1/playground/chat/completions` with the admin session instead of a key. Response header `X-AICluster-Node` names the serving node (can be disabled).

---

## 10. Management API (`:8090`)

REST over HTTPS (self-signed by the cluster CA; CLI pins it). Auth: admin token (created at install, shown on Controller console, rotatable), or unix socket as root. Endpoints mirror the CLI:

```
GET    /api/v1/nodes                        GET  /api/v1/nodes/{id}
POST   /api/v1/nodes/{id}/adopt   {pairing_code?}
POST   /api/v1/nodes/{id}/release {keep_models}
POST   /api/v1/nodes/{id}/drain | /undrain | /reboot
GET    /api/v1/catalog                      catalog + fit of every model on every adopted node + Controller cache state
GET    /api/v1/models                       every assignment with live state (fetching → downloading → loaded) and progress
POST   /api/v1/models/install  {model_id, quant?, nodes?[] | replicas?, residency?}   all-or-nothing; 409 does_not_fit with reasons
POST   /api/v1/assignments/{id}/swap  {model_id, quant?}   replace in one step; the old model's memory counts as free
GET    /api/v1/assignments/{id}/swap-options               fit of every model on that node with the old one removed
POST   /api/v1/assignments/{id}/retry                      restart a failed Controller download
DELETE /api/v1/assignments/{id}                            unload now, delete from the node's disk after gc_grace_s
GET    /api/v1/policy        PUT  /api/v1/policy
POST   /api/v1/plan?dry_run=1
POST   /api/v1/rollouts  {target: agent|runtime|os, version, max_unavailable}
GET    /api/v1/rollouts/{id}
GET    /api/v1/settings      PUT  /api/v1/settings   (adoption_mode, offline, refresh schedule…)
GET    /api/v1/keys          POST /api/v1/keys        DELETE /api/v1/keys/{id}
GET    /api/v1/events?follow=1     (SSE: node state changes, job progress — drives the dashboard)
GET    /api/v1/nodes/{id}/logs?follow=1
```

CLI mapping: `aicluster nodes [ls|show|adopt|release|drain|reboot]`, `aicluster models [ls|assign|unassign]`, `aicluster catalog [ls|refresh]`, `aicluster policy [show|set]`, `aicluster plan [--apply]`, `aicluster rollout …`, `aicluster keys …`, `aicluster settings …`, `aicluster offline on|off`, `aicluster backup|restore`.

---

## 11. Versioning and compatibility

- Protocol major `v` changes only on breaking changes. The Controller supports agents of the current and previous major so that rolling agent upgrades work.
- Agent advertises `agent_version`; Controller refuses to send desired-state fields unknown to that version (feature flags derived from version).
