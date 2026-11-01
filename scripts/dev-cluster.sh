#!/usr/bin/env bash
# Runs a Controller, the provisioner, and two Slave agents on this machine
# over loopback, for trying the dashboard and CLI without any VMs. Discovery
# uses unicast beacons to 127.0.0.1, so nothing is broadcast on the LAN.
# State lives in .dev/.
#
#   scripts/dev-cluster.sh                 # approve-first, fake models
#   MODE=auto scripts/dev-cluster.sh       # auto-adopt
#   UPSTREAM=real scripts/dev-cluster.sh   # download real models from registry.ollama.ai
#
# Models: by default a local fake registry serves small stand-in models and
# the agents use the "fake" runtime (real pulls through the Controller, no
# inference), so installing from the dashboard costs no internet traffic.
#
# Dashboard: https://localhost:8090  (token: .dev/controller/admin.token)
# Pairing code for slave-2: .dev/slave-2/pairing-code
# Python: uses .dev/venv if present (python -m venv .dev/venv && .dev/venv/bin/pip install -e controller/provisioner)
set -euo pipefail
cd "$(dirname "$0")/.."

MODE="${MODE:-approve}"
UPSTREAM="${UPSTREAM:-fake}"
DEV=.dev
BIN="$DEV/bin"
EXE=""
case "$(go env GOOS 2>/dev/null || echo linux)" in windows) EXE=".exe" ;; esac
mkdir -p "$BIN"

if [ -z "${SKIP_BUILD:-}" ]; then
  go build -o "$BIN/aiclusterd$EXE" ./controller/cmd/aiclusterd
  go build -o "$BIN/aicluster-agent$EXE" ./slave-agent/cmd/aicluster-agent
  go build -o "$BIN/fakeregistry$EXE" ./test/fakeregistry/cmd/fakeregistry
fi

PY=python3
for p in "$DEV/venv/Scripts/python.exe" "$DEV/venv/bin/python"; do [ -x "$p" ] && PY="$p" && break; done
command -v "$PY" >/dev/null 2>&1 || PY=python

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM

PYTHONPATH=controller/provisioner "$PY" -m aicluster_provisioner \
  --listen tcp:127.0.0.1:7460 --catalog catalog --log-level warning &
pids+=($!)

REGISTRY=https://registry.ollama.ai
FAKE_MBPS=0
if [ "$UPSTREAM" = fake ]; then
  "$BIN/fakeregistry$EXE" --listen 127.0.0.1:7470 --layer-mb 40 --mbps 8 &
  pids+=($!)
  REGISTRY=http://127.0.0.1:7470
  FAKE_MBPS=10
fi

"$BIN/aiclusterd$EXE" \
  --data-dir "$DEV/controller" \
  --nodeapi-listen 127.0.0.1:7443 --advertise https://127.0.0.1:7443 \
  --mgmt-listen 127.0.0.1:8090 --gateway-listen 127.0.0.1:8080 \
  --beacon-listen 127.0.0.1:47810 --beacon-targets 127.0.0.1:47811 \
  --provisioner tcp:127.0.0.1:7460 --registry-upstream "$REGISTRY" \
  --mdns=false --adoption-mode "$MODE" &
pids+=($!)

agent() { # name port shim-port extra-flags...
  local name=$1 port=$2 shim=$3; shift 3
  "$BIN/aicluster-agent$EXE" \
    --data-dir "$DEV/$name" --model-dir "$DEV/$name/models" \
    --listen "127.0.0.1:$port" --registry-shim-listen "127.0.0.1:$shim" \
    --beacon-targets 127.0.0.1:47810 --controller-beacon-listen "" \
    --pairing-code-file "$DEV/$name/pairing-code" \
    --hw-fingerprint "dev-$name" \
    --runtime fake --fake-mbps "$FAKE_MBPS" --fake-label "$name" \
    --mdns=false "$@" &
  pids+=($!)
}
agent slave-1 7451 7453
agent slave-2 7452 7454 --pairing

echo "dev cluster running (mode=$MODE, models=$UPSTREAM)"
echo "  dashboard  https://localhost:8090"
echo "  API        http://localhost:8080/v1  (create a key under Connect)"
wait
