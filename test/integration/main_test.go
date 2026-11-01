package integration

import (
	"bufio"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"aicluster/test/fakeregistry"
)

// Shared for the whole package: a fake model registry (stands in for
// registry.ollama.ai) and the Python provisioner serving the repo catalog.
var (
	registry        *fakeregistry.Registry
	registryURL     string
	provisionerAddr string
	provisionerErr  error
)

func TestMain(m *testing.M) {
	registry = fakeregistry.New(256 << 10)
	srv := httptest.NewServer(registry)
	registryURL = srv.URL

	cmd, addr, err := startProvisioner()
	provisionerAddr, provisionerErr = addr, err
	if provisionerAddr == "" {
		provisionerAddr = "tcp:127.0.0.1:1" // nothing listens; model tests skip
	}
	code := m.Run()
	if cmd != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	srv.Close()
	os.Exit(code)
}

func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// pythonExe prefers $AICLUSTER_TEST_PYTHON, then the repo's .dev venv, then PATH.
func pythonExe(root string) string {
	if p := os.Getenv("AICLUSTER_TEST_PYTHON"); p != "" {
		return p
	}
	venv := filepath.Join(root, ".dev", "venv", "bin", "python")
	if runtime.GOOS == "windows" {
		venv = filepath.Join(root, ".dev", "venv", "Scripts", "python.exe")
	}
	if _, err := os.Stat(venv); err == nil {
		return venv
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return "python3"
}

func startProvisioner() (*exec.Cmd, string, error) {
	root := repoRoot()
	cmd := exec.Command(pythonExe(root), "-m", "aicluster_provisioner",
		"--listen", "tcp:127.0.0.1:0", "--catalog", filepath.Join(root, "catalog"), "--log-level", "warning")
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(root, "controller", "provisioner"))
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("start provisioner: %w", err)
	}
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if addr, ok := strings.CutPrefix(sc.Text(), "READY "); ok {
				ready <- addr
			}
		}
		close(ready)
	}()
	select {
	case addr, ok := <-ready:
		if !ok {
			_ = cmd.Wait()
			return nil, "", fmt.Errorf("provisioner exited before becoming ready")
		}
		return cmd, addr, nil
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		return nil, "", fmt.Errorf("provisioner did not start in time")
	}
}

func requireProvisioner(t *testing.T) {
	t.Helper()
	if provisionerErr != nil {
		t.Skipf("provisioner unavailable (needs Python 3.10+ with PyYAML): %v", provisionerErr)
	}
}
