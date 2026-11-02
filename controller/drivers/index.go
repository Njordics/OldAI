// Package drivers picks GPU drivers for Slaves from Ubuntu's archive and
// serves that archive to them through the Controller (protocol §14).
//
// Ubuntu's NVIDIA driver packages list the cards they support as modalias
// patterns, the same data the ubuntu-drivers tool uses. The Controller
// matches each Slave's cards against them and names the packages; the
// Slave installs them with apt, which checks Ubuntu's signatures.
package drivers

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Driver is one nvidia-driver-* package that declares the cards it supports.
type Driver struct {
	Package string // e.g. nvidia-driver-580-server-open
	Branch  int    // 580
	Server  bool   // the -server (long-term support) line
	Open    bool   // NVIDIA's open kernel modules (Turing and newer)
	Version string
	aliases []string // upper-cased modalias globs
}

// Suffix is the part every package of this driver shares after its kind,
// e.g. "580-server-open".
func (d Driver) Suffix() string {
	s := strconv.Itoa(d.Branch)
	if d.Server {
		s += "-server"
	}
	if d.Open {
		s += "-open"
	}
	return s
}

// Supports reports whether the driver declares the device modalias.
func (d Driver) Supports(modalias string) bool {
	m := strings.ToUpper(modalias)
	for _, p := range d.aliases {
		if ok, _ := path.Match(p, m); ok {
			return true
		}
	}
	return false
}

type pkg struct {
	version string
	depends string
}

// Index is what the Controller knows about one Ubuntu release's restricted
// component: every package name, and the drivers.
type Index struct {
	pkgs    map[string]pkg
	drivers map[string]Driver
}

func NewIndex() *Index { return &Index{pkgs: map[string]pkg{}, drivers: map[string]Driver{}} }

var driverName = regexp.MustCompile(`^nvidia-driver-(\d+)(-server)?(-open)?$`)

// AddPackages reads a Packages file (gzip-compressed when gz is set). When a
// package appears more than once, as in release and -updates, the newest
// version wins.
func (ix *Index) AddPackages(r io.Reader, gz bool) error {
	if gz {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20) // Modaliases lines run to hundreds of KB
	fields := map[string]string{}
	var last string
	flush := func() {
		if name := fields["Package"]; name != "" {
			ix.add(name, fields)
		}
		clear(fields)
		last = ""
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case line[0] == ' ' || line[0] == '\t':
			if last != "" {
				fields[last] += " " + strings.TrimSpace(line)
			}
		default:
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			last = k
			switch k {
			case "Package", "Version", "Depends", "Modaliases":
				fields[k] = strings.TrimSpace(v)
			default:
				last = ""
			}
		}
	}
	flush()
	return sc.Err()
}

func (ix *Index) add(name string, f map[string]string) {
	if old, ok := ix.pkgs[name]; ok && CompareVersions(old.version, f["Version"]) >= 0 {
		return
	}
	p := pkg{version: f["Version"]}
	if strings.HasPrefix(name, "linux-modules-nvidia-") {
		p.depends = f["Depends"]
	}
	ix.pkgs[name] = p
	m := driverName.FindStringSubmatch(name)
	if m == nil {
		return
	}
	// The newest version decides whether the driver lists any cards.
	delete(ix.drivers, name)
	aliases := parseModaliases(f["Modaliases"])
	if len(aliases) == 0 {
		return
	}
	branch, _ := strconv.Atoi(m[1])
	ix.drivers[name] = Driver{Package: name, Branch: branch, Server: m[2] != "", Open: m[3] != "", Version: f["Version"], aliases: aliases}
}

// parseModaliases turns "nvidia(pci:v…, pci:v…), other(…)" into globs.
func parseModaliases(s string) []string {
	var out []string
	for s != "" {
		open := strings.IndexByte(s, '(')
		end := strings.IndexByte(s, ')')
		if open < 0 || end < open {
			break
		}
		for _, a := range strings.Split(s[open+1:end], ",") {
			if a = strings.TrimSpace(a); a != "" {
				out = append(out, strings.ToUpper(a))
			}
		}
		s = s[end+1:]
	}
	return out
}

// Has reports whether the archive has a package of that name.
func (ix *Index) Has(name string) bool { _, ok := ix.pkgs[name]; return ok }

// Len is the number of packages indexed.
func (ix *Index) Len() int { return len(ix.pkgs) }

// ErrNoDriver means no driver in the archive supports the card.
var ErrNoDriver = errors.New("no driver in Ubuntu's archive supports this card")

// Choice is the driver picked for a node and the packages that install it.
type Choice struct {
	Driver   Driver
	Packages []string
}

