#!/bin/bash
# Runs inside the freshly installed system (curtin in-target) at the end of
# the unattended install. Installs the AI Cluster payload for one role.
# Idempotent: running it again on an installed system just refreshes files.
set -euo pipefail

role=${1:?usage: install-target.sh controller|slave}
src=$(cd "$(dirname "$0")" && pwd)
log() { echo "aicluster-install: $*"; }

case "$role" in controller|slave) ;; *) echo "unknown role $role" >&2; exit 2 ;; esac

install -d -m 0755 /etc/aicluster /usr/lib/aicluster /etc/issue.d
echo "$role" >/etc/aicluster/role
cp "$src/BUILD" /etc/aicluster/build-info

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
units=(aicluster-console.timer)

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
	sed 's/0750 root root/0750 aicluster aicluster/' "$src/systemd/aicluster.tmpfiles" >/etc/tmpfiles.d/aicluster.conf
	install -m 0644 "$src/systemd/aiclusterd.service" "$src/systemd/aicluster-provisioner.service" /etc/systemd/system/
	[ -f /etc/aicluster/controller.env ] || echo 'AICLUSTERD_ARGS=' >/etc/aicluster/controller.env
	units+=(aicluster-provisioner.service aiclusterd.service)
	;;
slave)
	install -m 0755 "$src/bin/aicluster-agent" /usr/bin/
	id -u ollama >/dev/null 2>&1 || useradd --system --home-dir /var/lib/aicluster/models/ollama --no-create-home --shell /usr/sbin/nologin ollama
	install -d -m 0755 /var/lib/aicluster /var/lib/aicluster/models
	install -d -o ollama -g ollama -m 0755 /var/lib/aicluster/models/ollama
	install -m 0644 "$src/systemd/aicluster.tmpfiles" /etc/tmpfiles.d/aicluster.conf

	if [ -f "$src/runtime/ollama-linux-amd64.tgz" ]; then
		log "installing Ollama"
		rm -rf /usr/lib/ollama
		tar -C /usr -xzf "$src/runtime/ollama-linux-amd64.tgz"
		install -m 0644 "$src/systemd/ollama.service" /etc/systemd/system/
		units+=(ollama.service)
	else
		log "no Ollama in this image; the node will report its runtime as unavailable"
	fi

	if [ -d "$src/runtime/llama.cpp" ]; then
		log "installing llama.cpp (pooled models)"
		rm -rf /usr/lib/aicluster/llama.cpp
		cp -r "$src/runtime/llama.cpp" /usr/lib/aicluster/llama.cpp
		chmod 0755 /usr/lib/aicluster/llama.cpp/*-server
		missing=$(LD_LIBRARY_PATH=/usr/lib/aicluster/llama.cpp ldd /usr/lib/aicluster/llama.cpp/llama-server | awk '/not found/ {print $1}')
		if [ -n "$missing" ]; then
			log "llama.cpp needs libraries not in the image: $missing (pooled models will fail until they are installed)"
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
