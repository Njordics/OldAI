// Package updates checks the project's GitHub repository for commits newer
// than the one this Controller was built from. It only reads public data
// (one anonymous request every few hours, sending nothing about the cluster),
// can be turned off in Settings, and stays quiet in offline mode.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultRepo     = "Njordics/OldAI"
	DefaultBranch   = "main"
	DefaultAPI      = "https://api.github.com"
	DefaultInterval = 6 * time.Hour
	maxCommits      = 15
)

// Build identifies the code this binary was built from. Go records the git
// commit automatically when building inside a checkout.
type Build struct {
	Version  string    `json:"version"`
	Commit   string    `json:"commit"`
	Time     time.Time `json:"time,omitempty"`
	Modified bool      `json:"modified"` // built with uncommitted changes
}

// CurrentBuild reads the VCS stamp from the running binary.
func CurrentBuild(version string) Build {
	b := Build{Version: version}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Commit = s.Value
			case "vcs.time":
				b.Time, _ = time.Parse(time.RFC3339, s.Value)
			case "vcs.modified":
				b.Modified = s.Value == "true"
			}
		}
	}
	return b
}

type Commit struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"` // first line
	Author  string    `json:"author"`
	Time    time.Time `json:"time"`
	URL     string    `json:"url"`
}

// Status is what the dashboard shows.
type Status struct {
	Enabled    bool      `json:"enabled"`
	Repo       string    `json:"repo"`
	Branch     string    `json:"branch"`
	Current    Build     `json:"current"`
	Latest     *Commit   `json:"latest,omitempty"`
	Available  bool      `json:"available"`
	Behind     int       `json:"behind"`  // commits on the branch since this build; -1 = unknown
	Commits    []Commit  `json:"commits"` // newest first, at most maxCommits
	CompareURL string    `json:"compare_url,omitempty"`
	CheckedAt  time.Time `json:"checked_at,omitempty"`
	Error      string    `json:"error,omitempty"`
	Note       string    `json:"note,omitempty"`
}

// Settings come from the Controller's settings table.
type Settings struct {
	Enabled bool
	Repo    string
	Branch  string
}

type Checker struct {
	API      string
	Interval time.Duration
	Current  Build
	Client   *http.Client
	Log      *slog.Logger
	// Settings and Offline are read before every check, so changes apply
	// without a restart.
	Settings func(ctx context.Context) Settings
	Offline  func() bool

	mu        sync.Mutex
	status    Status
	etag      string // of the last branch-head response
	etagFor   string // repo@branch the etag belongs to
	nextAfter time.Time
	wake      chan struct{}
}

func New(current Build, settings func(context.Context) Settings, log *slog.Logger) *Checker {
	return &Checker{
		API: DefaultAPI, Interval: DefaultInterval, Current: current, Log: log, Settings: settings,
		Client:  &http.Client{Timeout: 20 * time.Second},
		Offline: func() bool { return false },
		wake:    make(chan struct{}, 1),
	}
}

func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Current = c.Current
	s.Commits = append([]Commit(nil), s.Commits...)
	return s
}

// Run checks shortly after start and then every Interval until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.wake:
		}
		_ = c.Check(ctx)
		timer.Reset(c.Interval)
	}
}

