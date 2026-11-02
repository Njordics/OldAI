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
  "allow_reboot": true,                             // set while a driver may need a reboot (§14)
  "drivers": { "vendor": "nvidia", "name": "nvidia-driver-580-server-open", "version": "580.178.04-0ubuntu0.24.04.1",
               "packages": ["nvidia-headless-no-dkms-580-server-open", "nvidia-utils-580-server",
                            "linux-modules-nvidia-580-server-open-generic"],
               "mirror": "/apt/ubuntu", "suites": ["noble", "noble-updates", "noble-security"],
               "components": ["main", "restricted"] },
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
  ],
  "driver": { "name": "nvidia-driver-580-server-open", "state": "ready", "version": "580.178.04" },
  "gpus": [ /* the current GPU list, as in §6.1 */ ]
}
```

`driver.state` is `installing`, `rebooting`, `reboot_required`, `ready` or `failed` (§14).

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
  "host": { "hostname": "gpu-box-1", "os": "Ubuntu 24.04.1 LTS", "codename": "noble", "kernel": "6.8.0-48-generic",
            "product": "ASUS PRIME X670", "product_uuid": "…" },
  "cpu": { "model": "AMD Ryzen 9 7950X", "vendor": "amd", "sockets": 1, "cores": 16, "threads": 32,
           "flags": ["avx2", "avx512f", "avx512_vnni"] },
  "memory": { "total": 68719476736 },
  "disks": [ { "mount": "/var/lib/aicluster", "total": 2000000000000, "free": 1800000000000, "rotational": false } ],
  "gpus": [
    { "index": 0, "vendor": "nvidia", "model": "NVIDIA GeForce RTX 4090", "pci_id": "10de:2684",
      "bus_id": "0000:01:00.0", "modalias": "pci:v000010DEd00002684sv00001043sd000088E2bc03sc00i00",
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
| GPU (pre-driver) | PCI scan of `/sys/bus/pci/devices/*` class `0x03xx`, vendor `10de`/`1002`/`8086`: PCI ID, bus address and `modalias`. No model name or memory until a driver is loaded. |
| GPU names | `/usr/share/misc/pci.ids` |
| GPU (post-driver) | NVIDIA: `nvidia-smi --query-gpu=pci.bus_id,name,memory.total,driver_version,compute_cap`. Intel and AMD: `compute.api = "vulkan"` once the kernel driver is bound, a render node exists and Mesa's Vulkan driver is installed; AMD memory from `mem_info_vram_total`. |
| CUDA capability | `nvidia-smi`'s `compute_cap` |
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
| `GET /v1/update/bundle` | the update bundle being rolled out (§13); any adopted node |
| `GET /blobs/{path}` | GGUF / safetensors / wheel files from the model cache; supports `Range`; response header `X-Content-SHA256` |
| `GET /v1/nodes/{id}/blob-source/{digest}` | where to download a model file from: `{node_id, endpoint}` of a peer, or `{}` for the Controller (§16); `?failed=<node_id>` reports a peer that failed |
| `GET /apt/ubuntu/{…}` | the Controller's copy of Ubuntu's archive, for GPU drivers (§14); any adopted node |
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
POST /api/v1/updates/apply       {commit}   rolling update to the branch's newest commit (§13); 409 if it isn't the
                                            newest any more or an update is running, 502 if its bundle isn't published
PUT  /api/v1/settings            {update_check: bool, update_repo: "owner/name", update_branch: "main"}
```

`GET /api/v1/updates` also returns:
- `can_apply`, and `apply_blocked` with the reason this Controller can't update itself.
- `rollout`, the current or last update:
  - `target`, `message`, `error`;
  - `phase`: `downloading`, `controller`, `slaves`, `done` or `failed`;
  - `nodes[]`, each with a `detail` and a `state`: `waiting`, `updating`, `updated`, `failed` or `skipped`.

The dashboard's chat playground calls the same routing code through `POST /api/v1/playground/chat/completions` with the admin session instead of a key. Response header `X-AICluster-Node` names the serving node (can be disabled).

---

## 10. Management API (`:8090`)

REST over HTTPS (self-signed by the cluster CA; CLI pins it). Auth: the admin token as `Authorization: Bearer` (CLI, scripts), or a dashboard session cookie from `POST /session {"token"}`, which takes the admin token or the **login code**.
- **Login code:** 8 Crockford-base32 characters, shown on the Controller console as `XXXX-XXXX`. Case, dashes and look-alikes (O/0, I/L/1) don't matter.
- **Rate limits** on `/session` (answered with `429` + `Retry-After`):
  - per address: 5 free attempts, then 1, 2, 4… minutes, up to 1 h;
  - overall: 30 failed code attempts per 10 min. The full token skips this cap, so a flood of guesses can't lock the admin out.
- **Rotation:** `POST /api/v1/settings/login-code` replaces the code; existing sessions stay valid.
- **Unix socket:** as root, also accepted. Endpoints mirror the CLI:

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

## 12. Slave network

With `aiclusterd --slave-net=auto` (the Controller ISO's default), adopted Slaves leave the home network for a subnet the Controller runs on the **same wire**. The Controller is that subnet's only DHCP and DNS server. It is a logical separation, not a security boundary: anything on the switch can still reach the subnet if it configures an address in it.

**Controller**
- **Subnet:** picks the first of `10.10.10.0/24`, `10.10.20.0/24`, `10.20.30.0/24`, `172.31.250.0/24`, `192.168.250.0/24` that overlaps none of its own networks, or uses the CIDR given. The choice is stored, so addresses survive restarts. If the subnet changes, all reservations are dropped.
- **Address:** adds `.1` to the default-route interface (or `--slave-iface`) and re-adds it if networkd removes it. Node-API endpoints list that address **last**, so Slaves still on the home network don't wait on it.
- **DHCP** (UDP 67, bound to the interface): answers **only MACs with a reservation**, and only DISCOVERs or REQUESTs aimed at itself. Your router and every other device are ignored.
  - Offers: the reserved address, the mask, DNS = `.1`, router = `.1`, domain `aicluster`, the node name as hostname, a 12 h lease.
  - A REQUEST for an address that isn't the reserved one gets a NAK.
  - The router option only gives Slaves a default route for broadcasts. The ISO pins `net.ipv4.ip_forward = 0`, so the Controller never routes Slave traffic anywhere.
- **DNS** (`.1:53`, UDP and TCP): `controller.aicluster` and `<node-name>.aicluster` (the node name lower-cased, other characters replaced by `-`). Every other name is REFUSED: Slaves have no internet access, so there is nothing else to resolve.
- **Reservations** (`slave_leases`: node, IP, MAC): allocated from `.10` up. The MAC comes from the node's heartbeat. Releasing the node deletes its reservation.

**Handoff**
1. **Report:** in every heartbeat's observed state, the Slave reports `network {iface, mac, address, on_slave_net, error}`: the interface it reaches the Controller through. A node that can't switch networks (not Linux, or not using systemd-networkd, or `--manage-network=false`) reports nothing and stays where it is.
2. **Reserve:** the Controller reserves an address for that MAC and adds `network {subnet, controller_ip, address, mac, domain, node_api_port}` to the node's desired state.
3. **Switch:** if `mac` matches the interface it uses, the agent writes `/etc/systemd/network/05-aicluster-slave.network` and runs `networkctl reload` and `networkctl reconfigure <iface>`. The file sorts before netplan's `10-netplan-*` and matches on the MAC. It sets DHCPv4 with `AllowList=<controller_ip>` (the router's offers are ignored), no IPv6 RA and no link-local addressing.
4. **Verify:** within 90 s the interface must hold the reserved address and reach `controller_ip:node_api_port`. Otherwise the file is removed and the interface returns to the home network. The error shows in observed state and the dashboard, and the agent retries after 10 minutes.
5. **Done:** the agent puts `https://controller_ip:port` first in its Controller endpoints. From then on the endpoint that answered stays first, even when the Controller's list orders it last.

**Back to the home network:** the agent removes the file and reconfigures the interface when:
- the node is released, or factory reset from its console;
- the desired state no longer has `network`;
- there has been no successful heartbeat for 10 minutes, for example because the Controller is gone or broken. The node is then discoverable on the home network again, and the handoff repeats once the Controller is back.

## 13. In-place updates

The dashboard's **Update Controller and Slaves now** button installs the branch's newest commit on the whole cluster without reinstalling.

**Bundle.** CI (`.github/workflows/update-bundle.yml`) runs `make update-bundle` on every push. It publishes two assets on the rolling pre-release `update-<branch>`:
- `aicluster-update-linux-amd64.tar.gz`, a single `aicluster/` directory with:
  - all three binaries, the provisioner and the catalog;
  - the systemd units, `install-target.sh`, `apply-update` and `manifest.json`.

  The model runtimes (Ollama, llama.cpp) are not in it and stay as installed.
- `manifest.json`: `{"commit","version","built"}`.

**Controller**
1. **Check:** `POST /api/v1/updates/apply` confirms the published manifest is for the commit being installed. If CI hasn't finished, it says so and changes nothing.
2. **Download and verify:**
   - It downloads the bundle and checks its SHA-256 against the digest GitHub records for the asset.
   - It unpacks the bundle into `/var/lib/aicluster/update/staged`. Only regular files and directories are allowed, and no path may point outside that directory.
   - It checks the manifest inside the bundle.
3. **Install:** it writes `/var/lib/aicluster/update/request`, and `aicluster-update.path` starts `apply-update` as root, which:
   - stops `aiclusterd` and backs up the files it will replace, plus the database;
   - runs the bundle's `install-target.sh controller` and restarts the services;
   - requires `aiclusterd` to stay active without restarting for 20 s, within 90 s;
   - otherwise restores the backup and the database and restarts the old version.

   Either way it leaves `result.json` behind.
4. **Resume:** progress is kept in the `update_rollout` setting.
   - If the restarted Controller runs the target commit, it moves on to the nodes.
   - If it is still on the old commit, it reports the updater's error.

**Slaves**, one at a time, sorted by name:
1. **Skip** nodes that are offline, already on the target, or can't update themselves. A node can when it reports `versions.self_update = "yes"`. Agents older than this feature don't report it and need one reinstall from a current Slave ISO.
2. **Signal:** add `update: {commit, path: "/v1/update/bundle", sha256, size}` to the node's desired state.
3. **Download:** the agent downloads the bundle over mTLS, checks the SHA-256, unpacks it, checks the manifest and requests the install. It reports progress as `observed.update {commit, state: downloading|installing|failed, error}`.
4. **Install:** `apply-update` runs `install-target.sh slave`, restarts `aicluster-agent` and checks it the same way, rolling back on failure.
5. **Confirm:** the node is done when its heartbeat reports `versions.agent_commit` equal to the target. The update is then removed from its desired state.
6. **Stop on failure:** a node that reports `failed`, or isn't back within 10 minutes, fails the rollout. The remaining nodes are left untouched, since a bad build would most likely break them all. Pressing Update again retries the nodes that aren't current.

**Trust.** The updater runs whatever the bundle contains as root, so anyone who can push to the configured branch can update every cluster that follows it. The SHA-256 check guards against corrupted or swapped downloads, not against a compromised repository. Point Settings → Updates at a repository and branch you control.

## 14. GPU drivers

Slaves have no internet, so the Controller picks each node's GPU driver and serves Ubuntu's archive to it. NVIDIA cards get NVIDIA's driver. Intel and AMD GPUs, including the graphics built into many laptop and small-desktop processors, get Mesa's Vulkan driver, which Ollama uses.

**Identify.** The agent reports each card's PCI `modalias` (§6.1) in its inventory, and its current GPU list in `observed.gpus`. The Controller replaces the stored inventory's GPUs with that list, so a card shows its model and memory once its driver loads, and the provisioner can place models on it.

**Pick for NVIDIA** (Controller, `controller/drivers`):
1. It reads `restricted/binary-amd64/Packages.gz` for the node's release (`host.codename`, default `noble`) and its `-updates` and `-security` pockets, keeping the newest version of each package. It reads them through its archive copy, and again every 12 hours.
2. Ubuntu's `nvidia-driver-<branch>[-server][-open]` packages list the cards they support as modalias patterns, the same data `ubuntu-drivers` uses. Every NVIDIA card on the node must match.
3. Like `ubuntu-drivers --gpgpu`, it prefers the `-server` (long-term support) line, then the newest branch, then NVIDIA's open kernel modules. The open modules only list the cards that support them (Turing and newer).
4. The packages are the headless driver without DKMS, `nvidia-utils` for `nvidia-smi`, and Canonical's prebuilt, signed kernel modules for the node's kernel line: `linux-modules-nvidia-<suffix>-generic` for Ubuntu's main kernel, `-generic-hwe-24.04` or `-generic-<x.y>` for the others, chosen by which kernel series each one depends on. Signed modules load with Secure Boot on, and nothing is compiled on the node.
5. It puts the result in `desired.drivers` and sets `allow_reboot`. When no driver supports a card, `drivers` is left out and the dashboard shows why.

**Pick for Intel and AMD.** When a node has no NVIDIA card but has an Intel or AMD GPU, the Controller asks for `mesa-vulkan-drivers` and `libvulkan1` from Ubuntu's main component, with `allow_reboot` off: the kernel drivers (`i915`/`xe`, `amdgpu`) are already in Ubuntu's kernel. `use_integrated` carries the Settings switch for integrated GPUs (on by default). A node with both an NVIDIA card and integrated graphics gets only the NVIDIA driver.

**Install** (agent, as root):
1. It writes its own source list pointing at `http://127.0.0.1:7452/apt/ubuntu` (the loopback shim, which forwards to the Controller over mTLS), `signed-by` Ubuntu's archive keyring. Its package lists live in `/var/lib/aicluster/apt`, so the node's normal apt sources are neither used nor changed.
2. It runs `apt-get update` and `apt-get install --no-install-recommends` with the packages. apt checks the archive's signature and every file's hash, so the Controller can't alter what gets installed.
3. If the running kernel has no module for the driver, or `nouveau` holds the card, it reports `rebooting` and reboots (blocking `nouveau` first if needed). The module packages pin the newest kernel of their line, so a node installed from an older ISO reboots into that kernel. It reboots once per driver; if the module still won't load after that, it reports `failed`. Without `allow_reboot` it reports `reboot_required` and waits.
4. NVIDIA: it loads `nvidia` and `nvidia_uvm` and checks `nvidia-smi`. Intel and AMD: it checks that the card runs `i915`, `xe` or `amdgpu` and has a render node, and that Mesa's Vulkan driver for it is installed. It then writes `/etc/systemd/system/ollama.service.d/aicluster-gpu.conf`, which gives Ollama the `render` and `video` groups, `CAP_PERFMON` (so Vulkan can report memory use, per Ollama's docs) and `OLLAMA_IGPU_ENABLE` from `use_integrated`; Ollama skips integrated GPUs without it.
5. It rescans the GPUs, restarts Ollama so it finds them, and reports `ready` with the driver version.
6. Before installing it finishes any interrupted dpkg run, waiting up to 10 minutes for dpkg's lock, which Ubuntu's own daily apt jobs hold for a few seconds after boot. A failed install is tried again after 30 minutes. When the packages are already installed it only loads the driver. Upgrading an installed driver isn't done yet.

**Archive copy** (`GET /apt/ubuntu/…` on the node API):
- It fetches from `--apt-mirror` (default `http://archive.ubuntu.com/ubuntu`) into `<data dir>/apt`. An empty `--apt-mirror` turns GPU drivers off.
- Only paths under `dists/` and `pool/` are served.
- Packages and `by-hash` files never change, so they are kept. Other index files are fetched again after 30 minutes, and the old copy is served if the archive can't be reached.
- Concurrent requests for one file share a single download, and a second node with the same card gets everything from the Controller's copy.
- Offline mode serves only what is already cached.

**Memory.** Integrated GPUs (Intel's at `00:02.0`, AMD's with under 2 GiB of dedicated memory) are marked `integrated`: they use the node's RAM, so the planner keeps budgeting models from RAM and doesn't count that memory twice. Discrete AMD cards report their memory from `mem_info_vram_total`. The planner counts a GPU's memory only once its driver works (`compute.api` set). Intel Arc cards are used by Ollama, but their memory isn't read yet, so the planner budgets from RAM for them.

**Tested:** a Slave VM without internet, reporting an RTX 4090's modalias (a VM can't have the real card), got `nvidia-driver-580-server-open`. It installed it and kernel 6.8.0-147 through the Controller, rebooted once from 6.8.0-139, and then failed at `modprobe nvidia: No such device`, as it should without the card. A second run reported an Intel UHD 630: the node installed Mesa through the Controller in about 12 seconds, without a reboot, and then reported that no `i915`/`xe` driver was loaded, which is right in a VM. With Mesa installed and integrated GPUs allowed, Ollama found no Vulkan devices and stayed on the CPU, so Mesa's software renderer (`llvmpipe`) isn't mistaken for a GPU. Not tested: a real NVIDIA, Intel or AMD GPU.

## 15. Temperatures and fans

**Reading.** Every heartbeat's `health` carries `thermal` and, for NVIDIA cards, `gpus`. The agent samples every 3 seconds.
- **Sensors** come from `/sys/class/hwmon`: one directory per chip (`coretemp`, `k10temp`, `nvme`, `amdgpu`, the motherboard's `nct6775`/`it87`, …), with `tempN_input` in millidegrees and `tempN_crit`. Per-core readings are left out; the package reading covers them. At most 24 sensors are sent.
- **`cpu_temp_c`** is the hottest reading of the CPU chips (`coretemp`, `k10temp`, `zenpower`, `cpu_thermal`).
- **`throttled`** means Intel's `package_throttle_count` went up since the last sample. AMD doesn't report throttling.
- **NVIDIA cards** report utilisation, memory, temperature, power and thermal slowdown through `nvidia-smi` in `health.gpus[]`.
- **Fans** are `fanN_input` (RPM) paired with `pwmN` (0–255, shown as `duty` in percent). `control` is `firmware` when the agent could drive the fan, `agent` while it does, and `fixed` when the OS can't set its speed.
- **`fan_control`** is `yes` when the agent can drive at least one fan, otherwise why not. Office PCs and most laptops leave fans to their firmware.

```json
"thermal": {
  "cpu_temp_c": 71, "throttled": false, "fan_control": "yes",
  "sensors": [ { "chip": "coretemp", "label": "Package id 0", "temp_c": 71, "crit_c": 100 },
               { "chip": "nvme", "label": "Composite", "temp_c": 42 } ],
  "fans": [ { "id": "nct6798/fan2", "label": "", "rpm": 1240, "duty": 91, "control": "agent" } ]
}
```

**Hot nodes.** A node is hot when its CPU or an NVIDIA card is throttling for heat, or its CPU is at 95 °C or more. The gateway adds 0.5 to a hot node's score, so new requests go to cooler nodes with free slots, but a hot node still serves when nothing else can. The dashboard shows the CPU temperature in the node list and a warning in the node's details.

**Fans.** The setting `fans` (`PUT /api/v1/settings {"fans": "firmware"|"boost"}`) is cluster-wide. With `boost`, every node's desired state carries `fans: {mode: "boost"}`. The agent then drives every fan whose `pwmN` and `pwmN_enable` it can write:
1. **Save first.** Before touching a fan it saves `pwmN_enable` and `pwmN` to `/var/lib/aicluster/fans.json`.
2. **Take over.** It sets `pwmN_enable` to 1 (manual).
3. **Follow the temperature.** It sets the duty from the hottest CPU or GPU (a graphics card's own fan follows that card): 35% at 45 °C or below, rising evenly to 100% at 75 °C, and full speed from 85 °C. It never sets less than the `pwmN` the firmware was running when the agent took over.
4. **Hand back.** Without any temperature reading, after a failed write, when the setting goes back to `firmware`, and when the agent stops, it restores the saved mode.

A crash can't leave fans boosted:
- the unit runs `aicluster-agent fans-restore` as `ExecStopPost`, which restores from `fans.json` whenever the agent stops for any reason;
- the agent also restores from that file when it starts.

NVIDIA cards' own fans stay under the card's control.

**Tested:**
- **Automated tests:** sensor readings from a fake `/sys` tree reach the Controller. `boost` takes over a fan at the expected duty, never below the firmware's, and at full speed when hot. Switching back, stopping the agent and restoring from the saved file all return the fan to the firmware.
- **Test VM:** a VM has no sensors. Its node reported `no fans are visible to the operating system`, `boost` changed nothing, and `fans-restore` ran cleanly on every stop.
- **Not tested:** real sensors and fans.

## 16. 1 Gb networks: peer copying and link checks

Nodes are expected on 1 Gb copper, which moves about 110 MB/s. Answers barely touch the network: a reply streams a few KB, and a pooled model passes about 32 KB per word between members. Moving model files is what takes time, 45 s for a 7B model and 6 minutes for a 70B one, and every node used to fetch them through the Controller's single port.

**Peer copying.** A node that has a model file serves it to other nodes, so copies don't all queue on the Controller's port.
- **Serving:** agents serve complete files from the runtime's blob store at `GET /v2/blobs/{digest}` on `:7450`, with `Range`, only to clients with a node certificate of this cluster. They report `versions.peer_blobs = "yes"`.
- **Asking:** before each blob request, the registry shim asks the Controller `GET /v1/nodes/{id}/blob-source/{digest}`.
- **Choosing a source** (Controller): it looks for online nodes that report the model as installed or loaded, whose manifest contains the file, and that aren't avoided for this file. It picks the one with the fewest downloads in progress, if under `--peer-uploads` (default 2); otherwise the Controller. Files under 64 MiB always come from the Controller.
- **Leases:** a one-minute lease, renewed by every request, keeps all of a runtime's ranged requests for one file on one source. Ollama fetches large files in 100 MB parts.
- **Fetching** (shim): it streams from the peer, authenticating it by its node certificate. If the peer can't be reached or answers with an error, it falls back to the Controller for that request. It also tells the Controller (`?failed=`), which avoids that peer for the file for 10 minutes.
- **Integrity:** the runtime checks every file's SHA-256, so a peer can't substitute anything.
- **Older agents** neither ask nor serve, and keep downloading from the Controller.

**Link checks.**
- **Reporting:** every heartbeat's `health.link` is the interface towards the Controller, from `/sys/class/net/<iface>/{speed,duplex,wireless}`: `{iface, mbps, duplex, wireless}`.
- **Warnings:** the dashboard flags links under 1000 Mb/s, at half duplex, or over Wi-Fi, in the node list and the node's details. A copper cable with a damaged pair commonly links at 100 Mb/s without any other symptom.
- **Controller:** Settings → Cluster shows the Controller's own link (the Slave network's interface, else the default route's).
- **Unknown speed:** virtual network cards report none; that shows as "speed unknown", with no warning.

**Speed test.** `POST /api/v1/nodes/{id}/nettest` moves 128 MiB each way over the node API's mTLS connection:
- `POST /v1/nettest` on the agent receives and drops a body (to the node);
- `GET /v1/nettest?bytes=` sends one (from the node).

It returns `{to_node_mbps, from_node_mbps, bytes, at, error}`, which the node details show. Only one test runs at a time. A result well below the link speed points at the cable, the switch, or other traffic.

**Tested:**
- **Automated tests:** a second node copied a model from the first. A third node, sent to two nodes that had just stopped, fell back to the Controller and still got a verified copy. Speed tests over loopback measured 3.5–4 Gb/s, so TLS doesn't limit a 1 Gb link.
- **Test VMs with real Ollama:** the second Slave's Ollama fetched a 531 MB model file in six 100 MB parts, all from the first Slave, in about a second.
- **Not tested:** physical 1 Gb links and switches.
