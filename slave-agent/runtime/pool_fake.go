package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FakePool stands in for llama.cpp on machines without it. Workers run a
// tiny line protocol ("PING n" -> "PONG <label> n"); the leader answers each
// chat request only after reaching every worker through its tunnel, so tests
// and the dev cluster exercise the real mTLS path between nodes.
type FakePool struct {
	Label      string
	TokenDelay time.Duration
	WorkerErr  string // when set, workers fail to start with this error
}

func (f *FakePool) Kind() string { return "fake" }
func (f *FakePool) Check() error { return nil }

type fakeProc struct {
	addr string
	stop func()
	done chan struct{}
	once sync.Once
}

func (p *fakeProc) Addr() string          { return p.addr }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Stop()                 { p.once.Do(func() { p.stop(); close(p.done) }) }

func (f *FakePool) StartWorker(ctx context.Context) (Proc, error) {
	if f.WorkerErr != "" {
		return nil, errors.New(f.WorkerErr)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	// One client at a time, like ggml-rpc-server: a connection the tunnel
	// fails to close blocks every later one.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			br := bufio.NewReader(c)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					break
				}
				n := strings.TrimSpace(strings.TrimPrefix(line, "PING"))
				fmt.Fprintf(c, "PONG %s %s\n", f.Label, n)
			}
			c.Close()
		}
	}()
	return &fakeProc{addr: ln.Addr().String(), stop: func() { ln.Close() }, done: make(chan struct{})}, nil
}

func (f *FakePool) StartLeader(ctx context.Context, spec LeaderSpec) (Proc, error) {
	// Prove every worker is reachable before declaring the model loaded.
	if _, err := f.pingAll(spec); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { f.chat(w, r, spec) })
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) { f.chat(w, r, spec) })
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return &fakeProc{addr: ln.Addr().String(), stop: func() { _ = srv.Close() }, done: make(chan struct{})}, nil
}

// pingAll reaches each worker through its tunnel and returns their labels.
func (f *FakePool) pingAll(spec LeaderSpec) ([]string, error) {
	var labels []string
	for i, w := range spec.Workers {
		c, err := net.DialTimeout("tcp", w.Addr, 5*time.Second)
		if err != nil {
			return nil, fmt.Errorf("worker %s unreachable: %w", w.Label, err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(c, "PING %d\n", i)
		line, err := bufio.NewReader(c).ReadString('\n')
		c.Close()
		parts := strings.Fields(line)
		if err != nil || len(parts) < 2 || parts[0] != "PONG" {
			return nil, fmt.Errorf("worker %s did not answer", w.Label)
		}
		labels = append(labels, fmt.Sprintf("%s (%.0f%%)", parts[1], w.Share*100))
	}
	return labels, nil
}

func (f *FakePool) chat(w http.ResponseWriter, r *http.Request, spec LeaderSpec) {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	labels, err := f.pingAll(spec)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": err.Error(), "type": "server_error"}})
		return
	}
	reply := fmt.Sprintf("Pooled stand-in reply from %s: layers computed on %s (%.0f%%) and %s.",
		spec.ModelName, f.Label, spec.LocalShare*100, strings.Join(labels, ", "))
	id := fmt.Sprintf("chatcmpl-pool-%d", time.Now().UnixNano())
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": spec.ModelName,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]string{"role": "assistant", "content": reply}}},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	for _, word := range strings.SplitAfter(reply, " ") {
		b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "model": spec.ModelName,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": word}}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(f.TokenDelay)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}
