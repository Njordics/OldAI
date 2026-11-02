#!/bin/bash
# Runs inside the freshly installed system (curtin in-target) at the end of
# the unattended install. Installs the AI Cluster payload for one role.
# Idempotent: running it again on an installed system just refreshes files.
set -euo pipefail
# Name the failing command in the installer log instead of exiting silently.
trap 'echo "aicluster-install: FAILED at line $LINENO (exit $?): $BASH_COMMAND" >&2' ERR

role=${1:?usage: install-target.sh controller|slave}
src=$(cd "$(dirname "$0")" && pwd)
log() { echo "aicluster-install: $*"; }

case "$role" in controller|slave) ;; *) echo "unknown role $role" >&2; exit 2 ;; esac

install -d -m 0755 /etc/aicluster /usr/lib/aicluster /etc/issue.d
echo "$role" >/etc/aicluster/role
# Update bundles leave the runtimes alone, so keep the old components line.
components=$(grep '^components ' /etc/aicluster/build-info 2>/dev/null || true)
cp "$src/BUILD" /etc/aicluster/build-info
if [ -n "$components" ] && ! grep -q '^components ' /etc/aicluster/build-info; then
	echo "$components" >>/etc/aicluster/build-info
fi

# Hostname: one Controller per cluster, Slaves get a stable unique suffix.
if [ "$role" = controller ]; then
	host=aicluster-controller
else
	host="aicluster-node-$(head -c 6 /etc/machine-id)"
fi
echo "$host" >/etc/hostname
if grep -q '^127\.0\.1\.1' /etc/hosts; then
	sed -i "s/^127\.0\.1\.1.*/127.0.1.1 $host/" /etc/hosts
else
	echo "127.0.1.1 $host" >>/etc/hosts
fi

install -m 0755 "$src/console-status" /usr/lib/aicluster/console-status
install -m 0644 "$src/systemd/aicluster-console.service" "$src/systemd/aicluster-console.timer" /etc/systemd/system/
# In-place updates: the daemons stage a bundle in /var/lib/aicluster/update
# and this root-owned updater installs it (docs/protocol.md §13).
install -m 0755 "$src/apply-update" /usr/lib/aicluster/apply-update
install -m 0644 "$src/systemd/aicluster-update.path" "$src/systemd/aicluster-update.service" /etc/systemd/system/
units=(aicluster-console.timer aicluster-update.path)

case "$role" in
controller)
	python3 -c 'import yaml' || { echo "python3-yaml is missing from the target" >&2; exit 1; }
	id -u aicluster >/dev/null 2>&1 || useradd --system --home-dir /var/lib/aicluster --no-create-home --shell /usr/sbin/nologin aicluster
	install -m 0755 "$src/bin/aiclusterd" "$src/bin/aicluster" /usr/bin/
	rm -rf /usr/lib/aicluster/provisioner /usr/share/aicluster/catalog
	install -d /usr/lib/aicluster/provisioner /usr/share/aicluster /etc/aicluster/catalog.d
	cp -r "$src/provisioner/aicluster_provisioner" /usr/lib/aicluster/provisioner/
	cp -r "$src/catalog" /usr/share/aicluster/catalog
	install -d -o aicluster -g aicluster -m 0750 /var/lib/aicluster /var/lib/aicluster/controller
	install -d -o aicluster -g aicluster -m 0755 /var/lib/aicluster/update
	sed 's/0750 root root/0750 aicluster aicluster/' "$src/systemd/aicluster.tmpfiles" >/etc/tmpfiles.d/aicluster.conf
	install -m 0644 "$src/systemd/aiclusterd.service" "$src/systemd/aicluster-provisioner.service" /etc/systemd/system/
	# Slaves move to the Controller's own subnet after adoption (docs/iso.md).
	[ -f /etc/aicluster/controller.env ] || echo 'AICLUSTERD_ARGS=--slave-net=auto' >/etc/aicluster/controller.env
	# The Controller is the Slaves' gateway only so broadcasts work; it must
	# never route them anywhere.
	echo 'net.ipv4.ip_forward = 0' >/etc/sysctl.d/60-aicluster.conf
	units+=(aicluster-provisioner.service aiclusterd.service)
	;;
slave)
	install -m 0755 "$src/bin/aicluster-agent" /usr/bin/
	id -u ollama >/dev/null 2>&1 || useradd --system --home-dir /var/lib/aicluster/models/ollama --no-create-home --shell /usr/sbin/nologin ollama
	install -d -m 0755 /var/lib/aicluster /var/lib/aicluster/models /var/lib/aicluster/update
	install -d -o ollama -g ollama -m 0755 /var/lib/aicluster/models/ollama
	install -m 0644 "$src/systemd/aicluster.tmpfiles" /etc/tmpfiles.d/aicluster.conf

	if [ -f "$src/runtime/ollama-linux-amd64.tgz" ]; then
		log "installing Ollama"
		rm -rf /usr/lib/ollama
		tar -C /usr -xzf "$src/runtime/ollama-linux-amd64.tgz"
		install -m 0644 "$src/systemd/ollama.service" /etc/systemd/system/
		units+=(ollama.service)
	elif [ -x /usr/bin/ollama ]; then
		log "keeping the installed Ollama"
	else
		log "no Ollama in this image; the node will report its runtime as unavailable"
	fi

	# Optional: without llama.cpp the node works, only pooled models don't.
	lc=/usr/lib/aicluster/llama.cpp
	if [ -f "$src/runtime/llama.cpp.tgz" ] || [ -d "$src/runtime/llama.cpp" ]; then
		log "installing llama.cpp (pooled models)"
		if rm -rf "$lc" && mkdir -p "$lc" &&
			if [ -f "$src/runtime/llama.cpp.tgz" ]; then
				tar -C "$lc" -xzf "$src/runtime/llama.cpp.tgz"
			else
				cp -r "$src/runtime/llama.cpp/." "$lc/"
			fi; then
			chmod 0755 "$lc"
		else
			log "WARNING: llama.cpp could not be installed; pooled models won't work on this node"
		fi
	fi
	if [ -d "$lc" ]; then
		# Images that kept llama.cpp as loose files lost its library symlinks
		# when written to a FAT32 USB stick: they arrived as empty files.
		# Point each one back at the versioned library it stood for.
		for f in "$lc"/lib*.so*; do
			[ -f "$f" ] && [ ! -L "$f" ] && [ ! -s "$f" ] || continue
			to=$(cd "$lc" && ls -d "$(basename "$f")".* 2>/dev/null | awk '{print length, $0}' | sort -n | head -n1 | cut -d' ' -f2-) || true
			if [ -n "$to" ]; then
				ln -sfn "$to" "$f"
				log "repaired llama.cpp library link $(basename "$f") -> $to"
			fi
		done
		bad=
		for b in "$lc"/*-server; do
			[ -f "$b" ] || continue
			chmod 0755 "$b"
			LD_LIBRARY_PATH=$lc "$b" --help >/dev/null 2>&1 || bad+=" $(basename "$b")"
		done
		if [ -n "$bad" ]; then
			log "WARNING: llama.cpp doesn't start ($bad ); pooled models won't work on this node"
		fi
	fi

	install -m 0644 "$src/systemd/aicluster-agent.service" /etc/systemd/system/
	[ -f /etc/aicluster/agent.env ] || echo 'AGENT_ARGS=' >/etc/aicluster/agent.env
	units+=(aicluster-agent.service)
	;;
esac

systemctl daemon-reload 2>/dev/null || true
systemctl enable "${units[@]}"
log "$role installed ($(head -n1 /etc/aicluster/build-info))"
