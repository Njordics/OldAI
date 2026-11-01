package inventory

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"

	"aicluster/common/proto"
)

// Health samples the cheap per-heartbeat metrics. GPU and inference fields
// are filled by later build steps.
func Health(modelDir string) proto.Health {
	h := proto.Health{
		MemTotal: meminfo("MemTotal"),
		MemAvail: meminfo("MemAvailable"),
	}
	if f := strings.Fields(readTrim("proc/uptime")); len(f) > 0 {
		up, _ := strconv.ParseFloat(f[0], 64)
		h.UptimeS = int64(up)
	}
	if f := strings.Fields(readTrim("proc/loadavg")); len(f) > 0 {
		h.Load1, _ = strconv.ParseFloat(f[0], 64)
	}
	if _, free, ok := diskUsage(modelDir); ok {
		h.DiskFree = free
	}
	return h
}

var fallbackBootID = func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

// BootID changes on every reboot; the Controller uses it to detect restarts.
func BootID() string {
	if id := readTrim("proc/sys/kernel/random/boot_id"); id != "" {
		return id
	}
	return fallbackBootID
}