// Check runs one check now and returns the new status.
func (c *Checker) Check(ctx context.Context) Status {
	set := c.Settings(ctx)
	if set.Repo == "" {
		set.Repo = DefaultRepo
	}
	if set.Branch == "" {
		set.Branch = DefaultBranch
	}
	st := Status{Enabled: set.Enabled, Repo: set.Repo, Branch: set.Branch, Behind: 0, Commits: []Commit{}}
	switch {
	case !set.Enabled:
		st.Note = "Update checks are turned off."
		return c.store(st)
	case c.Offline():
		st.Note = "Offline mode: not contacting GitHub."
		return c.store(st)
	}

	c.mu.Lock()
	wait := time.Until(c.nextAfter)
	prev := c.status
	c.mu.Unlock()
	if wait > 0 {
		prev.Error = fmt.Sprintf("GitHub rate limit reached; next check after %s.", c.nextAfter.Format(time.Kitchen))
		return c.store(prev)
	}

	latest, notModified, err := c.branchHead(ctx, set.Repo, set.Branch)
	if err != nil {
		prev.Enabled, prev.Repo, prev.Branch = true, set.Repo, set.Branch
		prev.Error = err.Error()
		prev.CheckedAt = time.Now().UTC()
		c.Log.Warn("update check failed", "repo", set.Repo, "err", err)
		return c.store(prev)
	}
	if notModified && prev.Latest != nil && prev.Repo == set.Repo && prev.Branch == set.Branch {
		prev.CheckedAt, prev.Error = time.Now().UTC(), ""
		return c.store(prev)
	}
	st.Latest = latest
	st.CheckedAt = time.Now().UTC()

	cur := c.Current.Commit
	switch {
	case cur == "":
		st.Behind = -1
		st.Note = "This build has no commit information (it was built outside a git checkout), so it can't tell whether it is current."
	case strings.HasPrefix(latest.SHA, cur) || strings.HasPrefix(cur, latest.SHA):
		st.Note = "Up to date."
	default:
		cmp, err := c.compare(ctx, set.Repo, cur, latest.SHA)
		var nf *notFound
		switch {
		case errors.As(err, &nf):
			// Our commit isn't on GitHub (local or unpushed build).
			st.Behind = -1
			st.Available = !c.Current.Time.IsZero() && latest.Time.After(c.Current.Time)
			st.Note = "This build's commit isn't on GitHub (a local build), so only dates can be compared."
		case err != nil:
			st.Error = err.Error()
		default:
			st.Behind = cmp.AheadBy
			st.Available = cmp.AheadBy > 0
			st.Commits = cmp.Commits
			st.CompareURL = cmp.URL
			if !st.Available {
				st.Note = "Up to date (this build is ahead of " + set.Branch + ")."
			}
		}
	}
	if st.Available {
		c.Log.Info("update available", "repo", set.Repo, "branch", set.Branch, "behind", st.Behind, "latest", short(latest.SHA))
	}
	return c.store(st)
}

func (c *Checker) store(s Status) Status {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
	return c.Status()
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

type notFound struct{ what string }

func (e *notFound) Error() string { return e.what + " not found on GitHub" }

func (c *Checker) get(ctx context.Context, url, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "aicluster-controller")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach GitHub: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		defer resp.Body.Close()
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.StatusCode == http.StatusTooManyRequests {
			reset := time.Now().Add(time.Hour)
			if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				reset = time.Unix(n, 0)
			}
			c.mu.Lock()
			c.nextAfter = reset
			c.mu.Unlock()
			return nil, fmt.Errorf("GitHub rate limit reached; next check after %s", reset.Local().Format(time.Kitchen))
		}
		return nil, fmt.Errorf("GitHub refused the request (%s)", resp.Status)
	}
	return resp, nil
}

func (c *Checker) branchHead(ctx context.Context, repo, branch string) (*Commit, bool, error) {
	key := repo + "@" + branch
	c.mu.Lock()
	etag := ""
	if c.etagFor == key {
		etag = c.etag
	}
	c.mu.Unlock()
	resp, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/commits/%s", strings.TrimRight(c.API, "/"), repo, branch), etag)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, true, nil
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return nil, false, fmt.Errorf("repository %s or branch %s not found on GitHub", repo, branch)
	default:
		return nil, false, fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var gc ghCommit
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&gc); err != nil || gc.SHA == "" {
		return nil, false, errors.New("unexpected response from GitHub")
	}
	c.mu.Lock()
	c.etag, c.etagFor = resp.Header.Get("ETag"), key
	c.mu.Unlock()
	cm := gc.toCommit()
	return &cm, false, nil
}

type ghCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

func (g ghCommit) toCommit() Commit {
	msg, _, _ := strings.Cut(g.Commit.Message, "\n")
	t := g.Commit.Committer.Date
	if t.IsZero() {
		t = g.Commit.Author.Date
	}
	return Commit{SHA: g.SHA, Message: strings.TrimSpace(msg), Author: g.Commit.Author.Name, Time: t, URL: g.HTMLURL}
}

type comparison struct {
	AheadBy int
	Commits []Commit
	URL     string
}

func (c *Checker) compare(ctx context.Context, repo, base, head string) (*comparison, error) {
	resp, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/compare/%s...%s", strings.TrimRight(c.API, "/"), repo, base, head), "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, &notFound{what: "commit " + short(base)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub compare returned %s", resp.Status)
	}
	var body struct {
		Status  string     `json:"status"`
		AheadBy int        `json:"ahead_by"`
		HTMLURL string     `json:"html_url"`
		Commits []ghCommit `json:"commits"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body); err != nil {
		return nil, errors.New("unexpected compare response from GitHub")
	}
	out := &comparison{AheadBy: body.AheadBy, URL: body.HTMLURL, Commits: []Commit{}}
	// GitHub lists oldest first; show newest first.
	for i := len(body.Commits) - 1; i >= 0 && len(out.Commits) < maxCommits; i-- {
		out.Commits = append(out.Commits, body.Commits[i].toCommit())
	}
	return out, nil
}

// Wake asks Run to check now (after a settings change).
func (c *Checker) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
