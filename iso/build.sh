#!/usr/bin/env bash
# Builds an AI Cluster installer ISO by remastering the official Ubuntu
# 24.04 live-server ISO: adds an autoinstall config, a boot menu that starts
# it, and the AI Cluster payload (binaries, units, and for Slaves the Ollama
# and llama.cpp runtimes). The original boot setup (BIOS + UEFI, hybrid USB)
# is replayed unchanged, so the result boots the same ways Ubuntu's does.
#
#   iso/build.sh controller|slave
#
# Needs bash, curl, jq, unzip, zstd, sha256sum and xorriso. Without xorriso
# but with Docker, it re-runs itself in the build container (iso/Dockerfile).
# Go binaries must already be built into bin/linux-amd64 (make build).
#
# Environment (all optional):
#   VERSION            version recorded in the image (default: git describe)
#   OUT_DIR            where the ISO goes (default: out)
#   CACHE_DIR          downloads are kept here between builds (default: iso/cache)
#   WORK_DIR           scratch space for staging (default: build/iso)
#   UBUNTU_ISO         use this local Ubuntu 24.04 live-server ISO instead of downloading
#   UBUNTU_MIRROR      default https://releases.ubuntu.com/24.04
#   OLLAMA_VERSION     Ollama release tag, e.g. v0.40.13 (default: latest)
#   LLAMACPP_VERSION   llama.cpp release tag, e.g. b12032 (default: newest CPU build)
#   SKIP_OLLAMA=1      Slave image without Ollama
#   SKIP_LLAMACPP=1    Slave image without llama.cpp (no pooled models)
#   SSH_KEY_FILE       public key file: enables SSH for user aicluster-admin
#   CONSOLE_PASSWORD   enables console login for aicluster-admin
#   GRUB_TIMEOUT       seconds before the installer starts (default: 10)
#   KERNEL_ARGS        extra installer kernel arguments, e.g. console=ttyS0
#   GITHUB_TOKEN       avoids GitHub API rate limits when resolving releases
set -euo pipefail

role=${1:-${ISO_ROLE:-}}
case "$role" in
controller) title=Controller ;;
slave) title=Node ;;
*) echo "usage: $0 controller|slave" >&2; exit 2 ;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

