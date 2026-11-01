# Installer ISOs

Two images, both unattended Ubuntu Server 24.04 installs:

| Image | Installs | Hostname |
|---|---|---|
| `aicluster-controller-<version>.iso` | `aiclusterd`, the `aicluster` CLI, the provisioner and the model catalog | `aicluster-controller` |
| `aicluster-slave-<version>.iso` | `aicluster-agent`, Ollama, and llama.cpp's `ggml-rpc-server`/`llama-server` plus the OpenMP library they need (pooled models) | `aicluster-node-<6 chars of machine-id>` |

Booting either one shows a menu that starts the install after 10 seconds. **The install erases the largest disk** without asking: that is the point of an unattended image, so don't leave the stick plugged into a machine you care about. The machine reboots into the installed system when it is done. Pick "Boot from the next device" to skip.

The install works without internet; Ubuntu packages come from the ISO itself. With internet it also applies security updates.

## Building

```bash
make controller-iso
make slave-iso
```

Without `make`, build the Linux binaries (see the `build` target) and run `bash iso/build.sh controller` or `bash iso/build.sh slave`.

`iso/build.sh` takes the official Ubuntu 24.04 live-server ISO (downloaded once into `iso/cache/` and checked against Ubuntu's SHA256SUMS), adds `/autoinstall.yaml`, a boot menu and the `/aicluster` payload, and writes `out/aicluster-<role>-<version>.iso` plus a `.sha256`. Ubuntu's own boot setup (BIOS and UEFI, CD and USB) is replayed unchanged by `xorriso -boot_image any replay`.

It needs `curl git jq unzip zstd sha256sum xorriso` and `libgomp1` (Ubuntu: `apt install curl git jq libgomp1 unzip zstd xorriso`). If xorriso is missing and Docker is available it builds inside `iso/Dockerfile` instead. On Windows, run it inside WSL with Ubuntu 24.04. Keep `CACHE_DIR` and `WORK_DIR` on the Linux side (e.g. `/root/iso-cache`); building through `/mnt/c` is much slower.

Slave images download Ollama and llama.cpp from their GitHub releases and verify GitHub's sha256 digests:
- **Ollama:** the latest release, re-packed as gzip because a fresh install may not have `zstd`.
- **llama.cpp:** the newest CPU build. Its builds are published as pre-releases, so the script takes the newest release that has one.
- **OpenMP:** llama.cpp needs `libgomp.so.1`, which is copied from the build machine. Build on Ubuntu 24.04, or in the Docker image, so the library matches the target.

The exact versions go into `/etc/aicluster/build-info` on the installed node.

| Variable | Effect |
|---|---|
| `UBUNTU_ISO=path` | use a local Ubuntu ISO instead of downloading |
| `OLLAMA_VERSION`, `LLAMACPP_VERSION` | pin releases, e.g. `v0.40.13`, `b12032` |
| `SKIP_OLLAMA=1`, `SKIP_LLAMACPP=1` | leave a runtime out |
| `SSH_KEY_FILE=path` | install OpenSSH and allow this key for `aicluster-admin` (path must be inside the repo when building in Docker) |
| `CONSOLE_PASSWORD=…` | allow console login as `aicluster-admin` |
| `KERNEL_ARGS=console=ttyS0` | extra installer kernel arguments (serial console in VMs) |
| `GRUB_TIMEOUT=10` | seconds before the install starts |

## Access after install

There is no SSH and no console login unless the image was built with `SSH_KEY_FILE` or `CONSOLE_PASSWORD`. The console shows a status screen above the login prompt instead, refreshed every 15 s:

- **Controller:** dashboard URL, OpenAI API URL, the admin token, and the CA certificate's SHA-256 fingerprint. Anyone at the physical console can read the token, the same way anyone there could reinstall the machine. To replace it, delete `/var/lib/aicluster/controller/admin.token` and restart `aiclusterd`; a new one is generated.
- **Slave:** node ID, adoption status, and the pairing code when pairing is required.

On a Slave, the console escape hatch `aicluster-agent factory-reset` needs a console login, so build with `CONSOLE_PASSWORD` if you want it.

## What the install sets up

- **Controller:**
  - `aicluster-provisioner.service` (Python, unix socket `/run/aicluster/provisioner.sock`).
  - `aiclusterd.service` (state in `/var/lib/aicluster/controller`, extra flags in `/etc/aicluster/controller.env` as `AICLUSTERD_ARGS=`).
  - Both run as the unprivileged `aicluster` user.
  - The catalog goes in `/usr/share/aicluster/catalog`; local additions go in `/etc/aicluster/catalog.d`.
- **Slave:**
  - `ollama.service` runs as the `ollama` user, listens on loopback only, and keeps models in `/var/lib/aicluster/models/ollama`.
  - `aicluster-agent.service` runs as root, takes extra flags from `/etc/aicluster/agent.env` as `AGENT_ARGS=`, and expects llama.cpp in `/usr/lib/aicluster/llama.cpp`.
- **Both:** `aicluster-console.timer` (the status screen) and `/etc/aicluster/role`.

`iso/payload/install-target.sh` does all of this and is safe to re-run.

## Tested

QEMU/KVM inside WSL2 on an isolated bridge with DHCP, 6 GB RAM and 4 vCPUs per VM. Ubuntu 24.04.5, Ollama v0.40.13, llama.cpp b12032:

- **Controller:**
  - Installed unattended under UEFI.
  - Dashboard, CA download, gateway and provisioner all answer.
  - The console screen shows the token and the CA fingerprint the Controller actually serves.
  - The Controller survives a reboot.
- **Slaves:**
  - One installed under BIOS and one under UEFI.
  - Both were discovered and auto-adopted with their hostnames as names.
- **Models:**
  - Only the Controller had internet. It cached `qwen2.5:0.5b` from registry.ollama.ai, a Slave's Ollama pulled it through the shim, and a chat through the Controller answered.
  - `qwen2.5:1.5b` was pooled across both Slaves: leader 60% on its CPU, worker 40% (11 layers) over the RPC tunnel. A chat through the Controller answered.

Not tested: physical hardware, booting from USB, GPUs.

## Not covered yet

- **GPU drivers:** NVIDIA and AMD drivers are not in the image. Ollama's bundled CUDA libraries need the NVIDIA kernel driver. Until drivers come through the Controller, Slaves run models on the CPU.
- **Raw USB image:** writing the ISO to a stick with `dd` or a USB imager works, because Ubuntu's ISO is hybrid. A separate raw image is not built.
