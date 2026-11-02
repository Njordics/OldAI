package drivers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A trimmed-down restricted Packages file: Pascal (1B80) is supported only by
// the proprietary modules, Ada (2684) by both, and Blackwell (2B85) only by
// the newest non-server open driver.
const testPackages = `Package: nvidia-driver-580-server
Version: 580.126.09-0ubuntu0.24.04.1
Modaliases: nvidia(pci:v000010DEd00001B80sv*sd*bc03sc*i*, pci:v000010DEd00002684sv*sd*bc03sc*i*)

Package: nvidia-driver-580-server-open
Version: 580.178.04-0ubuntu0.24.04.1
Modaliases: nvidia(pci:v000010DEd00002684sv*sd*bc03sc*i*)

Package: nvidia-driver-570-server
Version: 570.1-0ubuntu1
Modaliases: nvidia(pci:v000010DEd00001B80sv*sd*bc03sc*i*, pci:v000010DEd00002684sv*sd*bc03sc*i*)

Package: nvidia-driver-595-open
Version: 595.99.02-0ubuntu0.24.04.1
Modaliases: nvidia(pci:v000010DEd00002684sv*sd*bc03sc*i*,
 pci:v000010DEd00002B85sv*sd*bc03sc*i*)

Package: nvidia-driver-590-server
Version: 590.1-0ubuntu1
Depends: something

`

const testPackagesUpdates = `Package: nvidia-driver-580-server
Version: 580.178.04-0ubuntu0.24.04.1
Modaliases: nvidia(pci:v000010DEd00001B80sv*sd*bc03sc*i*, pci:v000010DEd00002684sv*sd*bc03sc*i*)

Package: nvidia-driver-570-server
Version: 570.0-0ubuntu1
Modaliases: nvidia(pci:v000010DEd00009999sv*sd*bc03sc*i*)
`

func testIndex(t *testing.T) *Index {
	t.Helper()
	ix := NewIndex()
	var extra strings.Builder
	for _, sfx := range []string{"580-server", "580-server-open", "570-server", "595-open"} {
		extra.WriteString("Package: nvidia-headless-no-dkms-" + sfx + "\nVersion: 1\n\n")
		extra.WriteString("Package: nvidia-utils-" + strings.TrimSuffix(sfx, "-open") + "\nVersion: 1\n\n")
		base := "linux-modules-nvidia-" + sfx
		extra.WriteString("Package: " + base + "-generic\nVersion: 6.8.0-147.147\nDepends: " + base + "-6.8.0-147-generic (= 6.8.0-147.147), nvidia-kernel-common\n\n")
		extra.WriteString("Package: " + base + "-generic-hwe-24.04\nVersion: 6.17.0-9.9\nDepends: " + base + "-6.17.0-9-generic (= 6.17.0-9.9)\n\n")
		extra.WriteString("Package: " + base + "-generic-6.14\nVersion: 6.14.0-30.30\nDepends: " + base + "-6.14.0-30-generic (= 6.14.0-30.30)\n\n")
		extra.WriteString("Package: " + base + "-6.8.0-107-generic\nVersion: 6.8.0-107.107\n\n")
	}
	for _, s := range []string{testPackages, testPackagesUpdates, extra.String()} {
		if err := ix.AddPackages(strings.NewReader(s), false); err != nil {
			t.Fatal(err)
		}
	}
	return ix
}

const (
	pascal    = "pci:v000010DEd00001B80sv00001043sd000085AAbc03sc00i00"
	ada       = "pci:v000010DEd00002684sv00001043sd000088E2bc03sc00i00"
	blackwell = "pci:v000010DEd00002B85sv0000196Esd00001234bc03sc00i00"
)

