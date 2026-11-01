package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"aicluster/common/httpx"
	"aicluster/controller/core"
	"aicluster/controller/store"
	"aicluster/controller/web"
)

func TestWorkspaceAPI(t *testing.T) {
	c := startController(t, core.ModeAuto)
	srv := httptest.NewServer(web.New(c.Core, logger("web")).Handler())
	defer srv.Close()
	ctx := context.Background()
	auth := []string{"Authorization", "Bearer " + c.AdminToken}
	call := func(method, path string, in, out any) error {
		return httpx.Do(ctx, srv.Client(), method, srv.URL+path, in, out, auth...)
	}
	q := url.QueryEscape

	put := func(p, content string) error {
		return call("PUT", "/api/v1/workspace/file", map[string]string{"path": p, "content": content}, nil)
	}
	if err := put("src/app.js", "console.log(1)\n"); err != nil {
		t.Fatal(err)
	}
	if err := put("index.html", "<h1>hi</h1>"); err != nil {
		t.Fatal(err)
	}
	var f store.WorkspaceFile
	if err := call("GET", "/api/v1/workspace/file?path="+q("src/app.js"), nil, &f); err != nil || f.Content != "console.log(1)\n" {
		t.Fatalf("get: %v %+v", err, f)
	}
	var list []store.WorkspaceFile
	if err := call("GET", "/api/v1/workspace/files", nil, &list); err != nil || len(list) != 2 || list[1].Path != "src/app.js" || list[1].Content != "" {
		t.Fatalf("list: %v %+v", err, list)
	}

	// Paths that could escape or confuse are refused.
	for _, bad := range []string{"../etc/passwd", "/abs.txt", "a/../../b", "a\\..\\b", "", "con:x", "a/./b", "x\x00y"} {
		if err := put(bad, "x"); err == nil {
			t.Errorf("accepted path %q", bad)
		}
	}
	if err := put("big.txt", strings.Repeat("x", store.MaxWorkspaceFile+1)); err == nil {
		t.Error("accepted a file over the size limit")
	}

	// Rename, conflicts, delete.
	if err := call("POST", "/api/v1/workspace/rename", map[string]string{"from": "src/app.js", "to": "app.js"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call("POST", "/api/v1/workspace/rename", map[string]string{"from": "app.js", "to": "index.html"}, nil); err == nil {
		t.Error("rename over an existing file succeeded")
	}
	if err := call("DELETE", "/api/v1/workspace/file?path="+q("index.html"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := call("GET", "/api/v1/workspace/file?path="+q("index.html"), nil, nil); err == nil {
		t.Error("deleted file still readable")
	}

	// Zip download holds the remaining file.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/workspace/zip", nil)
	req.Header.Set("Authorization", "Bearer "+c.AdminToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(zr.File) != 1 || zr.File[0].Name != "app.js" {
		t.Fatalf("zip: %v", err)
	}

	// Workspace needs sign-in.
	if r, _ := http.Get(srv.URL + "/api/v1/workspace/files"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", r.StatusCode)
	}
}

// The preview runs user code: it must be sandboxed, offline and unable to
// reach the dashboard, while the dashboard itself keeps its strict policy.
func TestPreviewIsolation(t *testing.T) {
	c := startController(t, core.ModeAuto)
	srv := httptest.NewServer(web.New(c.Core, logger("web")).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/preview")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"sandbox allow-scripts", "connect-src 'none'", "frame-ancestors 'self'", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("preview CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "allow-same-origin") {
		t.Error("preview may share the dashboard's origin")
	}
	if !strings.Contains(string(body), "__aicPreview") {
		t.Error("preview page missing its bridge")
	}

	resp, _ = http.Get(srv.URL + "/")
	if main := resp.Header.Get("Content-Security-Policy"); strings.Contains(main, "unsafe-inline") || !strings.Contains(main, "default-src 'self'") {
		t.Errorf("dashboard CSP weakened: %s", main)
	}
	for _, asset := range []string{"/ui/codekit.js", "/ui/code.js"} {
		if r, _ := http.Get(srv.URL + asset); r.StatusCode != 200 {
			t.Errorf("%s: %d", asset, r.StatusCode)
		}
	}
	// The preview relies on its CSP sandbox, not the iframe attribute, so
	// user code must not be able to navigate its frame to any other page of
	// the Controller: every other response forbids framing.
	for _, p := range []string{"/", "/ui/app.js", "/ui/preview.html", "/ca.crt", "/trust-info", "/api/v1/nodes", "/session", "/nope"} {
		r, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s can be framed: %q", p, csp)
		}
	}
}
