// Package selfupdate is the part of in-place updates shared by the
// Controller and the Slave agent: verifying and unpacking an update bundle,
// and handing it to the root-owned updater (iso/payload/apply-update), which
// installs it, restarts the service and rolls back if it doesn't come up.
//
// The daemons never write system files themselves. They leave the unpacked
// bundle in Dir/staged and a request in Dir/request; a systemd path unit
// starts the updater when the request appears, and the updater leaves its
// verdict in Dir/result.json.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"aicluster/common/pki"
)

const (
	// DefaultDir is shared by both roles; the updater looks only here.
	DefaultDir = "/var/lib/aicluster/update"
	// UpdaterUnit is installed by install-target.sh. Without it nothing
	// would act on a request.
	UpdaterUnit = "/etc/systemd/system/aicluster-update.path"

	BundleName = "aicluster-update-linux-amd64.tar.gz"
	// ManifestName is published next to the bundle, and also inside it at
	// aicluster/manifest.json.
	ManifestName = "manifest.json"

	maxBundle   = 512 << 20
	maxUnpacked = 1 << 30
)

// Manifest says which commit a bundle was built from.
type Manifest struct {
	Commit  string `json:"commit"`
	Version string `json:"version"`
	Built   string `json:"built,omitempty"`
}

// Result is what the updater reports after an attempt.
type Result struct {
	Commit string `json:"commit"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	At     string `json:"at,omitempty"`
}

// BuildCommit is the git commit the running binary was built from, or "".
func BuildCommit() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}

// SameCommit compares full or abbreviated commit hashes.
func SameCommit(a, b string) bool {
	if len(a) < 7 || len(b) < 7 {
		return a != "" && a == b
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// Installer hands a staged bundle to whatever installs it.
type Installer interface {
	// Dir holds bundle.tar.gz, staged/, request and result.json.
	Dir() string
	// Available says why this machine can't update itself, or nil.
	Available() error
	// Request asks for the staged bundle to be installed. On success the
	// caller is normally stopped and restarted by the updater.
	Request(commit string) error
	// Result is the updater's last verdict, or nil if there is none.
	Result() (*Result, error)
}

// FileInstaller is the real one: request files for the systemd path unit.
type FileInstaller struct{ Path string }

func (f FileInstaller) Dir() string { return f.Path }

func (f FileInstaller) Available() error {
	if _, err := os.Stat(UpdaterUnit); err != nil {
		return errors.New("the updater isn't installed on this machine (installed from an older image, or not from the ISO); reinstall it once from a current ISO")
	}
	return nil
}

func (f FileInstaller) Request(commit string) error {
	_ = os.Remove(filepath.Join(f.Path, "result.json"))
	b, _ := json.Marshal(map[string]string{"commit": commit, "at": time.Now().UTC().Format(time.RFC3339)})
	return pki.WriteFileAtomic(filepath.Join(f.Path, "request"), b, 0o644)
}

func (f FileInstaller) Result() (*Result, error) { return ReadResult(f.Path) }

// ReadResult reads dir/result.json; a missing file is (nil, nil).
func ReadResult(dir string) (*Result, error) {
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("result.json: %w", err)
	}
	return &r, nil
}

// Verifying is true while the updater is installing commit, or still
// checking that the restarted service stays up: the bundle for it is staged
// and there is no verdict on it yet.
func Verifying(dir, commit string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "staged", "aicluster", ManifestName))
	if err != nil {
		return false
	}
	var m Manifest
	if json.Unmarshal(b, &m) != nil || !SameCommit(m.Commit, commit) {
		return false
	}
	r, _ := ReadResult(dir)
	return r == nil || !SameCommit(r.Commit, commit)
}

// SHA256File hashes a file.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Save copies r into dir/bundle.tar.gz, checking size and SHA-256 before
// the file replaces the previous bundle.
func Save(dir string, r io.Reader, wantSHA256 string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(dir, "bundle.tar.gz")
	tmp, err := os.CreateTemp(dir, "bundle-*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxBundle+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if n > maxBundle {
		return "", errors.New("update bundle is too large")
	}
	got := hex.EncodeToString(h.Sum(nil))
	if want := strings.TrimPrefix(strings.ToLower(wantSHA256), "sha256:"); got != want {
		return "", fmt.Errorf("update bundle checksum mismatch (got %s, want %s)", got[:12], want)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	return final, os.Rename(tmp.Name(), final)
}

// Stage unpacks bundle into dir/staged (replacing what was there) and
// returns the manifest it carries.
func Stage(dir, bundle string) (*Manifest, error) {
	staged := filepath.Join(dir, "staged")
	if err := os.RemoveAll(staged); err != nil {
		return nil, err
	}
	if err := Extract(bundle, staged); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(staged, "aicluster", ManifestName))
	if err != nil {
		return nil, errors.New("update bundle has no aicluster/manifest.json")
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil || m.Commit == "" {
		return nil, errors.New("update bundle has an invalid manifest")
	}
	for _, f := range []string{"install-target.sh", "apply-update"} {
		if _, err := os.Stat(filepath.Join(staged, "aicluster", f)); err != nil {
			return nil, fmt.Errorf("update bundle lacks aicluster/%s", f)
		}
	}
	return &m, nil
}

// Extract unpacks a .tar.gz of regular files and directories into dest.
// Anything else (links, devices, paths leaving dest) is refused.
func Extract(tarGz, dest string) error {
	f, err := os.Open(tarGz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("update bundle: %w", err)
	}
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("update bundle: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." {
			continue
		}
		if !filepath.IsLocal(name) || strings.HasPrefix(h.Name, "/") {
			return fmt.Errorf("update bundle: unsafe path %q", h.Name)
		}
		p := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; total > maxUnpacked {
				return errors.New("update bundle unpacks too large")
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("update bundle: %q is not a regular file or directory", h.Name)
		}
	}
}
