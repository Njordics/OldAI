package integration

import (
	"context"
	"testing"

	"aicluster/common/proto"
	"aicluster/controller/core"
)

// A second node copies a model from the first instead of the Controller,
// and a node whose peers have gone falls back to the Controller.
func TestPeerCopy(t *testing.T) {
	c := startController(t, core.ModeAuto, func(cfg *core.Config) { cfg.PeerMinBlob = 1 })
	s1, s2, s3 := startSlave(t, c), startSlave(t, c), startSlave(t, c)
	adoptAll(t, c, s1, s2, s3)
	ctx := context.Background()
	install := func(s *slave) {
		t.Helper()
		as, err := c.Install(ctx, core.InstallRequest{ModelID: "qwen2.5:0.5b", Nodes: []string{s.NodeID()}})
		if err != nil {
			t.Fatal(err)
		}
		c.waitModel(t, as[0].ID, proto.ModelLoaded)
	}

	install(s1)
	if s1.PeerServed() != 0 {
		t.Fatal("the first node served files before anyone else had the model")
	}
	install(s2)
	if s1.PeerServed() == 0 {
		t.Fatal("the second node didn't copy from the first")
	}
	if s2.PeerServed() != 0 {
		t.Fatal("the second node served itself")
	}

	// Both holders go away before the Controller notices: the third node
	// is sent to a dead peer, falls back to the Controller, and still gets
	// a verified copy.
	served := s1.PeerServed() + s2.PeerServed()
	s1.stop()
	s2.stop()
	install(s3)
	if s1.PeerServed()+s2.PeerServed() != served {
		t.Fatal("stopped nodes served files")
	}
}

func TestNetTest(t *testing.T) {
	old := core.NetTestBytes
	core.NetTestBytes = 4 << 20
	defer func() { core.NetTestBytes = old }()
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	adoptAll(t, c, s)
	r, err := c.NetTest(context.Background(), s.NodeID())
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != "" || r.ToNodeMbps <= 0 || r.FromNodeMbps <= 0 || r.Bytes != 4<<20 {
		t.Fatalf("result %+v", r)
	}
	if got := c.LastNetTest(s.NodeID()); got != r {
		t.Fatal("result not kept")
	}
	t.Logf("loopback: %.0f Mb/s to, %.0f Mb/s from", r.ToNodeMbps, r.FromNodeMbps)
}
