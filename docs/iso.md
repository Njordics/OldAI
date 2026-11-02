# Installer ISOs

Two images, both unattended Ubuntu Server 24.04 installs:

| Image | Installs | Hostname |
|---|---|---|
| `aicluster-controller-<version>.iso` | `aiclusterd`, the `aicluster` CLI, the provisioner and the model catalog | `aicluster-controller` |
| `aicluster-slave-<version>.iso` | `aicluster-agent`, Ollama, and llama.cpp's `ggml-rpc-server`/`llama-server` plus the OpenMP library they need (pooled models) | `aicluster-node-<6 chars of machine-id>` |

Booting either one shows a menu that starts the install after 10 seconds. **The install erases every internal disk** without asking: that is the point of an unattended image, so don't leave the stick plugged into a machine you care about. The machine reboots into the installed system when it is done. Pick "Boot from the next device" to skip.

## Disk wipe

Before the installer looks at the disks, `iso/payload/wipe-disks.sh` erases every internal disk.
- **What's skipped:** USB and removable drives, the install medium itself, optical drives and virtual block devices.
- **On each disk:**
  1. Stop any LVM, RAID or swap that uses it.
  2. Remove every partition, filesystem, RAID and LVM signature, and zap both GPT copies.
  3. Discard (TRIM) the whole device.
- **SSDs and NVMe drives:** after the discard the old data reads back as zeros or is gone.
- **Spinning disks** can't discard, so their first and last 64 MiB are zeroed instead. Nothing can mount or see the old data, but it stays on the platters until overwritten, and forensic tools could recover it.

The OS then goes on the largest disk. Any others are left empty.

## Slave network

The Controller image starts `aiclusterd --slave-net=auto` (in `/etc/aicluster/controller.env`).
- **Joining:** Slaves are found and adopted on your home network as usual.
- **Moving:** the Controller then moves them to its own subnet on the same wire, `10.10.10.0/24` unless your network already uses it. The Controller is `10.10.10.1` and is the Slaves' only DHCP and DNS server.
- **Your network:** the Controller keeps its home address, and its DHCP answers only adopted Slaves, so your router and other devices are unaffected.
- **Limits:** this is a logical separation on the shared switch, not a security boundary. Slaves still have no internet access, because the Controller never forwards packets.
- **Status:** Settings shows the subnet, and each node's details show which network it is on.
- **Opting out:** set `AICLUSTERD_ARGS=--slave-net=off` and restart `aiclusterd`.

A Slave that can't reach the Controller on the new subnet within 90 s rolls back to the home network and retries after 10 minutes. A Slave that loses the Controller for 10 minutes goes back to the home network on its own, and so does a released or factory-reset one. Details: [protocol.md §12](protocol.md#12-slave-network).

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

- **Controller:** dashboard URL, OpenAI API URL, the **login code** (`XXXX-XXXX`) for signing in to the dashboard, and the CA certificate's SHA-256 fingerprint.
  - Anyone at the physical console can read the code, the same way anyone there could reinstall the machine. Settings → **New login code** replaces it.
  - The long admin token for the CLI and API stays in `/var/lib/aicluster/controller/admin.token`. To replace it, delete that file and restart `aiclusterd`.
- **Slave:** node ID, adoption status, and the pairing code when pairing is required.

On a Slave, the console escape hatch `aicluster-agent factory-reset` needs a console login, so build with `CONSOLE_PASSWORD` if you want it.

## Updating

Use **Update Controller and Slaves now**, in Settings → Updates or in the pop-up that appears when an update is available.
1. The Controller downloads the update that GitHub's CI built for the newest commit and checks it.
2. It installs the update on itself and restarts. The dashboard reconnects on its own.
3. It then updates the nodes one at a time.

If anything doesn't come back on the new version, it is rolled back and the remaining nodes are left alone. Details: [protocol.md §13](protocol.md#13-in-place-updates).

