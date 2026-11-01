package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aicluster/common/httpx"
	"aicluster/controller/core"
	"aicluster/controller/updates"
	"aicluster/controller/web"
)

// githubStub: main is two commits ahead of "base1234".
func githubStub() *httptest.Server {
	commit := func(sha, msg string, day int) map[string]any {
		return map[string]any{"sha": sha, "html_url": "https://github.com/o/r/commit/" + sha,
			"commit": map[string]any{"message": msg, "author": map[string]any{"name": "dev", "date": time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC)},
				"committer": map[string]any{"date": time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC)}}}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/commits/main":
			_ = json.NewEncoder(w).Encode(commit("head5678", "Add update pop-up", 3))
		case r.URL.Path == "/repos/o/r/compare/base1234...head5678":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "ahead_by": 2, "html_url": "https://github.com/o/r/compare/x",
				"commits": []any{commit("mid0000", "Fix things", 2), commit("head5678", "Add update pop-up", 3)}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestUpdateNotifications(t *testing.T) {
	c := startController(t, core.ModeAuto)
	gh := githubStub()
	defer gh.Close()
	ctx := context.Background()
	_ = c.Store.SetSetting(ctx, "update_repo", "o/r")

	checker := updates.New(updates.Build{Version: "test", Commit: "base1234"}, web.UpdateSettings(c.Store), logger("updates"))
	checker.API = gh.URL
	srv := httptest.NewServer(web.New(c.Core, logger("web")).WithUpdates(checker).Handler())
	defer srv.Close()
	auth := []string{"Authorization", "Bearer " + c.AdminToken}
	call := func(method, path string, in, out any) error {
		return httpx.Do(ctx, srv.Client(), method, srv.URL+path, in, out, auth...)
	}

	var v web.UpdateView
	if err := call("POST", "/api/v1/updates/check", nil, &v); err != nil {
		t.Fatal(err)
	}
	if !v.Available || v.Behind != 2 || len(v.Commits) != 2 || v.Commits[0].Message != "Add update pop-up" || v.Dismissed != "" {
		t.Fatalf("check: %+v", v)
	}

	// Skip this version.
	if err := call("POST", "/api/v1/updates/dismiss", map[string]string{"commit": "head5678"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call("GET", "/api/v1/updates", nil, &v); err != nil || v.Dismissed != "head5678" || !v.Available {
		t.Fatalf("after dismiss: %v %+v", err, v)
	}

	// Settings round-trip and validation.
	var s web.Settings
	if err := call("GET", "/api/v1/settings", nil, &s); err != nil || s.UpdateCheck == nil || !*s.UpdateCheck || s.UpdateRepo != "o/r" || s.UpdateBranch != "main" {
		t.Fatalf("settings: %v %+v", err, s)
	}
	off := false
	if err := call("PUT", "/api/v1/settings", web.Settings{UpdateCheck: &off}, &s); err != nil || *s.UpdateCheck {
		t.Fatalf("turn off: %v %+v", err, s)
	}
	if err := call("POST", "/api/v1/updates/check", nil, &v); err != nil || v.Enabled || v.Available {
		t.Fatalf("disabled check: %v %+v", err, v)
	}
	for _, bad := range []web.Settings{{UpdateRepo: "not a repo"}, {UpdateRepo: "../x"}, {UpdateRepo: "o/.."}, {UpdateRepo: "o/r/x"}, {UpdateBranch: "a b"}, {UpdateBranch: "a/../b"}, {UpdateBranch: "-x"}} {
		if err := call("PUT", "/api/v1/settings", bad, nil); err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("accepted %+v: %v", bad, err)
		}
	}
}
