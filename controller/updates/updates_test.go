package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves /repos/o/r/commits/main and /repos/o/r/compare/a...b for
// a linear history c1 <- c2 <- c3 (c3 = main).
type fakeGitHub struct {
	mu        sync.Mutex
	history   []string
	hits      map[string]int
	rateLimit bool
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[r.URL.Path]++
	if f.rateLimit {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(30*time.Minute).Unix()))
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Header.Get("User-Agent") == "" {
		w.WriteHeader(http.StatusForbidden) // GitHub requires a User-Agent
		return
	}
	head := f.history[len(f.history)-1]
	commit := func(i int) map[string]any {
		return map[string]any{
			"sha": f.history[i], "html_url": "https://github.com/o/r/commit/" + f.history[i],
			"commit": map[string]any{
				"message":   fmt.Sprintf("Change %d\n\nDetails", i+1),
				"author":    map[string]any{"name": "dev", "date": time.Date(2026, 10, i+1, 0, 0, 0, 0, time.UTC)},
				"committer": map[string]any{"date": time.Date(2026, 10, i+1, 0, 0, 0, 0, time.UTC)},
			},
		}
	}
	switch {
	case r.URL.Path == "/repos/o/r/commits/main":
		etag := `"` + head + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_ = json.NewEncoder(w).Encode(commit(len(f.history) - 1))
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/compare/"):
		base, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/o/r/compare/"), "...")
		idx := -1
		for i, s := range f.history {
			if s == base {
				idx = i
			}
		}
		if idx < 0 {
			http.NotFound(w, r)
			return
		}
		var cs []any
		for i := idx + 1; i < len(f.history); i++ {
			cs = append(cs, commit(i))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "ahead_by": len(cs), "commits": cs,
			"html_url": "https://github.com/o/r/compare/" + base + "..." + head})
	default:
		http.NotFound(w, r)
	}
}

func newChecker(t *testing.T, gh *fakeGitHub, commit string, set Settings) *Checker {
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	c := New(Build{Commit: commit, Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)},
		func(context.Context) Settings { return set }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.API = srv.URL
	return c
}

func gh() *fakeGitHub {
	return &fakeGitHub{history: []string{"c1aaaaaaaaaa", "c2bbbbbbbbbb", "c3cccccccccc"}, hits: map[string]int{}}
}

var on = Settings{Enabled: true, Repo: "o/r", Branch: "main"}

func TestBehind(t *testing.T) {
	c := newChecker(t, gh(), "c1aaaaaaaaaa", on)
	s := c.Check(context.Background())
	if !s.Available || s.Behind != 2 || len(s.Commits) != 2 || s.Error != "" {
		t.Fatalf("status = %+v", s)
	}
	if s.Commits[0].Message != "Change 3" || s.Latest.SHA != "c3cccccccccc" || !strings.Contains(s.CompareURL, "compare") {
		t.Fatalf("newest first, first line only: %+v", s.Commits)
	}
}

func TestUpToDateAndShortSHA(t *testing.T) {
	c := newChecker(t, gh(), "c3cccccccccc", on)
	if s := c.Check(context.Background()); s.Available || s.Behind != 0 || s.Error != "" {
		t.Fatalf("status = %+v", s)
	}
	c.Current.Commit = "c3ccccc" // short form still matches
	if s := c.Check(context.Background()); s.Available {
		t.Fatalf("short sha: %+v", s)
	}
}

func TestLocalBuildComparesDates(t *testing.T) {
	c := newChecker(t, gh(), "localonly123", on)
	s := c.Check(context.Background())
	if s.Behind != -1 || !s.Available || !strings.Contains(s.Note, "local build") {
		t.Fatalf("status = %+v", s) // latest (Oct 3) is newer than the build (Oct 1)
	}
}

func TestNoCommitInfo(t *testing.T) {
	c := newChecker(t, gh(), "", on)
	if s := c.Check(context.Background()); s.Available || s.Behind != -1 || s.Latest == nil {
		t.Fatalf("status = %+v", s)
	}
}

func TestDisabledAndOfflineMakeNoRequests(t *testing.T) {
	g := gh()
	c := newChecker(t, g, "c1aaaaaaaaaa", Settings{Enabled: false})
	if s := c.Check(context.Background()); s.Enabled || s.Available {
		t.Fatalf("disabled: %+v", s)
	}
	c.Settings = func(context.Context) Settings { return on }
	c.Offline = func() bool { return true }
	c.Check(context.Background())
	if len(g.hits) != 0 {
		t.Fatalf("contacted GitHub: %v", g.hits)
	}
}

func TestETagSavesRateLimit(t *testing.T) {
	g := gh()
	c := newChecker(t, g, "c1aaaaaaaaaa", on)
	c.Check(context.Background())
	s := c.Check(context.Background()) // 304: keeps the previous result, no compare call
	if !s.Available || s.Behind != 2 {
		t.Fatalf("after 304: %+v", s)
	}
	if g.hits["/repos/o/r/compare/c1aaaaaaaaaa...c3cccccccccc"] != 1 {
		t.Fatalf("compare re-fetched: %v", g.hits)
	}
}

func TestRateLimitBacksOff(t *testing.T) {
	g := gh()
	c := newChecker(t, g, "c1aaaaaaaaaa", on)
	c.Check(context.Background())
	g.rateLimit = true
	s := c.Check(context.Background())
	if !strings.Contains(s.Error, "rate limit") || !s.Available {
		t.Fatalf("rate limited: %+v (should keep the last good result)", s)
	}
	before := g.hits["/repos/o/r/commits/main"]
	c.Check(context.Background())
	if g.hits["/repos/o/r/commits/main"] != before {
		t.Fatal("kept calling GitHub while rate limited")
	}
}

func TestUnknownRepo(t *testing.T) {
	c := newChecker(t, gh(), "c1aaaaaaaaaa", Settings{Enabled: true, Repo: "nobody/nothing", Branch: "main"})
	if s := c.Check(context.Background()); !strings.Contains(s.Error, "not found") {
		t.Fatalf("status = %+v", s)
	}
}
