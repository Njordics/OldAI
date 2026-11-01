package runtime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pooled models run on llama.cpp's RPC mode: workers run rpc-server (a
// compute backend reachable over TCP), and the leader runs llama-server with
// --rpc pointing at them, splitting layers between its own devices and the
// workers. All listeners are loopback-only; the agents tunnel worker
// connections over mTLS.

// Proc is a running pool process.
type Proc interface {
	Addr() string          // host:port it listens on (loopback)
	Done() <-chan struct{} // closed when it exits
	Stop()
}

type PoolWorkerRef struct {
	Addr  string  // loopback tunnel to the worker's rpc-server
	Share float64 // fraction of the layers
	Label string  // node name, for logs and the fake runtime
}

type LeaderSpec struct {
	ID         string
	ModelPath  string // GGUF file
	ModelName  string
	Workers    []PoolWorkerRef
	LocalShare float64
	Layers     int // total layers, 0 = unknown
	Parallel   int
}

type PoolRuntime interface {
	Kind() string
	Check() error
	StartWorker(ctx context.Context) (Proc, error)
	// StartLeader returns once the model is loaded and serving.
	StartLeader(ctx context.Context, spec LeaderSpec) (Proc, error)
}

// ---------- llama.cpp ----------

type LlamaCpp struct {
	Dir         string        // holds rpc-server and llama-server
	LoadTimeout time.Duration // how long a big model may take to load
	CacheDir    string        // rpc-server tensor cache (avoids re-sending weights)
}

func (l *LlamaCpp) Kind() string { return "llamacpp" }

func (l *LlamaCpp) bin(name string) string {
	p := filepath.Join(l.Dir, name)
	if _, err := os.Stat(p + ".exe"); err == nil {
		return p + ".exe"
	}
	return p
}

// command runs a llama.cpp binary with its directory on the library path:
// release builds ship libllama/libggml next to the executables.
func (l *LlamaCpp) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, l.bin(name), args...)
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+l.Dir)
	if l.CacheDir != "" {
		cmd.Env = append(cmd.Env, "LLAMA_CACHE="+l.CacheDir)
	}
	return cmd
}

// rpcServer is the worker binary: "ggml-rpc-server" in current llama.cpp
// releases, "rpc-server" in older ones.
func (l *LlamaCpp) rpcServer() string {
	if _, err := os.Stat(l.bin("ggml-rpc-server")); err == nil {
		return "ggml-rpc-server"
	}
	return "rpc-server"
}

func (l *LlamaCpp) Check() error {
	for _, b := range []string{l.rpcServer(), "llama-server"} {
		if _, err := os.Stat(l.bin(b)); err != nil {
			return fmt.Errorf("llama.cpp %s not found in %s", b, l.Dir)
		}
	}
	return nil
}

