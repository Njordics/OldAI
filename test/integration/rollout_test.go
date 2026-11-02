package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/selfupdate"
	"aicluster/controller/core"
	"aicluster/controller/updates"
	"aicluster/controller/web"
	"aicluster/slave-agent/agent"
)

const (
	oldCommit = "base1234aaaa"
	newCommit = "head5678bbbb"
)

// fakeInstaller stands in for the root updater: Request calls onRequest
// instead of installing anything.
type fakeInstaller struct {
	dir       string
	mu        sync.Mutex
	requested []string
	onRequest func(commit string) error
}

func (f *fakeInstaller) Dir() string                         { return f.dir }
func (f *fakeInstaller) Available() error                    { return nil }
func (f *fakeInstaller) Result() (*selfupdate.Result, error) { return selfupdate.ReadResult(f.dir) }

// verdict writes the updater's result.json, as apply-update does once the
// restarted service has stayed up.
func (f *fakeInstaller) verdict(t *testing.T, commit string, ok bool) {
	b, _ := json.Marshal(selfupdate.Result{Commit: commit, OK: ok})
	if err := os.WriteFile(filepath.Join(f.dir, "result.json"), b, 0o644); err != nil {
		t.Error(err)
	}
}
func (f *fakeInstaller) Request(commit string) error {
	f.mu.Lock()
	f.requested = append(f.requested, commit)
	fn := f.onRequest
	f.mu.Unlock()
	if fn != nil {
		return fn(commit)
	}
	return nil
}
func (f *fakeInstaller) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requested...)
}

// bundleFor builds a minimal update bundle for commit.
func bundleFor(t *testing.T, commit string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"aicluster/manifest.json":     `{"commit":"` + commit + `","version":"test"}`,
		"aicluster/install-target.sh": "#!/bin/bash\n",
		"aicluster/apply-update":      "#!/bin/bash\n",
	} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func withInstaller(f *fakeInstaller) slaveOpt { return func(c *agent.Config) { c.Installer = f } }

// slaveWithUpdater starts a slave whose "updater" restarts it on the new
// commit, or fails when fail is set.
func slaveWithUpdater(t *testing.T, c *ctrl, fail bool) (*slave, *fakeInstaller) {
	inst := &fakeInstaller{dir: t.TempDir()}
	s := startSlave(t, c, withInstaller(inst), func(cfg *agent.Config) { cfg.BuildCommit = oldCommit })
	inst.onRequest = func(commit string) error {
		if fail {
			return errors.New("disk full")
		}
		go func() {
			time.Sleep(200 * time.Millisecond)
			s.SetBuildCommit(commit)
			// Running the new build, but not yet past the updater's check.
			time.Sleep(2 * time.Second)
			inst.verdict(t, commit, true)
		}()
		return nil
	}
	return s, inst
}

func waitRollout(t *testing.T, c *ctrl, phase string) *core.Rollout {
	t.Helper()
	var r *core.Rollout
	waitFor(t, 60*time.Second, "rollout "+phase, func() bool {
		r = c.Rollout()
		return r != nil && r.Phase == phase
	})
	return r
}

