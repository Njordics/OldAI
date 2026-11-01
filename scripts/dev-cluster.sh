#!/usr/bin/env bash
# Runs a Controller and two Slave agents on this machine over loopback, for
# trying the dashboard and CLI without any VMs. Discovery uses unicast beacons
# to 127.0.0.1, so nothing is broadcast on the LAN. State lives in .dev/.
#
#   scripts/dev-cluster.sh            # approve-first mode, one node needs pairing
#   MODE=auto scripts/dev-cluster.sh  # auto-adopt
#
# Dashboard: https://localhost:8090  (token: .dev/controller/admin.token)
# Pairing code for slave-2: .dev/slave-2/pairing-code
set -euo pipefail
cd "$(dirname "$0")/.."

MODE="${MODE:-approve}"
DEV=.dev
BIN="$DEV/bin"
EXE=""
case "$(go env GOOS)" in windows) EXE=".exe" ;; esac
mkdir -p "$BIN"

if [ -z "${SKIP_BUILD:-}" ]; then
  go build -o "$BIN/aiclusterd$EXE" ./controller/cmd/aiclusterd
  go build -o "$BIN/aicluster-agent$EXE" ./slave-agent/cmd/aicluster-agent
fi

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM

"$BIN/aiclusterd$EXE" \
  --data-dir "$DEV/controller" \
  --nodeapi-listen 127.0.0.1:7443 --advertise https://127.0.0.1:7443 \
  --mgmt-listen 127.0.0.1:8090 \
  --beacon-listen 127.0.0.1:47810 --beacon-targets 127.0.0.1:47811 \
  --mdns=false --adoption-mode "$MODE" &
pids+=($!)

agent() { # name port extra-flags...
  local name=$1 port=$2; shift 2
  "$BIN/aicluster-agent$EXE" \
    --data-dir "$DEV/$name" --model-dir "$DEV/$name/models" \
    --listen "127.0.0.1:$port" \
    --beacon-targets 127.0.0.1:47810 --controller-beacon-listen "" \
    --pairing-code-file "$DEV/$name/pairing-code" \
    --hw-fingerprint "dev-$name" \
    --mdns=false "$@" &
  pids+=($!)
}
agent slave-1 7451
agent slave-2 7452 --pairing

echo "dev cluster running (mode=$MODE); dashboard https://localhost:8090; Ctrl-C to stop"
wait
