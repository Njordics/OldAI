package pairing

import (
	"bytes"
	"testing"
)

func TestCPaceAgreesOnlyWithSameCode(t *testing.T) {
	nonce, fp := []byte("nonce-s"), "sha256:abc"
	for _, tc := range []struct {
		ctrlCode, slaveCode string
		same                bool
	}{
		{"123456", "123456", true},
		{"123456", "123457", false},
	} {
		ya, st, err := ControllerStart(tc.ctrlCode, "node", nonce, fp)
		if err != nil {
			t.Fatal(err)
		}
		yb, kSlave, err := SlaveExchange(tc.slaveCode, "node", nonce, fp, ya)
		if err != nil {
			t.Fatal(err)
		}
		kCtrl, err := st.Finish(yb)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(kCtrl, kSlave) != tc.same {
			t.Fatalf("codes %s/%s: key agreement = %v", tc.ctrlCode, tc.slaveCode, !tc.same)
		}
	}
}

func TestCPaceBoundToChannel(t *testing.T) {
	ya, st, _ := ControllerStart("123456", "node", []byte("n1"), "sha256:a")
	yb, kSlave, _ := SlaveExchange("123456", "node", []byte("n1"), "sha256:EVIL", ya)
	kCtrl, _ := st.Finish(yb)
	if bytes.Equal(kCtrl, kSlave) {
		t.Fatal("keys agree across different bootstrap fingerprints")
	}
}

func TestMACNilKey(t *testing.T) {
	if MAC(nil, "x") != nil || !CheckMAC(nil, nil, "x") || CheckMAC(nil, []byte{1}, "x") {
		t.Fatal("nil-key MAC semantics")
	}
	k := []byte("k")
	if !CheckMAC(k, MAC(k, "l", []byte("a"), []byte("bc")), "l", []byte("a"), []byte("bc")) {
		t.Fatal("MAC round trip")
	}
	if CheckMAC(k, MAC(k, "l", []byte("ab"), []byte("c")), "l", []byte("a"), []byte("bc")) {
		t.Fatal("MAC must length-prefix parts")
	}
}