// Choose picks a driver that supports every one of the node's NVIDIA cards
// (given as modaliases) and works with its kernel (from uname -r).
//
// Like `ubuntu-drivers --gpgpu`, it prefers the -server line, then the
// newest branch, then the open kernel modules NVIDIA recommends for cards
// that support them. It installs the headless driver without DKMS: kernel
// modules come prebuilt and signed by Canonical, so they load with Secure
// Boot on and nothing is compiled on the node.
func (ix *Index) Choose(modaliases []string, kernel string) (*Choice, error) {
	var fits []Driver
	for _, d := range ix.drivers {
		ok := true
		for _, m := range modaliases {
			ok = ok && d.Supports(m)
		}
		if ok {
			fits = append(fits, d)
		}
	}
	sort.Slice(fits, func(i, j int) bool {
		a, b := fits[i], fits[j]
		if a.Server != b.Server {
			return a.Server
		}
		if a.Branch != b.Branch {
			return a.Branch > b.Branch
		}
		return a.Open && !b.Open
	})
	var lastErr error = ErrNoDriver
	for _, d := range fits {
		pkgs, err := ix.packagesFor(d, kernel)
		if err != nil {
			lastErr = err
			continue
		}
		return &Choice{Driver: d, Packages: pkgs}, nil
	}
	return nil, lastErr
}

func (ix *Index) packagesFor(d Driver, kernel string) ([]string, error) {
	sfx := d.Suffix()
	userland := []string{"nvidia-headless-no-dkms-" + sfx, "nvidia-utils-" + strings.TrimSuffix(sfx, "-open")}
	for _, p := range userland {
		if !ix.Has(p) {
			return nil, fmt.Errorf("%s: package %s is missing from the archive", d.Package, p)
		}
	}
	mods, err := ix.modulesFor("linux-modules-nvidia-"+sfx, kernel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Package, err)
	}
	return append(userland, mods), nil
}

var kernelRe = regexp.MustCompile(`^(\d+\.\d+)\.\d+-\d+-([a-z0-9-]+)$`)

// modulesFor picks the meta package that keeps prebuilt modules matching the
// node's kernel line, e.g. linux-modules-nvidia-580-server-open-generic for
// Ubuntu's 6.8 kernels. Old kernels' module packages pin older driver
// versions, so the node moves to the newest kernel of its line; the agent
// reboots into it when the running kernel has no module.
func (ix *Index) modulesFor(base, kernel string) (string, error) {
	m := kernelRe.FindStringSubmatch(kernel)
	if m == nil && ix.Has(base+"-generic") {
		// Not reported, or not an Ubuntu kernel name: Ubuntu's main line.
		return base + "-generic", nil
	}
	if m == nil {
		return "", fmt.Errorf("can't tell the kernel line of %q", kernel)
	}
	series, flavor := m[1], m[2]
	meta := base + "-" + flavor
	var cands []string
	for name := range ix.pkgs {
		if name == meta || strings.HasPrefix(name, meta+"-") && !strings.HasSuffix(name, "-edge") && !kernelPkgRe.MatchString(name) {
			cands = append(cands, name)
		}
	}
	// The plain flavor first (Ubuntu's main kernel line), then HWE, then
	// lines named by version.
	sort.Slice(cands, func(i, j int) bool {
		ri, rj := metaRank(cands[i], meta), metaRank(cands[j], meta)
		if ri != rj {
			return ri < rj
		}
		return cands[i] < cands[j]
	})
	for _, c := range cands {
		if ix.seriesOf(c) == series {
			return c, nil
		}
	}
	if ix.Has(meta) {
		return meta, nil // a different kernel line; the node changes to it
	}
	return "", fmt.Errorf("no prebuilt kernel modules for %s kernels", flavor)
}

var kernelPkgRe = regexp.MustCompile(`-\d+\.\d+\.\d+-\d+-[a-z0-9-]+$`)

func metaRank(name, meta string) int {
	switch {
	case name == meta:
		return 0
	case strings.Contains(name, "-hwe-"):
		return 1
	default:
		return 2
	}
}

var dependsKernelRe = regexp.MustCompile(`linux-modules-nvidia-[a-z0-9-]+?-(\d+\.\d+)\.\d+-\d+-[a-z0-9-]+`)

// seriesOf reads a module meta package's kernel line from what it depends on.
func (ix *Index) seriesOf(meta string) string {
	if m := dependsKernelRe.FindStringSubmatch(ix.pkgs[meta].depends); m != nil {
		return m[1]
	}
	return ""
}