need=(curl git jq unzip zstd sha256sum xorriso)
[ -n "${CONSOLE_PASSWORD:-}" ] && need+=(openssl)
missing=()
for t in "${need[@]}"; do command -v "$t" >/dev/null || missing+=("$t"); done
if [ ${#missing[@]} -gt 0 ]; then
	if [ -z "${AICLUSTER_IN_BUILDER:-}" ] && command -v docker >/dev/null; then
		echo "==> ${missing[*]} not found; building in Docker"
		docker build -q -t aicluster-isobuild iso >/dev/null
		envs=()
		for v in VERSION OUT_DIR CACHE_DIR WORK_DIR UBUNTU_ISO UBUNTU_MIRROR OLLAMA_VERSION LLAMACPP_VERSION \
			SKIP_OLLAMA SKIP_LLAMACPP SSH_KEY_FILE CONSOLE_PASSWORD GRUB_TIMEOUT KERNEL_ARGS GITHUB_TOKEN; do
			[ -n "${!v:-}" ] && envs+=(-e "$v")
		done
		[ -n "${VERSION:-}" ] || envs+=(-e "VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)")
		exec docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e AICLUSTER_IN_BUILDER=1 "${envs[@]}" -v "$root:/src" -w /src \
			aicluster-isobuild iso/build.sh "$role"
	fi
	echo "missing tools: ${missing[*]} (install them, or install Docker to use iso/Dockerfile)" >&2
	exit 1
fi

version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
out_dir=${OUT_DIR:-out}
cache=${CACHE_DIR:-iso/cache}
mirror=${UBUNTU_MIRROR:-https://releases.ubuntu.com/24.04}
work=${WORK_DIR:-build/iso}/$role
stage=$work/stage
mkdir -p "$cache" "$out_dir"
rm -rf "$work"
mkdir -p "$stage/aicluster/bin" "$stage/aicluster/systemd" "$stage/aicluster/runtime"

log() { echo "==> $*"; }
die() { echo "error: $*" >&2; exit 1; }

# fetch URL DEST [SHA256]: download once into the cache, verify if a hash is known.
fetch() {
	local url=$1 dest=$2 sum=${3:-}
	if [ -f "$dest" ] && { [ -z "$sum" ] || echo "$sum  $dest" | sha256sum -c --status; }; then
		return
	fi
	log "downloading $url"
	curl -fL --retry 3 -o "$dest.part" "$url"
	if [ -n "$sum" ]; then
		echo "$sum  $dest.part" | sha256sum -c --status || die "checksum mismatch for $url"
	fi
	mv "$dest.part" "$dest"
}

gh_api() {
	local auth=()
	[ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
	curl -fsSL "${auth[@]}" -H "Accept: application/vnd.github+json" "https://api.github.com/$1"
}

# gh_asset REPO TAG REGEX: prints "<tag> <name> <url> <sha256-or-empty>".
# Without a tag it takes the latest release, or, when that has no matching
# file (llama.cpp publishes its builds as pre-releases), the newest one that does.
gh_asset() {
	local pick='. as $r | [.assets[] | select(.name | test($re))][0]
		| select(. != null) | "\($r.tag_name) \(.name) \(.browser_download_url) \((.digest // "") | sub("^sha256:"; ""))"'
	local found
	if [ -n "$2" ]; then
		gh_api "repos/$1/releases/tags/$2" | jq -r --arg re "$3" "$pick"
		return
	fi
	found=$(gh_api "repos/$1/releases/latest" | jq -r --arg re "$3" "$pick")
	if [ -z "$found" ]; then
		found=$(gh_api "repos/$1/releases?per_page=30" | jq -r --arg re "$3" "[.[] | select(.draft | not) | $pick][0] // empty")
	fi
	echo "$found"
}

# ---- Ubuntu base image ------------------------------------------------------
if [ -n "${UBUNTU_ISO:-}" ]; then
	base=$UBUNTU_ISO
	[ -f "$base" ] || die "UBUNTU_ISO=$base not found"
else
	sums=$(curl -fsSL "$mirror/SHA256SUMS") || die "cannot read $mirror/SHA256SUMS"
	line=$(grep -E 'ubuntu-24\.04(\.[0-9]+)?-live-server-amd64\.iso$' <<<"$sums" | tail -n1)
	[ -n "$line" ] || die "no live-server ISO listed in $mirror/SHA256SUMS"
	name=${line##*[ *]}
	base=$cache/$name
	fetch "$mirror/$name" "$base" "${line%% *}"
fi
base_name=$(basename "$base")
log "base image $base_name"

xorriso -osirrox on -indev "$base" -extract /boot/grub/grub.cfg "$work/grub.orig.cfg" >/dev/null 2>&1 ||
	die "cannot read /boot/grub/grub.cfg from $base_name"
grep -q '/casper/vmlinuz' "$work/grub.orig.cfg" && grep -q '/casper/initrd' "$work/grub.orig.cfg" ||
	die "$base_name does not boot /casper/vmlinuz + /casper/initrd; is it an Ubuntu 24.04 live-server ISO?"

# ---- payload ----------------------------------------------------------------
bin=bin/linux-amd64
case "$role" in
controller) bins=(aiclusterd aicluster) ;;
slave) bins=(aicluster-agent) ;;
esac
for b in "${bins[@]}"; do
	[ -f "$bin/$b" ] || die "$bin/$b missing; run 'make build' first"
	install -m 0755 "$bin/$b" "$stage/aicluster/bin/$b"
done
install -m 0755 iso/payload/install-target.sh iso/payload/console-status iso/payload/wipe-disks.sh iso/payload/apply-update "$stage/aicluster/"
cp iso/payload/systemd/* "$stage/aicluster/systemd/"

components="ubuntu=$base_name"
if [ "$role" = controller ]; then
	mkdir -p "$stage/aicluster/provisioner"
	cp -r controller/provisioner/aicluster_provisioner "$stage/aicluster/provisioner/"
	find "$stage/aicluster/provisioner" -name __pycache__ -prune -exec rm -rf {} +
	cp -r catalog "$stage/aicluster/catalog"
else
	if [ -z "${SKIP_OLLAMA:-}" ]; then
		read -r tag name url sum < <(gh_asset ollama/ollama "${OLLAMA_VERSION:-}" '^ollama-linux-amd64\.(tgz|tar\.zst)$') ||
			die "no ollama-linux-amd64 asset in release ${OLLAMA_VERSION:-latest}"
		[ -n "$sum" ] || log "warning: GitHub gives no checksum for $name; not verified"
		mkdir -p "$cache/ollama-$tag"
		fetch "$url" "$cache/ollama-$tag/$name" "$sum"
		# The installed system may lack zstd, so ship it gzip-compressed.
		tgz=$cache/ollama-$tag/ollama-linux-amd64.tgz
		if [ ! -f "$tgz" ]; then
			log "recompressing $name as gzip"
			zstd -dc "$cache/ollama-$tag/$name" | gzip -1 >"$tgz.part" && mv "$tgz.part" "$tgz"
		fi
		cp "$tgz" "$stage/aicluster/runtime/ollama-linux-amd64.tgz"
		components+=" ollama=$tag"
	fi
	if [ -z "${SKIP_LLAMACPP:-}" ]; then
		read -r tag name url sum < <(gh_asset ggml-org/llama.cpp "${LLAMACPP_VERSION:-}" '^llama-.*-bin-ubuntu-x64\.(zip|tar\.gz)$') ||
			die "no ubuntu-x64 build in llama.cpp release ${LLAMACPP_VERSION:-latest}"
		[ -n "$sum" ] || log "warning: GitHub gives no checksum for $name; not verified"
		mkdir -p "$cache/llama.cpp-$tag"
		fetch "$url" "$cache/llama.cpp-$tag/$name" "$sum"
		x=$work/llama.cpp-extract
		mkdir -p "$x"
		case "$name" in
		*.zip) unzip -q "$cache/llama.cpp-$tag/$name" -d "$x" ;;
		*) tar -C "$x" -xzf "$cache/llama.cpp-$tag/$name" ;;
		esac
		# The worker is "ggml-rpc-server" in current releases, "rpc-server" in older ones.
		rpc=$(find "$x" -type f \( -name ggml-rpc-server -o -name rpc-server \) | head -n1)
		[ -n "$rpc" ] || die "llama.cpp $tag has no RPC server in its ubuntu-x64 build (set LLAMACPP_VERSION, or SKIP_LLAMACPP=1)"
		dir=$(dirname "$rpc")
		[ -f "$dir/llama-server" ] || die "llama.cpp $tag: llama-server is not next to $(basename "$rpc")"
		lc=$work/llama.cpp-stage
		rm -rf "$lc"
		mkdir -p "$lc"
		cp "$rpc" "$dir/llama-server" "$lc/"
		find "$dir" -maxdepth 1 -name '*.so*' -exec cp -P {} "$lc/" \;
		# llama.cpp's CPU backends need OpenMP, which a minimal Ubuntu lacks.
		# The builder is Ubuntu 24.04 like the target, so its copy fits.
		gomp=$(ldconfig -p | awk '/libgomp\.so\.1 .*x86-64/ {print $NF; exit}')
		[ -n "$gomp" ] || die "libgomp.so.1 not found on the build machine (apt install libgomp1)"
		cp -L "$gomp" "$lc/libgomp.so.1"
		# Shipped as one archive: its libraries are found through symlinks,
		# which USB tools that copy the ISO onto FAT32 turn into empty files.
		tar -C "$lc" -czf "$stage/aicluster/runtime/llama.cpp.tgz" .
		components+=" llama.cpp=$tag"
	fi
fi

commit=$(git rev-parse HEAD 2>/dev/null || echo unknown)
cat >"$stage/aicluster/BUILD" <<EOF
aicluster $role $version
commit $commit
built $(date -u +%Y-%m-%dT%H:%M:%SZ)
components $components
EOF

# ---- autoinstall + boot menu --------------------------------------------------
ssh_server=false
keys=""
if [ -n "${SSH_KEY_FILE:-}" ]; then
	[ -f "$SSH_KEY_FILE" ] || die "SSH_KEY_FILE=$SSH_KEY_FILE not found"
	ssh_server=true
	keys="        ssh_authorized_keys:"
	while IFS= read -r k; do
		[ -n "$k" ] && keys+=$'\n'"          - $(jq -Rn --arg k "$k" '$k')"
	done <"$SSH_KEY_FILE"
fi
lock=true
passwd=""
if [ -n "${CONSOLE_PASSWORD:-}" ]; then
	lock=false
	passwd="        passwd: $(jq -Rn --arg h "$(openssl passwd -6 -stdin <<<"$CONSOLE_PASSWORD")" '$h')"
fi

awk -v role="$role" -v ssh="$ssh_server" -v lock="$lock" -v passwd="$passwd" -v keys="$keys" '
	/^@PASSWD_LINE@$/ { if (passwd != "") print passwd; next }
	/^@SSH_KEYS@$/ { if (keys != "") print keys; next }
	{ gsub(/@ROLE@/, role); gsub(/@SSH_SERVER@/, ssh); gsub(/@LOCK_PASSWD@/, lock); print }
' iso/autoinstall.yaml.in >"$work/autoinstall.yaml"

sed -e "s|@TIMEOUT@|${GRUB_TIMEOUT:-10}|" -e "s|@TITLE@|$title|" -e "s|@KERNEL_ARGS@|${KERNEL_ARGS:-}|" \
	iso/grub.cfg.in >"$work/grub.cfg"

# The installer's media check (casper-md5check) compares files with
# /md5sum.txt; record the new grub.cfg so the check passes.
xorriso -osirrox on -indev "$base" -extract /md5sum.txt "$work/md5sum.txt" >/dev/null 2>&1 ||
	die "cannot read /md5sum.txt from $base_name"
chmod u+w "$work/md5sum.txt"
sum=$(md5sum "$work/grub.cfg" | cut -d' ' -f1)
sed -i "s|^[0-9a-f]\{32\}  \./boot/grub/grub\.cfg\$|$sum  ./boot/grub/grub.cfg|" "$work/md5sum.txt"
grep -q "^$sum  \./boot/grub/grub\.cfg\$" "$work/md5sum.txt" || die "md5sum.txt has no ./boot/grub/grub.cfg entry"

# ---- remaster -----------------------------------------------------------------
out=$out_dir/aicluster-$role-$version.iso
rm -f "$out"
log "writing $out"
xorriso -report_about WARNING \
	-indev "$base" -outdev "$out" \
	-map "$stage/aicluster" /aicluster \
	-map "$work/autoinstall.yaml" /autoinstall.yaml \
	-map "$work/grub.cfg" /boot/grub/grub.cfg \
	-map "$work/md5sum.txt" /md5sum.txt \
	-boot_image any replay \
	-padding included
(cd "$out_dir" && sha256sum "$(basename "$out")" >"$(basename "$out").sha256")
log "done: $out ($(du -h "$out" | cut -f1)) $components"