type execProc struct {
	addr string
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

func (p *execProc) Addr() string          { return p.addr }
func (p *execProc) Done() <-chan struct{} { return p.done }
func (p *execProc) Stop() {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	<-p.done
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func start(cmd *exec.Cmd, addr string, out *bytes.Buffer) (*execProc, error) {
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProc{addr: addr, cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}

func (l *LlamaCpp) StartWorker(ctx context.Context) (Proc, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{"-H", "127.0.0.1", "-p", strconv.Itoa(port)}
	if l.CacheDir != "" {
		_ = os.MkdirAll(l.CacheDir, 0o755)
		args = append(args, "-c")
	}
	cmd := l.command(context.Background(), l.rpcServer(), args...)
	var out bytes.Buffer
	addr := "127.0.0.1:" + strconv.Itoa(port)
	p, err := start(cmd, addr, &out)
	if err != nil {
		return nil, err
	}
	if err := waitTCP(ctx, addr, p.done, 15*time.Second); err != nil {
		p.Stop()
		return nil, fmt.Errorf("rpc-server: %w: %s", err, tail(out.String()))
	}
	return p, nil
}

func (l *LlamaCpp) StartLeader(ctx context.Context, spec LeaderSpec) (Proc, error) {
	var rpc []string
	for _, w := range spec.Workers {
		rpc = append(rpc, w.Addr)
	}
	// --rpc must come first: llama.cpp prints the list as soon as it parses --list-devices.
	// It waits on every worker, so bound it: a dead tunnel must fail, not hang.
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	list, err := l.command(lctx, "llama-server", "--rpc", strings.Join(rpc, ","), "--list-devices").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("llama-server --list-devices: %w: %s", err, tail(string(list)))
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := LeaderArgs(spec, ParseDevices(string(list)), port)
	cmd := l.command(context.Background(), "llama-server", args...)
	var out bytes.Buffer
	addr := "127.0.0.1:" + strconv.Itoa(port)
	p, err := start(cmd, addr, &out)
	if err != nil {
		return nil, err
	}
	timeout := l.LoadTimeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	if err := waitHealthy(ctx, "http://"+addr+"/health", p.done, timeout); err != nil {
		p.Stop()
		return nil, fmt.Errorf("llama-server: %w: %s", err, tail(out.String()))
	}
	return p, nil
}

var (
	deviceLine = regexp.MustCompile(`^\s+([A-Za-z]+\d*|RPC\[[^\]]+\])\s*:\s*(\S*)`)
	rpcIndexed = regexp.MustCompile(`^RPC\d+$`)
)

// ParseDevices reads `llama-server --list-devices` output into device names
// in llama.cpp's order, e.g. ["CUDA0", "RPC[127.0.0.1:50052]"]. RPC devices
// are named by address: older llama.cpp prints "RPC[addr]: …", newer prints
// "RPC0: addr (…)".
func ParseDevices(out string) []string {
	var devs []string
	in := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "Available devices") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if m := deviceLine.FindStringSubmatch(line); m != nil {
			if rpcIndexed.MatchString(m[1]) {
				m[1] = "RPC[" + m[2] + "]"
			}
			devs = append(devs, m[1])
		}
	}
	return devs
}

// LeaderArgs builds llama-server's command line. --tensor-split follows the
// device order llama.cpp reports; local GPUs share the leader's portion.
// Without local GPUs the leader's share stays on its CPU by offloading only
// the workers' share of layers (-ngl).
func LeaderArgs(spec LeaderSpec, devices []string, port int) []string {
	shareOf := map[string]float64{}
	for _, w := range spec.Workers {
		shareOf["RPC["+w.Addr+"]"] = w.Share
	}
	var local []string
	for _, d := range devices {
		if _, rpc := shareOf[d]; !rpc && d != "CPU" {
			local = append(local, d)
		}
	}
	split := make([]string, 0, len(devices))
	for _, d := range devices {
		if s, ok := shareOf[d]; ok {
			split = append(split, strconv.FormatFloat(s, 'f', 3, 64))
		} else if d != "CPU" {
			split = append(split, strconv.FormatFloat(spec.LocalShare/float64(len(local)), 'f', 3, 64))
		}
	}
	ngl := "999"
	if len(local) == 0 && spec.Layers > 0 {
		ngl = strconv.Itoa(int(math.Round(float64(spec.Layers) * (1 - spec.LocalShare))))
	}
	var rpc []string
	for _, w := range spec.Workers {
		rpc = append(rpc, w.Addr)
	}
	parallel := max(spec.Parallel, 1)
	return []string{
		"-m", spec.ModelPath, "--alias", spec.ModelName,
		"--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--rpc", strings.Join(rpc, ","), "-ngl", ngl, "--tensor-split", strings.Join(split, ","),
		"--parallel", strconv.Itoa(parallel),
	}
}

func waitTCP(ctx context.Context, addr string, done <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-done:
			return errors.New("exited")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("did not start in time")
}

func waitHealthy(ctx context.Context, url string, done <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r, err := http.Get(url); err == nil {
			r.Body.Close()
			if r.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-done:
			return errors.New("exited")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("model did not load in time")
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		s = "…" + s[len(s)-600:]
	}
	return s
}
