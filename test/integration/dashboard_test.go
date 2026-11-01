package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aicluster/controller/core"
	"aicluster/controller/web"
)

func TestDashboardSession(t *testing.T) {
	c := startController(t, core.ModeAuto)
	srv := httptest.NewTLSServer(web.New(c.Core, logger("web")).Handler())
	defer srv.Close()
	cl := srv.Client()
	ctx := context.Background()

	req := func(method, path, body string, hdr ...string) *http.Response {
		t.Helper()
		r, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, strings.NewReader(body))
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := cl.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// The page and its assets load without auth, with a strict CSP.
	resp := req("GET", "/", "")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), `id="login-form"`) {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	for _, asset := range []string{"/ui/app.js", "/ui/app.css", "/ui/models.js"} {
		if r := req("GET", asset, ""); r.StatusCode != 200 {
			t.Fatalf("%s: %d", asset, r.StatusCode)
		}
	}

	// The CA is downloadable without signing in, so the warning can be fixed.
	resp = req("GET", "/ca.crt", "")
	caPEM, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(caPEM) != string(c.CA.CertPEM) {
		t.Fatalf("ca.crt: %d", resp.StatusCode)
	}
	if r := req("GET", "/trust-info", ""); r.StatusCode != 200 {
		t.Fatalf("trust-info: %d", r.StatusCode)
	}

	// Wrong token: no cookie.
	if r := req("POST", "/session", `{"token":"nope"}`); r.StatusCode != 401 || len(r.Cookies()) != 0 {
		t.Fatalf("bad login: %d %v", r.StatusCode, r.Cookies())
	}

	// Right token: HttpOnly, Secure, SameSite=Strict cookie.
	r := req("POST", "/session", `{"token":"`+c.AdminToken+`"}`)
	if r.StatusCode != 200 || len(r.Cookies()) != 1 {
		t.Fatalf("login: %d", r.StatusCode)
	}
	ck := r.Cookies()[0]
	if !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || strings.Contains(ck.Value, c.AdminToken) {
		t.Fatalf("cookie attributes: %+v", ck)
	}
	cookie := ck.Name + "=" + ck.Value

	if r := req("GET", "/api/v1/nodes", "", "Cookie", cookie); r.StatusCode != 200 {
		t.Fatalf("GET with cookie: %d", r.StatusCode)
	}
	// Writes by cookie need the anti-CSRF header.
	if r := req("PUT", "/api/v1/settings", `{"adoption_mode":"approve"}`, "Cookie", cookie); r.StatusCode != 403 {
		t.Fatalf("PUT without CSRF header: %d", r.StatusCode)
	}
	if r := req("PUT", "/api/v1/settings", `{"adoption_mode":"approve"}`, "Cookie", cookie, "X-Requested-With", "aicluster"); r.StatusCode != 200 {
		t.Fatalf("PUT with CSRF header: %d", r.StatusCode)
	}
	if c.Mode() != core.ModeApprove {
		t.Fatal("setting not applied")
	}
	// A forged or tampered cookie is rejected.
	if r := req("GET", "/api/v1/nodes", "", "Cookie", ck.Name+"=9999999999.deadbeef"); r.StatusCode != 401 {
		t.Fatalf("forged cookie: %d", r.StatusCode)
	}
	// The CLI's bearer token keeps working without the CSRF header.
	if r := req("PUT", "/api/v1/settings", `{"adoption_mode":"auto"}`, "Authorization", "Bearer "+c.AdminToken); r.StatusCode != 200 {
		t.Fatalf("bearer PUT: %d", r.StatusCode)
	}
}
