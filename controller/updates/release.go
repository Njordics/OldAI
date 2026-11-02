package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"aicluster/common/selfupdate"
)

// Update bundles are built by CI (.github/workflows/update-bundle.yml) for
// every push and published on a rolling release tagged "update-<branch>",
// with the bundle and its manifest as assets. GitHub records each asset's
// SHA-256, which the Controller checks before using the bundle.

// ReleaseTag is the tag CI publishes the branch's bundle under.
func ReleaseTag(branch string) string {
	return "update-" + strings.ReplaceAll(branch, "/", "-")
}

// Bundle is a downloadable, verifiable update bundle.
type Bundle struct {
	Commit string
	URL    string
	SHA256 string
	Size   int64
}

type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// FindBundle looks up the bundle CI built for the configured branch and
// checks that it is for commit want.
func (c *Checker) FindBundle(ctx context.Context, want string) (*Bundle, error) {
	set := c.Settings(ctx)
	if set.Repo == "" {
		set.Repo = DefaultRepo
	}
	if set.Branch == "" {
		set.Branch = DefaultBranch
	}
	if c.Offline() {
		return nil, errors.New("offline mode: not contacting GitHub")
	}
	tag := ReleaseTag(set.Branch)
	resp, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/releases/tags/%s", strings.TrimRight(c.API, "/"), set.Repo, tag), "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s has no release %q with an update bundle yet; it is published by the repository's CI after each push", set.Repo, tag)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s for release %s", resp.Status, tag)
	}
	var rel struct {
		Assets []ghAsset `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, errors.New("unexpected release response from GitHub")
	}
	var bundle, manifest *ghAsset
	for i := range rel.Assets {
		switch rel.Assets[i].Name {
		case selfupdate.BundleName:
			bundle = &rel.Assets[i]
		case selfupdate.ManifestName:
			manifest = &rel.Assets[i]
		}
	}
	if bundle == nil || manifest == nil {
		return nil, fmt.Errorf("release %s lacks %s or %s", tag, selfupdate.BundleName, selfupdate.ManifestName)
	}
	if !strings.HasPrefix(bundle.Digest, "sha256:") {
		return nil, fmt.Errorf("GitHub gives no SHA-256 for %s, so it can't be verified", bundle.Name)
	}
	m, err := c.fetchManifest(ctx, manifest.URL)
	if err != nil {
		return nil, err
	}
	if !selfupdate.SameCommit(m.Commit, want) {
		return nil, fmt.Errorf("the update bundle for %.7s isn't published yet (the latest is for %.7s); GitHub builds it a few minutes after each push, so try again shortly", want, m.Commit)
	}
	return &Bundle{Commit: m.Commit, URL: bundle.URL, SHA256: strings.TrimPrefix(bundle.Digest, "sha256:"), Size: bundle.Size}, nil
}

func (c *Checker) fetchManifest(ctx context.Context, url string) (*selfupdate.Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aicluster-controller")
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download manifest: %s", resp.Status)
	}
	var m selfupdate.Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m); err != nil || m.Commit == "" {
		return nil, errors.New("the published manifest is invalid")
	}
	return &m, nil
}

// Download saves the bundle into dir/bundle.tar.gz after checking its
// SHA-256 against GitHub's.
func (c *Checker) Download(ctx context.Context, b *Bundle, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "aicluster-controller")
	// The shared client's 20 s timeout is for API calls, not downloads.
	client := *c.Client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download update: %s", resp.Status)
	}
	return selfupdate.Save(dir, resp.Body, b.SHA256)
}