func TestChoose(t *testing.T) {
	ix := testIndex(t)
	for _, tc := range []struct {
		name    string
		cards   []string
		kernel  string
		driver  string
		modules string
	}{
		{"ada prefers server, then open", []string{ada}, "6.8.0-48-generic", "nvidia-driver-580-server-open", "linux-modules-nvidia-580-server-open-generic"},
		{"pascal has no open modules", []string{pascal}, "6.8.0-48-generic", "nvidia-driver-580-server", "linux-modules-nvidia-580-server-generic"},
		{"mixed cards need a driver for both", []string{ada, pascal}, "6.8.0-48-generic", "nvidia-driver-580-server", "linux-modules-nvidia-580-server-generic"},
		{"blackwell falls back to non-server", []string{blackwell}, "6.8.0-48-generic", "nvidia-driver-595-open", "linux-modules-nvidia-595-open-generic"},
		{"hwe kernel stays on hwe", []string{ada}, "6.17.0-5-generic", "nvidia-driver-580-server-open", "linux-modules-nvidia-580-server-open-generic-hwe-24.04"},
		{"versioned line", []string{ada}, "6.14.0-2-generic", "nvidia-driver-580-server-open", "linux-modules-nvidia-580-server-open-generic-6.14"},
		{"unknown line moves to the main one", []string{ada}, "6.11.0-1-generic", "nvidia-driver-580-server-open", "linux-modules-nvidia-580-server-open-generic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ix.Choose(tc.cards, tc.kernel)
			if err != nil {
				t.Fatal(err)
			}
			if c.Driver.Package != tc.driver || c.Packages[len(c.Packages)-1] != tc.modules {
				t.Fatalf("got %s %v", c.Driver.Package, c.Packages)
			}
			if !strings.HasPrefix(c.Packages[0], "nvidia-headless-no-dkms-") || !strings.HasPrefix(c.Packages[1], "nvidia-utils-") ||
				strings.HasSuffix(c.Packages[1], "-open") {
				t.Fatalf("userland packages: %v", c.Packages)
			}
		})
	}
	// The newer -updates stanza replaced the release one.
	if c, _ := ix.Choose([]string{pascal}, "6.8.0-48-generic"); c.Driver.Version != "580.178.04-0ubuntu0.24.04.1" {
		t.Fatalf("version %s", c.Driver.Version)
	}
	// An older stanza must not replace a newer one.
	if c, _ := ix.Choose([]string{"pci:v000010DEd00009999sv0sd0bc03sc00i00"}, "6.8.0-48-generic"); c != nil {
		t.Fatalf("an older stanza won: %+v", c)
	}
	if _, err := ix.Choose([]string{"pci:v000010DEd0000FFFFsv0sd0bc03sc00i00"}, "6.8.0-48-generic"); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("unknown card: %v", err)
	}
	if _, err := ix.Choose([]string{ada}, "weird"); err == nil {
		t.Fatal("chose modules for a kernel it can't parse")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"580.178.04-0ubuntu0.24.04.1", "580.126.09-0ubuntu0.24.04.1", 1},
		{"1.0", "1.0", 0},
		{"1.0~rc1", "1.0", -1},
		{"1:0.1", "2.0", 1},
		{"1.0-1", "1.0-1ubuntu1", -1},
		{"6.8.0-107.107", "6.8.0-47.47", 1},
		{"1.0a", "1.0+", -1},
	} {
		got := CompareVersions(tc.a, tc.b)
		if (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) {
			t.Errorf("CompareVersions(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMirror(t *testing.T) {
	var hits, fail atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() == 1 {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/ubuntu/dists/noble/InRelease":
			_, _ = w.Write([]byte("release " + time.Now().String()))
		case "/ubuntu/pool/restricted/n/nvidia/x.deb":
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte("deb"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	var offline atomic.Bool
	m := &Mirror{Upstream: up.URL + "/ubuntu", Dir: t.TempDir(), MaxAge: time.Hour, Offline: offline.Load}
	srv := httptest.NewServer(http.StripPrefix("/apt/ubuntu", m))
	defer srv.Close()
	get := func(p string) (int, string) {
		resp, err := http.Get(srv.URL + "/apt/ubuntu/" + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Concurrent requests for one package share a single download.
	done := make(chan string, 3)
	for range 3 {
		go func() { _, b := get("pool/restricted/n/nvidia/x.deb"); done <- b }()
	}
	for range 3 {
		if b := <-done; b != "deb" {
			t.Fatalf("body %q", b)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hit %d times for one package", hits.Load())
	}
	// Packages are kept; index files are refreshed once stale.
	_, first := get("dists/noble/InRelease")
	_, again := get("dists/noble/InRelease")
	if first != again {
		t.Fatal("a fresh index file was fetched again")
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(filepath.Join(m.Dir, "dists", "noble", "InRelease"), old, old)
	if _, b := get("dists/noble/InRelease"); b == first {
		t.Fatal("a stale index file was not refreshed")
	}
	// A failing upstream falls back to the cached copy.
	_ = os.Chtimes(filepath.Join(m.Dir, "dists", "noble", "InRelease"), old, old)
	fail.Store(1)
	if code, _ := get("dists/noble/InRelease"); code != http.StatusOK {
		t.Fatalf("stale copy not served when the upstream fails: %d", code)
	}
	if code, _ := get("dists/noble/Release"); code != http.StatusBadGateway {
		t.Fatalf("uncached file with the upstream down: %d", code)
	}
	fail.Store(0)
	if code, _ := get("dists/noble/missing"); code != http.StatusNotFound {
		t.Fatalf("missing file: %d", code)
	}
	offline.Store(true)
	before := hits.Load()
	if code, _ := get("pool/restricted/n/nvidia/x.deb"); code != http.StatusOK {
		t.Fatalf("cached package offline: %d", code)
	}
	if code, _ := get("dists/noble-updates/InRelease"); code != http.StatusServiceUnavailable || hits.Load() != before {
		t.Fatalf("offline fetch: %d", code)
	}
	for _, bad := range []string{"etc/passwd", "dists/../../x", "pool//x", "dists/a\\b"} {
		if _, err := m.fetch(context.Background(), bad); !errors.Is(err, errNotFound) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
