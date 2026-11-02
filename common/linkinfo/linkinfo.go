// Package linkinfo reads a network interface's negotiated speed and duplex
// from Linux's /sys/class/net (protocol §16). Elsewhere it reports nothing.
package linkinfo

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"aicluster/common/proto"
)

// Root is where Linux lists network interfaces (tests point it elsewhere).
var Root = "/sys/class/net"

// Read describes iface's link. Speed is 0 when the driver doesn't say (it
// reports -1 while the link is down, and nothing for Wi-Fi).
func Read(iface string) *proto.Link {
	if iface == "" {
		return nil
	}
	dir := filepath.Join(Root, iface)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	l := &proto.Link{Iface: iface}
	if b, err := os.ReadFile(filepath.Join(dir, "speed")); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 0 {
			l.Mbps = v
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "duplex")); err == nil {
		if d := strings.TrimSpace(string(b)); d == "full" || d == "half" {
			l.Duplex = d
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "wireless")); err == nil {
		l.Wireless = true
	}
	return l
}

// DefaultIface is the interface of the IPv4 default route, from
// /proc/net/route; "" when there is none or this isn't Linux.
func DefaultIface() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) > 2 && fs[1] == "00000000" && fs[0] != "Iface" {
			return fs[0]
		}
	}
	return ""
}

// Slow reports why a link is slower than a 1 Gb copper link should be, or
// "" when it looks right.
func Slow(l *proto.Link) string {
	switch {
	case l == nil:
		return ""
	case l.Wireless:
		return "Wi-Fi"
	case l.Duplex == "half":
		return "half duplex"
	case l.Mbps > 0 && l.Mbps < 1000:
		return strconv.Itoa(l.Mbps) + " Mb/s"
	}
	return ""
}