- **What's updated:** the Controller software, the provisioner, the model catalog and the node agent. Ollama and llama.cpp stay as installed.
- **Older installs:** machines installed from an ISO older than this feature have no updater. Reinstall each one once from a current ISO; later updates come through the button. If you reinstall the Controller, release its Slaves first, or they won't trust the new one.
- **During the update:** a node's models are unavailable for the few seconds its agent restarts.

## What the install sets up

- **Controller:**
  - `aicluster-provisioner.service` (Python, unix socket `/run/aicluster/provisioner.sock`).
  - `aiclusterd.service` (state in `/var/lib/aicluster/controller`, flags in `/etc/aicluster/controller.env` as `AICLUSTERD_ARGS=`, default `--slave-net=auto`). It holds only the network capabilities the Slave network needs: adding its address and serving DHCP and DNS.
  - `/etc/sysctl.d/60-aicluster.conf` keeps IP forwarding off.
  - Both run as the unprivileged `aicluster` user.
  - The catalog goes in `/usr/share/aicluster/catalog`; local additions go in `/etc/aicluster/catalog.d`.
- **Slave:**
  - `ollama.service` runs as the `ollama` user, listens on loopback only, and keeps models in `/var/lib/aicluster/models/ollama`.
  - `aicluster-agent.service` runs as root, takes extra flags from `/etc/aicluster/agent.env` as `AGENT_ARGS=`, and expects llama.cpp in `/usr/lib/aicluster/llama.cpp`. When it stops, `aicluster-agent fans-restore` hands any fans it boosted back to the firmware.
- **Both:**
  - `aicluster-console.timer` (the status screen) and `/etc/aicluster/role`.
  - The updater: when a daemon stages an update in `/var/lib/aicluster/update`, `aicluster-update.path` starts `/usr/lib/aicluster/apply-update` as root.

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

- **Wipe:** a Slave was reinstalled over its previous LVM install, with a second disk carrying a partition and marker data at its start, middle and end. Both disks were discarded, the second came out with no partitions and no markers, and the OS installed on the first.
- **Slave network:**
  - The Controller picked `10.10.10.0/24` next to its home address. Both Slaves were adopted on the home network, then moved to `10.10.10.10`/`.11`.
  - On a Slave: the only address is the Slave one, DNS is the Controller, and cluster names resolve while `example.com` is refused. The home router and the internet are unreachable.
  - A normal and a pooled model both served chats over the Slave network.
  - Releasing a Slave put it back on the home network.

- **In-place updates:** a Controller and two Slaves were installed from ISOs of one commit, and later builds were served from a stand-in for GitHub's API.
  - **Normal update:** the Controller installed the update, restarted on the new commit and waited for the updater's verdict, about 20 s. It then updated each Slave in turn, waiting for each one's verdict. The runtime versions in `build-info` were kept, and both Slaves stayed on the Slave network.
  - **Broken Controller build** (an `aiclusterd` that exits at once): the updater restored the previous binaries and database. The Controller came back on the old commit and showed "rolled back" with the reason.
  - **Broken agent build:** the first Slave rolled back to its previous agent and reported the failure. The rollout stopped without touching the second Slave.

Not tested: physical hardware, booting from USB, GPUs, spinning disks (the non-discard wipe path), a home network that already uses `10.10.10.0/24`.

## GPU drivers

No GPU drivers are in the image. When a Slave has an NVIDIA card, the Controller picks the driver from Ubuntu's archive and the Slave installs it through the Controller, then reboots once if the driver needs a newer kernel. Intel and AMD graphics, including those built into the processor, get Mesa's Vulkan driver the same way, without a reboot. Settings → GPUs turns the use of built-in graphics off, for older chips where they are slower than the processor. The Controller needs internet the first time; later Slaves with the same card get it from the Controller's copy. Node details in the dashboard show the driver's progress. Details: [protocol.md §14](protocol.md#14-gpu-drivers).

## Not covered yet

- **Pooled models on GPUs:** the llama.cpp in the image is the CPU build, so models split across nodes run on their CPUs.
- **Raw USB image:** writing the ISO to a stick with `dd` or a USB imager works, because Ubuntu's ISO is hybrid. A separate raw image is not built.
