package linkinfo

import (
	"os"
	"path/filepath"
	"testing"

	"aicluster/common/proto"
)

func TestRead(t *testing.T) {
	Root = t.TempDir()
	write := func(p, v string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(Root, p)), 0o755)
		_ = os.WriteFile(filepath.Join(Root, p), []byte(v+"\n"), 0o644)
	}
	write("eno1/speed", "100")
	write("eno1/duplex", "full")
	write("wlp2s0/speed", "-1")
	_ = os.MkdirAll(filepath.Join(Root, "wlp2s0", "wireless"), 0o755)
	write("enp1s0/speed", "1000")
	write("enp1s0/duplex", "half")

	if l := Read("eno1"); l.Mbps != 100 || l.Duplex != "full" || Slow(l) != "100 Mb/s" {
		t.Fatalf("eno1: %+v %q", l, Slow(l))
	}
	if l := Read("wlp2s0"); !l.Wireless || l.Mbps != 0 || Slow(l) != "Wi-Fi" {
		t.Fatalf("wifi: %+v", l)
	}
	if l := Read("enp1s0"); Slow(l) != "half duplex" {
		t.Fatalf("half: %+v", l)
	}
	if Read("nope") != nil || Read("") != nil {
		t.Fatal("missing interface")
	}
	if Slow(&proto.Link{Mbps: 2500, Duplex: "full"}) != "" || Slow(&proto.Link{}) != "" {
		t.Fatal("fast or unknown links flagged")
	}
}
