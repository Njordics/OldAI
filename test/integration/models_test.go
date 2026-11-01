package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/controller/provision"
	"aicluster/controller/store"
	"aicluster/controller/web"
	"aicluster/slave-agent/runtime"
)

const modelTimeout = 20 * time.Second

func (c *ctrl) waitModel(t *testing.T, assignmentID, state string) core.ModelStatus {
	t.Helper()
	var got core.ModelStatus
	waitFor(t, modelTimeout, "assignment "+assignmentID[:8]+" to be "+state, func() bool {
		ms, err := c.ModelStatuses(context.Background())
		if err != nil {
			return false
		}
		for _, m := range ms {
			if m.ID == assignmentID {
				got = m
				return m.State == state
			}
		}
		return false
	})
	return got
}

func fake(s *slave) *runtime.Fake { return s.Runtime().(*runtime.Fake) }

func installed(t *testing.T, s *slave, name string) bool {
	t.Helper()
	m, err := fake(s).Installed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m[name]
}

// The whole path: catalog -> fit -> Controller downloads once -> nodes pull
// over the LAN through their registry shim -> loaded -> swap -> remove.
func TestInstallSwapRemove(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s1, s2 := startSlave(t, c), startSlave(t, c)
	c.waitState(t, s1.NodeID(), store.StateAdopted, adoptTimeout)
	c.waitState(t, s2.NodeID(), store.StateAdopted, adoptTimeout)
	ctx := context.Background()

	// The fake registry is shared across tests: count deltas, not totals.
	manifest := "/v2/library/qwen2.5/manifests/0.5b-instruct-q8_0"
	fetches := func() int {
		return registry.Count(manifest) + registry.Count("/v2/library/qwen2.5/manifests/0.5b-instruct-q4_K_M")
	}
	before := fetches()

	// Explicit node.
	as, err := c.Install(ctx, core.InstallRequest{ModelID: "qwen2.5:0.5b", Nodes: []string{s1.NodeID()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].NodeID != s1.NodeID() || as[0].Quant == "" || as[0].Placement == "" {
		t.Fatalf("assignment = %+v", as)
	}
	m := c.waitModel(t, as[0].ID, proto.ModelLoaded)
	local := "qwen2.5:" + m.Ref[len("library/qwen2.5:"):]
	if !fake(s1).Loaded(local) {
		t.Fatalf("%s not loaded in s1's runtime", local)
	}
	if n := fetches() - before; n != 1 {
		t.Fatalf("internet fetches = %d, want 1", n)
	}

	// Same model again, automatic placement: must pick the other node, and
	// come from the Controller's cache, not the internet.
	as2, err := c.Install(ctx, core.InstallRequest{ModelID: "qwen2.5:0.5b"})
	if err != nil {
		t.Fatal(err)
	}
	if as2[0].NodeID != s2.NodeID() {
		t.Fatalf("auto placement chose %s, want the node without the model", as2[0].NodeID[:8])
	}
	c.waitModel(t, as2[0].ID, proto.ModelLoaded)
	if n := fetches() - before; n != 1 {
		t.Fatalf("second node caused an internet fetch (%d)", n)
	}

	// Installing it on a node that already has it is refused.
	if _, err := c.Install(ctx, core.InstallRequest{ModelID: "qwen2.5:0.5b", Nodes: []string{s1.NodeID()}}); !errors.Is(err, core.ErrAlreadyThere) {
		t.Fatalf("duplicate install: %v", err)
	}
	if _, err := c.Install(ctx, core.InstallRequest{ModelID: "nope:1b"}); !errors.Is(err, core.ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}

	// Swap on s1: new model loads, old one is unloaded but kept on disk.
	sw, err := c.Swap(ctx, as[0].ID, "all-minilm:22m", "")
	if err != nil {
		t.Fatal(err)
	}
	c.waitModel(t, sw.ID, proto.ModelLoaded)
	waitFor(t, 5*time.Second, "old model unloaded", func() bool { return !fake(s1).Loaded(local) })
	if !installed(t, s1, local) {
		t.Fatal("swapped-out model deleted before the grace period")
	}
	if !fake(s1).Loaded("all-minilm:22m") {
		t.Fatal("swapped-in model not loaded")
	}

	// Remove from s2.
	if err := c.RemoveAssignment(ctx, as2[0].ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "s2 model unloaded", func() bool { return !fake(s2).Loaded(local) })
	ms, _ := c.ModelStatuses(ctx)
	if len(ms) != 1 || ms[0].ID != sw.ID {
		t.Fatalf("remaining assignments: %+v", ms)
	}
}

// After the grace period an unassigned model is deleted from the node's disk.
func TestUnassignedModelDeletedAfterGrace(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto, func(cfg *core.Config) { cfg.ModelGCGrace = time.Second })
	s := startSlave(t, c)
	c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	ctx := context.Background()
	as, err := c.Install(ctx, core.InstallRequest{ModelID: "all-minilm:22m", Nodes: []string{s.NodeID()}, Residency: proto.ResidencyOnDemand})
	if err != nil {
		t.Fatal(err)
	}
	c.waitModel(t, as[0].ID, proto.ModelInstalled) // on-demand: on disk, not loaded
	if fake(s).Loaded("all-minilm:22m") {
		t.Fatal("on-demand model was loaded")
	}
	if err := c.RemoveAssignment(ctx, as[0].ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "model deleted from disk", func() bool { return !installed(t, s, "all-minilm:22m") })
}

// A node that reboots keeps its models and reloads the resident ones.
func TestModelsSurviveAgentRestart(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	as, err := c.Install(context.Background(), core.InstallRequest{ModelID: "all-minilm:22m", Nodes: []string{s.NodeID()}})
	if err != nil {
		t.Fatal(err)
	}
	c.waitModel(t, as[0].ID, proto.ModelLoaded)
	manifest := "/v2/library/all-minilm/manifests/22m"
	fetches := registry.Count(manifest)
	s.restart(t)
	waitFor(t, 5*time.Second, "reloaded after restart", func() bool { return fake(s).Loaded("all-minilm:22m") })
	c.waitModel(t, as[0].ID, proto.ModelLoaded)
	if registry.Count(manifest) != fetches {
		t.Fatal("restart re-downloaded the model")
	}
}

// A model the registry doesn't have fails visibly instead of hanging.
func TestControllerDownloadFailureVisible(t *testing.T) {
	requireProvisioner(t)
	registry.Missing["library/mistral:7b-instruct-q4_K_M"] = true
	registry.Missing["library/mistral:7b-instruct-q8_0"] = true
	defer delete(registry.Missing, "library/mistral:7b-instruct-q4_K_M")
	defer delete(registry.Missing, "library/mistral:7b-instruct-q8_0")
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	as, err := c.Install(context.Background(), core.InstallRequest{ModelID: "mistral:7b", Nodes: []string{s.NodeID()}, Quant: "q4_K_M"})
	if err != nil {
		var fe *core.FitError
		if errors.As(err, &fe) {
			t.Skipf("this machine is too small for mistral:7b: %v", err)
		}
		t.Fatal(err)
	}
	m := c.waitModel(t, as[0].ID, proto.ModelFailed)
	if m.Stage != "controller" || m.Error == "" {
		t.Fatalf("failure not attributed to the Controller download: %+v", m)
	}
}

// The dashboard's model endpoints, end to end over HTTP.
func TestModelAPI(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	srv := httptest.NewServer(web.New(c.Core, logger("web")).Handler())
	defer srv.Close()
	ctx := context.Background()
	auth := []string{"Authorization", "Bearer " + c.AdminToken}
	call := func(method, path string, in, out any) error {
		return httpx.Do(ctx, srv.Client(), method, srv.URL+path, in, out, auth...)
	}

	var cat web.CatalogView
	if err := call("GET", "/api/v1/catalog", nil, &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Families) == 0 || !cat.Fit["all-minilm:22m"][s.NodeID()].Fits {
		t.Fatalf("catalog fit missing for the node: %+v", cat.Fit["all-minilm:22m"])
	}

	var as []store.Assignment
	if err := call("POST", "/api/v1/models/install", core.InstallRequest{ModelID: "all-minilm:22m"}, &as); err != nil || len(as) != 1 {
		t.Fatalf("install: %v %+v", err, as)
	}
	c.waitModel(t, as[0].ID, proto.ModelLoaded)

	var opts []provision.FitResult
	if err := call("GET", "/api/v1/assignments/"+as[0].ID+"/swap-options", nil, &opts); err != nil || len(opts) == 0 {
		t.Fatalf("swap options: %v (%d)", err, len(opts))
	}
	var sw store.Assignment
	if err := call("POST", "/api/v1/assignments/"+as[0].ID+"/swap", web.SwapRequest{ModelID: "nomic-embed-text:v1.5"}, &sw); err != nil {
		t.Fatal(err)
	}
	c.waitModel(t, sw.ID, proto.ModelLoaded)

	err := call("POST", "/api/v1/models/install", core.InstallRequest{ModelID: "nomic-embed-text:v1.5", Nodes: []string{s.NodeID()}}, nil)
	if ae, ok := err.(*httpx.APIError); !ok || ae.Status != 409 {
		t.Fatalf("duplicate install over HTTP: %v", err)
	}
	if err := call("DELETE", "/api/v1/assignments/"+sw.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	var left []core.ModelStatus
	if err := call("GET", "/api/v1/models", nil, &left); err != nil || len(left) != 0 {
		t.Fatalf("models after delete: %v %+v", err, left)
	}
}

// Ollama asks a registry where a blob lives and expects a 307 with a
// Location (registry.ollama.ai redirects to its CDN), then downloads that
// location in ranges. The shim must answer the same way (found with real
// Ollama on VMs: "http: no Location header in response").
func TestShimRedirectsBlobsLikeARegistry(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	as, err := c.Install(context.Background(), core.InstallRequest{ModelID: "all-minilm:22m", Nodes: []string{s.NodeID()}})
	if err != nil {
		t.Fatal(err)
	}
	c.waitModel(t, as[0].ID, proto.ModelLoaded)

	base := "http://" + s.ShimHost()
	resp, err := http.Get(base + "/v2/library/all-minilm/manifests/22m")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Layers []struct{ Digest string } `json:"layers"`
	}
	err = json.NewDecoder(resp.Body).Decode(&manifest)
	resp.Body.Close()
	if err != nil || len(manifest.Layers) == 0 {
		t.Fatalf("manifest: %v %+v", err, manifest)
	}

	// Ollama's own rule: follow redirects within a host name, stop at the first
	// one to another host and use its Location.
	blobURL := base + "/v2/library/all-minilm/blobs/" + manifest.Layers[0].Digest
	asOllama := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Hostname() == via[0].URL.Hostname() {
			return nil
		}
		return http.ErrUseLastResponse
	}}
	resp, err = asOllama.Get(blobURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := resp.Location()
	if resp.StatusCode != http.StatusTemporaryRedirect || err != nil {
		t.Fatalf("blob GET = %d, location %v", resp.StatusCode, err)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, _ := http.NewRequest(http.MethodGet, loc.String(), nil)
	req.Header.Set("Range", "bytes=0-9")
	resp, err = noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || len(body) != 10 {
		t.Fatalf("ranged download of %s = %d with %d bytes", loc, resp.StatusCode, len(body))
	}
}