// The whole flow: check, apply from the dashboard API, the Controller
// installs itself and restarts, then each node is updated in turn.
func TestRollingUpdate(t *testing.T) {
	bundle := bundleFor(t, newCommit)
	sum := sha256.Sum256(bundle)
	var gh *httptest.Server
	gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": newCommit, "commit": map[string]any{"message": "New things"}})
		case "/repos/o/r/compare/" + oldCommit + "..." + newCommit:
			_ = json.NewEncoder(w).Encode(map[string]any{"ahead_by": 1, "commits": []any{}})
		case "/repos/o/r/releases/tags/update-main":
			_ = json.NewEncoder(w).Encode(map[string]any{"assets": []any{
				map[string]any{"name": selfupdate.BundleName, "browser_download_url": gh.URL + "/dl/bundle", "digest": "sha256:" + hex.EncodeToString(sum[:]), "size": len(bundle)},
				map[string]any{"name": selfupdate.ManifestName, "browser_download_url": gh.URL + "/dl/manifest", "digest": "sha256:x"},
			}})
		case "/dl/bundle":
			_, _ = w.Write(bundle)
		case "/dl/manifest":
			_, _ = w.Write([]byte(`{"commit":"` + newCommit + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()

	ctrlInst := &fakeInstaller{dir: t.TempDir()}
	c := startController(t, core.ModeAuto, func(cfg *core.Config) {
		cfg.BuildCommit, cfg.Installer = oldCommit, ctrlInst
	})
	s1, _ := slaveWithUpdater(t, c, false)
	s2, _ := slaveWithUpdater(t, c, false)
	s3 := startSlave(t, c) // can't update itself
	adoptAll(t, c, s1, s2, s3)
	waitFor(t, 20*time.Second, "agents to report their commits", func() bool {
		return strings.Contains(string(c.node(t, s1.NodeID()).Observed), oldCommit) &&
			strings.Contains(string(c.node(t, s2.NodeID()).Observed), oldCommit)
	})

	ctx := context.Background()
	_ = c.Store.SetSetting(ctx, "update_repo", "o/r")
	checker := updates.New(updates.Build{Version: "test", Commit: oldCommit}, web.UpdateSettings(c.Store), logger("updates"))
	checker.API = gh.URL
	srv := httptest.NewServer(web.New(c.Core, logger("web")).WithUpdates(checker).Handler())
	defer srv.Close()
	call := func(method, path string, in, out any) error {
		return httpx.Do(ctx, srv.Client(), method, srv.URL+path, in, out, "Authorization", "Bearer "+c.AdminToken)
	}
	var v web.UpdateView
	if err := call("POST", "/api/v1/updates/check", nil, &v); err != nil || !v.Available || !v.CanApply {
		t.Fatalf("check: %v %+v", err, v)
	}
	if err := call("POST", "/api/v1/updates/apply", map[string]string{"commit": "stale000000"}, nil); err == nil {
		t.Fatal("applied a commit that isn't the latest")
	}
	if err := call("POST", "/api/v1/updates/apply", map[string]string{"commit": newCommit}, &v); err != nil {
		t.Fatal(err)
	}

	// The Controller hands the verified bundle to its updater...
	waitRollout(t, c, core.RolloutController)
	if got := ctrlInst.calls(); len(got) != 1 || got[0] != newCommit {
		t.Fatalf("controller updater calls: %v", got)
	}
	if err := call("POST", "/api/v1/updates/apply", map[string]string{"commit": newCommit}, nil); err == nil {
		t.Fatal("a second update started while one is running")
	}

	// ...which restarts it on the new build; it then updates the nodes.
	ctrlInst.verdict(t, newCommit, true)
	old := c.cfg
	c.stop()
	c = startController(t, core.ModeAuto, func(cfg *core.Config) {
		*cfg = old
		cfg.BuildCommit = newCommit
	})
	r := waitRollout(t, c, core.RolloutDone)
	states := map[string]string{}
	for _, n := range r.Nodes {
		states[n.NodeID] = n.State + ":" + n.Detail
	}
	if !strings.HasPrefix(states[s1.NodeID()], core.NodeUpdated) || !strings.HasPrefix(states[s2.NodeID()], core.NodeUpdated) ||
		!strings.HasPrefix(states[s3.NodeID()], core.NodeSkipped) || len(states) != 3 {
		t.Fatalf("node states: %v", states)
	}
	if s1.BuildCommit() != newCommit || s2.BuildCommit() != newCommit {
		t.Fatalf("agents on %s / %s", s1.BuildCommit(), s2.BuildCommit())
	}
	// The update is no longer in anyone's desired state.
	for _, s := range []*slave{s1, s2} {
		waitFor(t, 10*time.Second, "update cleared from desired state", func() bool {
			return !strings.Contains(string(c.node(t, s.NodeID()).Desired), `"update"`)
		})
	}
}

// A node that fails stops the rollout; the rest are left alone.
func TestRollingUpdateStopsAtFirstFailure(t *testing.T) {
	ctrlInst := &fakeInstaller{dir: t.TempDir()}
	c := startController(t, core.ModeAuto, func(cfg *core.Config) {
		cfg.BuildCommit, cfg.Installer = newCommit, ctrlInst // already on the new build
	})
	s1, _ := slaveWithUpdater(t, c, true)
	s2, _ := slaveWithUpdater(t, c, true)
	adoptAll(t, c, s1, s2)
	waitFor(t, 20*time.Second, "agents to report their commits", func() bool {
		return strings.Contains(string(c.node(t, s1.NodeID()).Observed), oldCommit) &&
			strings.Contains(string(c.node(t, s2.NodeID()).Observed), oldCommit)
	})

	bundle := bundleFor(t, newCommit)
	sum := sha256.Sum256(bundle)
	fetch := func(_ context.Context, _ string, dir string) (string, error) {
		return selfupdate.Save(dir, bytes.NewReader(bundle), hex.EncodeToString(sum[:]))
	}
	if err := c.StartUpdate(newCommit, fetch); err != nil {
		t.Fatal(err)
	}
	r := waitRollout(t, c, core.RolloutFailed)
	if len(ctrlInst.calls()) != 0 {
		t.Fatal("the Controller reinstalled the build it already runs")
	}
	var failed, waiting int
	for _, n := range r.Nodes {
		switch n.State {
		case core.NodeFailed:
			failed++
			if !strings.Contains(n.Detail, "disk full") {
				t.Errorf("failure detail: %q", n.Detail)
			}
		case core.NodeWaiting:
			waiting++
		}
	}
	if failed != 1 || waiting != 1 || !strings.Contains(r.Error, "disk full") {
		t.Fatalf("rollout: %+v", r)
	}
}
