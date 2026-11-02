package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"aicluster/common/proto"
)

// AptDrivers installs GPU drivers with apt (Ubuntu, run as root). It keeps
// its own source list and package lists under Dir, so the node's normal
// apt sources, which point at the unreachable internet, aren't used or
// changed.
type AptDrivers struct {
	Dir     string // e.g. /var/lib/aicluster/apt
	Runtime string // systemd unit of the model runtime, e.g. ollama.service
	Log     *slog.Logger
}

const (
	ubuntuKeyring = "/usr/share/keyrings/ubuntu-archive-keyring.gpg"
	// nouveauConf keeps the open-source NVIDIA driver off the card.
	nouveauConf = "/etc/modprobe.d/aicluster-blacklist-nouveau.conf"
)

func (d *AptDrivers) aptOptions() []string {
	return []string{
		"-o", "Dir::Etc::SourceList=" + filepath.Join(d.Dir, "sources.list"),
		"-o", "Dir::Etc::SourceParts=" + filepath.Join(d.Dir, "sources.list.d"),
		"-o", "Dir::State::Lists=" + filepath.Join(d.Dir, "lists"),
		// The Controller may be downloading a large package before it answers.
		"-o", "Acquire::http::Timeout=900",
		"-o", "Acquire::Retries=3",
		"-o", "Acquire::Languages=none",
		// Only package lists: no app-store metadata or command-not-found data.
		"-o", "Acquire::IndexTargets::deb::DEP-11::DefaultEnabled=false",
		"-o", "Acquire::IndexTargets::deb::DEP-11-icons::DefaultEnabled=false",
		"-o", "Acquire::IndexTargets::deb::DEP-11-icons-hidpi::DefaultEnabled=false",
		"-o", "Acquire::IndexTargets::deb::CNF::DefaultEnabled=false",
		"-o", "DPkg::Lock::Timeout=600",
	}
}

func (d *AptDrivers) Installed(ctx context.Context, pkgs []string) bool {
	out, err := exec.CommandContext(ctx, "dpkg-query", append([]string{"-W", "-f=${db:Status-Abbrev}\n"}, pkgs...)...).Output()
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, l := range lines {
		if !strings.HasPrefix(l, "ii") {
			return false
		}
	}
	return len(lines) == len(pkgs)
}

func (d *AptDrivers) Install(ctx context.Context, spec proto.DriverSpec, baseURL string) error {
	for _, dir := range []string{filepath.Join(d.Dir, "lists", "partial"), filepath.Join(d.Dir, "sources.list.d")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var src strings.Builder
	for _, s := range spec.Suites {
		fmt.Fprintf(&src, "deb [signed-by=%s] %s %s %s\n", ubuntuKeyring, baseURL, s, strings.Join(spec.Components, " "))
	}
	if err := os.WriteFile(filepath.Join(d.Dir, "sources.list"), []byte(src.String()), 0o644); err != nil {
		return err
	}
	// Finish an install that a restart of the agent cut short.
	if out, err := exec.CommandContext(ctx, "dpkg", "--configure", "-a").CombinedOutput(); err != nil {
		return fmt.Errorf("dpkg --configure -a: %v: %s", err, lastLines(string(out), 4))
	}
	if err := d.apt(ctx, "update"); err != nil {
		return err
	}
	args := append([]string{"install", "-y", "--no-install-recommends",
		"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}, spec.Packages...)
	return d.apt(ctx, args...)
}

func (d *AptDrivers) apt(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "apt-get", append(d.aptOptions(), args...)...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("apt-get %s: %v: %s", args[0], err, lastLines(out.String(), 4))
	}
	if d.Log != nil {
		d.Log.Debug("apt-get", "args", args, "output", lastLines(out.String(), 20))
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func (d *AptDrivers) Load(ctx context.Context) (string, error) {
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(kernel))
	if exec.CommandContext(ctx, "modinfo", "-k", k, "nvidia").Run() != nil {
		return "", fmt.Errorf("%w: the driver's kernel module is for a newer kernel than %s", ErrNeedReboot, k)
	}
	if _, err := os.Stat("/sys/module/nouveau"); err == nil {
		// nouveau holds the card until it is kept out at boot.
		if err := os.WriteFile(nouveauConf, []byte("blacklist nouveau\noptions nouveau modeset=0\n"), 0o644); err != nil {
			return "", err
		}
		if out, err := exec.CommandContext(ctx, "update-initramfs", "-u").CombinedOutput(); err != nil {
			return "", fmt.Errorf("update-initramfs: %v: %s", err, lastLines(string(out), 3))
		}
		return "", fmt.Errorf("%w: the open-source nouveau driver holds the card", ErrNeedReboot)
	}
	for _, mod := range []string{"nvidia", "nvidia_uvm"} {
		if out, err := exec.CommandContext(ctx, "modprobe", mod).CombinedOutput(); err != nil {
			return "", fmt.Errorf("modprobe %s: %v: %s", mod, err, strings.TrimSpace(string(out)))
		}
	}
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader").Output()
	if err != nil {
		return "", fmt.Errorf("nvidia-smi: %v", err)
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return version, nil
}

func (d *AptDrivers) Reboot() error {
	return exec.Command("systemctl", "reboot").Run()
}

func (d *AptDrivers) RestartRuntime(ctx context.Context) error {
	if d.Runtime == "" {
		return nil
	}
	return exec.CommandContext(ctx, "systemctl", "try-restart", d.Runtime).Run()
}
