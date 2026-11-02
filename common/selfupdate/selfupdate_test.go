package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	typ        byte
}

func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: e.typ}
		if e.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
			h.Linkname = "/etc/passwd"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func good(t *testing.T) []byte {
	return tarGz(t,
		entry{name: "aicluster/manifest.json", body: `{"commit":"abcdef1234567"}`},
		entry{name: "aicluster/install-target.sh", body: "#!/bin/bash\n"},
		entry{name: "aicluster/apply-update", body: "#!/bin/bash\n"},
		entry{name: "aicluster/bin/aicluster-agent", body: "binary"},
	)
}

func TestSaveAndStage(t *testing.T) {
	dir := t.TempDir()
	b := good(t)
	p, err := Save(dir, bytes.NewReader(b), "sha256:"+sum(b))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Stage(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Commit != "abcdef1234567" {
		t.Fatalf("manifest %+v", m)
	}
	fi, err := os.Stat(filepath.Join(dir, "staged", "aicluster", "bin", "aicluster-agent"))
	if err != nil || (fi.Mode().Perm()&0o100 == 0 && os.PathSeparator == '/') {
		t.Fatalf("agent binary: %v %v", fi, err)
	}
}

func TestSaveRejectsWrongChecksum(t *testing.T) {
	dir := t.TempDir()
	b := good(t)
	if err := os.WriteFile(filepath.Join(dir, "bundle.tar.gz"), []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(dir, bytes.NewReader(b), strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	// The previous bundle is untouched.
	if got, _ := os.ReadFile(filepath.Join(dir, "bundle.tar.gz")); string(got) != "previous" {
		t.Fatalf("previous bundle replaced: %q", got)
	}
}

func TestExtractRefusesUnsafeEntries(t *testing.T) {
	for name, e := range map[string]entry{
		"parent":   {name: "../evil", body: "x"},
		"absolute": {name: "/etc/evil", body: "x"},
		"symlink":  {name: "aicluster/link", typ: tar.TypeSymlink},
		"hardlink": {name: "aicluster/hard", typ: tar.TypeLink},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "b.tar.gz")
			_ = os.WriteFile(p, tarGz(t, e), 0o644)
			if err := Extract(p, filepath.Join(dir, "out")); err == nil {
				t.Fatal("unsafe entry accepted")
			}
		})
	}
}

func TestStageNeedsManifestAndScripts(t *testing.T) {
	dir := t.TempDir()
	b := tarGz(t, entry{name: "aicluster/manifest.json", body: `{"commit":"abc1234"}`})
	p, err := Save(dir, bytes.NewReader(b), sum(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(dir, p); err == nil || !strings.Contains(err.Error(), "install-target.sh") {
		t.Fatalf("want missing-script error, got %v", err)
	}
}

func TestSameCommit(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"abcdef1234", "abcdef1", true},
		{"abcdef1", "abcdef1234", true},
		{"abcdef1", "abcdef2", false},
		{"", "", false},
		{"abc", "abcdef1", false},
	} {
		if got := SameCommit(c.a, c.b); got != c.want {
			t.Errorf("SameCommit(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
