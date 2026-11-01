# Ports and firewall

Both OSes use nftables with **default drop** for input. Output is open on the Controller and restricted on Slaves.

## Controller

| Port | Proto | Service | Allowed from | Auth |
|---|---|---|---|---|
| 22 | TCP | SSH | LAN (can be disabled) | keys only |
| 123 | UDP | NTP (chrony) for Slaves | LAN | — |
| 5353 | UDP | mDNS (Avahi) | LAN, multicast | — |
| 47800 | UDP | discovery beacon listen/send | LAN, broadcast | none (hint only) |
| 7443 | TCP | node API: heartbeat, renew, model blobs, OCI pull-through, apt proxy, aicluster repo | LAN | mTLS (cluster CA) |
| 8080 | TCP | OpenAI-compatible gateway (8443 if TLS enabled) | LAN | API key |
| 8090 | TCP | management API + dashboard | LAN (configurable CIDR) | admin token / CLI |
| 9464 | TCP | Prometheus metrics + `/sd/nodes` | LAN (configurable CIDR) | none / optional bearer |

Loopback only: provisioner socket `/run/aicluster/provisioner.sock`, core socket `/run/aicluster/core.sock`, apt-cacher-ng `127.0.0.1:3142`.

Outbound: HTTPS to the configured upstreams (registry.ollama.ai, huggingface.co + CDN, archive.ubuntu.com, developer.download.nvidia.com, repo.radeon.com), NTP, DNS.

Update check: one anonymous `GET` to `api.github.com` every 6 hours (branch head, plus a compare when it moved; ETag-cached). Nothing about the cluster is sent. Turn it off under Settings → Updates; offline mode also stops it.

## Slave

| Port | Proto | Service | Allowed from | Auth |
|---|---|---|---|---|
| 5353 | UDP | mDNS (Avahi) | LAN, multicast | — |
| 47800 | UDP | beacon (send only; receives Controller beacon) | LAN, broadcast | none |
| 7450 | TCP | agent: enrollment (pre-adoption, TLS) → command API + inference proxy (mTLS) | LAN before adoption; **Controller IPs only** after | mTLS |
| 9464 | TCP | Prometheus metrics | Controller IPs only | none |

Loopback only: registry shim `127.0.0.1:7452`, Ollama `127.0.0.1:11434`, llama.cpp server `127.0.0.1:18000+`, vLLM `127.0.0.1:18100+`, whisper `127.0.0.1:18200`.

Outbound: Controller `:7443` only, plus mDNS/beacon broadcast and DHCP. NTP comes from the Controller (`:123/udp`), because certificate validation needs correct clocks.

After adoption the agent rewrites the `controller_ips` nftables set whenever the heartbeat response returns new `controller_endpoints`. SSH is **not installed** on Slaves by default; `aicluster nodes debug-shell <id>` (post-v1) would open a temporary, Controller-proxied shell.
