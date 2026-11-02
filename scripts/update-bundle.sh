#!/bin/bash
# Builds the in-place update bundle (docs/protocol.md §13) from the Linux
# binaries in bin/linux-amd64 (run 'make build' first):
#
#   out/aicluster-update-linux-amd64.tar.gz   everything both roles install,
#                                             except the model runtimes
#   out/manifest.json                         the commit it was built from
#
# CI publishes both on the release "update-<branch>"; Controllers download
# them from there.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
version=${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}
commit=$(git rev-parse HEAD)
if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ] && [ -z "${ALLOW_DIRTY:-}" ]; then
	echo "update-bundle: the checkout has uncommitted changes; the bundle would not match commit $commit (set ALLOW_DIRTY=1 to build anyway)" >&2
	exit 1
fi
built=$(date -u +%Y-%m-%dT%H:%M:%SZ)

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
a=$stage/aicluster
mkdir -p "$a/bin" "$a/systemd" "$a/provisioner" out
for b in aiclusterd aicluster aicluster-agent; do
	[ -f "bin/linux-amd64/$b" ] || { echo "update-bundle: bin/linux-amd64/$b missing; run 'make build' first" >&2; exit 1; }
	install -m 0755 "bin/linux-amd64/$b" "$a/bin/$b"
done
install -m 0755 iso/payload/install-target.sh iso/payload/console-status iso/payload/apply-update "$a/"
cp iso/payload/systemd/* "$a/systemd/"
cp -r controller/provisioner/aicluster_provisioner "$a/provisioner/"
find "$a/provisioner" -name __pycache__ -prune -exec rm -rf {} +
cp -r catalog "$a/catalog"
cat >"$a/BUILD" <<EOF
aicluster update $version
commit $commit
built $built
EOF
printf '{"commit":"%s","version":"%s","built":"%s"}\n' "$commit" "$version" "$built" | tee "$a/manifest.json" >out/manifest.json

tar -C "$stage" --owner=0 --group=0 --numeric-owner -czf out/aicluster-update-linux-amd64.tar.gz aicluster
echo "out/aicluster-update-linux-amd64.tar.gz ($commit)"
