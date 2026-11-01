package modelcache

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aicluster/test/fakeregistry"
)

func TestDownloadOnceAndServe(t *testing.T) {
	up := fakeregistry.New(1 << 20)
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	c, err := New(t.TempDir(), upSrv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	c.OnReady = func(ref string) { ready <- ref }

	const ref = "library/tiny:1b-q4_K_M"
	if _, err := c.Ensure(ref); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Ensure(ref) // second call joins the running job
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := c.Wait(ctx, ref); e.State != StateReady || e.Progress != 1 {
		t.Fatalf("status = %+v", e)
	}
	select {
	case got := <-ready:
		if got != ref {
			t.Fatalf("OnReady(%q)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnReady not called")
	}
	if n := up.Count("/v2/library/tiny/manifests/1b-q4_K_M"); n != 1 {
		t.Fatalf("manifest fetched %d times", n)
	}
	// Already cached: no new upstream traffic.
	_, _ = c.Ensure(ref)
	if n := up.Count("/v2/library/tiny/manifests/1b-q4_K_M"); n != 1 {
		t.Fatalf("re-fetched a cached model (%d)", n)
	}

	srv := httptest.NewServer(c)
	defer srv.Close()
	m, _, _ := c.Manifest(ref)
	layer := m.Layers[0]
	req, _ := http.NewRequest("GET", srv.URL+"/v2/library/tiny/blobs/"+layer.Digest, nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 10 {
		t.Fatalf("range: %d len=%d", resp.StatusCode, len(body))
	}
	if r, _ := http.Get(srv.URL + "/v2/library/other/manifests/x"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("uncached manifest: %d", r.StatusCode)
	}
	if r, _ := http.Get(srv.URL + "/v2/library/tiny/blobs/sha256:../../etc"); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad digest: %d", r.StatusCode)
	}
}

func TestMissingModelFails(t *testing.T) {
	up := fakeregistry.New(1024)
	up.Missing["library/nope:1b"] = true
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()
	c, _ := New(t.TempDir(), upSrv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, _ = c.Ensure("library/nope:1b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := c.Wait(ctx, "library/nope:1b"); e.State != StateFailed || e.Error == "" {
		t.Fatalf("status = %+v", e)
	}
	c.Offline.Store(true)
	if _, err := c.Ensure("library/other:1b"); err == nil {
		t.Fatal("offline mode fetched from upstream")
	}
}
