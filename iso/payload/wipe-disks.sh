#!/bin/bash
# Runs in the installer (autoinstall early-commands) before storage is
# probed: erases every internal disk so nothing from a previous system
# survives. USB and removable devices, the install medium, optical drives
# and virtual block devices are left alone.
#
# Per disk: stop LVM/RAID that use it, remove every partition and
# filesystem/RAID/LVM signature, zap both GPT copies, then discard (TRIM)
# the whole device. On disks that can't discard (spinning drives) the
# first and last 64 MiB are zeroed instead; their old blocks stay on the
# platters until overwritten, but nothing can see or mount them.
set -u
log() { echo "aicluster-wipe: $*"; }

# The disk holding the installer, wherever it was booted from.
medium=""
src=$(findmnt -n -o SOURCE /cdrom 2>/dev/null || true)
if [ -n "$src" ] && [ -b "$src" ]; then
	medium=$(lsblk -n -d -o PKNAME "$src" 2>/dev/null)
	[ -n "$medium" ] || medium=$(basename "$src")
fi

# Stop anything that could hold disks open.
swapoff -a 2>/dev/null || true
vgchange -an >/dev/null 2>&1 || true
mdadm --stop --scan >/dev/null 2>&1 || true
dmsetup remove_all -f >/dev/null 2>&1 || true

lsblk -d -n -b -o NAME,TYPE,TRAN,RM,RO,SIZE | while read -r name type tran rm ro size; do
	[ "$type" = disk ] || continue
	case "$name" in loop* | sr* | ram* | zram* | fd* | nbd*) continue ;; esac
	if [ "$name" = "$medium" ] || [ "$tran" = usb ] || [ "$rm" = 1 ] || [ "$ro" = 1 ] || [ "${size:-0}" -eq 0 ]; then
		log "skipping /dev/$name (install medium, removable, USB or read-only)"
		continue
	fi
	dev=/dev/$name
	log "wiping $dev ($((size / 1000000000)) GB)"
	for part in $(lsblk -n -l -o NAME "$dev" | tail -n +2); do
		wipefs -a -f "/dev/$part" >/dev/null 2>&1 || true
	done
	wipefs -a -f "$dev" >/dev/null 2>&1 || true
	sgdisk --zap-all "$dev" >/dev/null 2>&1 || true
	if blkdiscard -f "$dev" >/dev/null 2>&1; then
		log "$dev discarded"
	else
		mib=$((size / 1048576))
		dd if=/dev/zero of="$dev" bs=1M count=64 oflag=direct conv=notrunc status=none 2>/dev/null || true
		if [ "$mib" -gt 128 ]; then
			dd if=/dev/zero of="$dev" bs=1M seek=$((mib - 64)) count=64 oflag=direct conv=notrunc status=none 2>/dev/null || true
		fi
		log "$dev can't discard; cleared its first and last 64 MiB"
	fi
	partprobe "$dev" >/dev/null 2>&1 || blockdev --rereadpt "$dev" >/dev/null 2>&1 || true
done
udevadm settle 2>/dev/null || true
exit 0
