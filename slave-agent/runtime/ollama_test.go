package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aicluster/common/registry"
)

// fakeOllama implements the slice of Ollama's API the driver uses.
type fakeOllama struct {
	mu        sync.Mutex
	models    map[string]bool
	keepAlive map[string]any
	pulled    []map[string]any
	failPull  string
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/api/version":
		fmt.Fprint(w, `{"version":"0.9.0"}`)
	case "/api/tags":
		var ms []map[string]string
		for n := range f.models {
			ms = append(ms, map[string]string{"name": n})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": ms})
	case "/api/pull":
		f.pulled = append(f.pulled, body)
		if f.failPull != "" {
			fmt.Fprintf(w, "{\"status\":\"pulling manifest\"}\n{\"error\":%q}\n", f.failPull)
			return
		}
		for _, line := range []string{
			`{"status":"pulling manifest"}`,
			`{"status":"pulling a","digest":"sha256:a","total":100,"completed":40}`,
			`{"status":"pulling b","digest":"sha256:b","total":50,"completed":50}`,
			`{"status":"pulling a","digest":"sha256:a","total":100,"completed":100}`,
			`{"status":"success"}`,
		} {
			fmt.Fprintln(w, line)
		}
		f.models[body["model"].(string)] = true
	case "/api/copy":
		f.models[body["destination"].(string)] = true
	case "/api/delete":
		name := body["model"].(string)
		if !f.models[name] {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"model not found"}`)
			return
		}
		delete(f.models, name)
	case "/api/generate":
		f.keepAlive[body["model"].(string)] = body["keep_alive"]
		fmt.Fprint(w, `{"done":true}`)
	default:
		http.NotFound(w, r)
	}
}

func TestOllamaDriver(t *testing.T) {
	fo := &fakeOllama{models: map[string]bool{}, keepAlive: map[string]any{}}
	srv := httptest.NewServer(fo)
	defer srv.Close()
	o := NewOllama(srv.URL)
	ctx := context.Background()
	if err := o.Check(ctx); err != nil {
		t.Fatal(err)
	}
	ref, _ := registry.ParseRef("library/qwen2.5:0.5b-instruct-q4_K_M")
	m := Model{Ref: ref, LocalName: ref.LocalName(), RegistryHost: "127.0.0.1:7452"}

	var last [2]int64
	if err := o.Pull(ctx, m, func(done, total int64) { last = [2]int64{done, total} }); err != nil {
		t.Fatal(err)
	}
	if last != [2]int64{150, 150} {
		t.Fatalf("progress = %v, want summed across layers", last)
	}
	if got := fo.pulled[0]; got["model"] != "127.0.0.1:7452/library/qwen2.5:0.5b-instruct-q4_K_M" || got["insecure"] != true {
		t.Fatalf("pull request = %v", got)
	}
	inst, _ := o.Installed(ctx)
	if !inst["qwen2.5:0.5b-instruct-q4_K_M"] || len(inst) != 1 {
		t.Fatalf("installed = %v (plain name only, registry-prefixed copy removed)", inst)
	}
	if err := o.Load(ctx, m); err != nil || fo.keepAlive[m.LocalName] != float64(-1) {
		t.Fatalf("load: %v keep_alive=%v", err, fo.keepAlive[m.LocalName])
	}
	if err := o.Unload(ctx, m); err != nil || fo.keepAlive[m.LocalName] != float64(0) {
		t.Fatalf("unload: %v", err)
	}
	if err := o.Remove(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := o.Remove(ctx, m); err != nil {
		t.Fatalf("removing a missing model should be a no-op: %v", err)
	}

	fo.failPull = "pull model manifest: file does not exist"
	if err := o.Pull(ctx, m, nil); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("pull error = %v", err)
	}
}

func TestOllamaDown(t *testing.T) {
	o := NewOllama("http://127.0.0.1:1")
	if err := o.Check(context.Background()); err == nil {
		t.Fatal("Check succeeded with nothing listening")
	}
}
