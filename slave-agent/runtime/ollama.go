package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama drives a local Ollama server (bound to loopback). Models are pulled
// from the agent's registry shim, which forwards to the Controller's cache,
// then copied to their plain name so clients see "qwen2.5:7b-…".
type Ollama struct {
	URL    string // http://127.0.0.1:11434
	Client *http.Client
}

func NewOllama(url string) *Ollama {
	return &Ollama{URL: strings.TrimRight(url, "/"), Client: &http.Client{}}
}

func (o *Ollama) Kind() string { return "ollama" }

// InferenceURL: Ollama serves the OpenAI API under /v1 itself.
func (o *Ollama) InferenceURL() string { return o.URL }

func (o *Ollama) call(ctx context.Context, method, path string, in, out any, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ollama %s %s: %s %s", method, path, resp.Status, ollamaErr(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func ollamaErr(data []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(data))
}

func (o *Ollama) Check(ctx context.Context) error {
	var v struct {
		Version string `json:"version"`
	}
	if err := o.call(ctx, http.MethodGet, "/api/version", nil, &v, 3*time.Second); err != nil {
		return fmt.Errorf("ollama not reachable at %s: %w", o.URL, err)
	}
	return nil
}

func (o *Ollama) Installed(ctx context.Context) (map[string]bool, error) {
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := o.call(ctx, http.MethodGet, "/api/tags", nil, &tags, 10*time.Second); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range tags.Models {
		out[m.Name] = true
	}
	return out, nil
}

func (o *Ollama) Pull(ctx context.Context, m Model, p Progress) error {
	pullName := m.RegistryHost + "/" + m.Ref.String()
	b, _ := json.Marshal(map[string]any{"model": pullName, "insecure": true, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/pull", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("ollama pull: %s %s", resp.Status, ollamaErr(data))
	}
	// Ollama streams one JSON object per line, per blob.
	type layer struct{ done, total int64 }
	layers := map[string]*layer{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	success := false
	for sc.Scan() {
		var ev struct {
			Status    string `json:"status"`
			Digest    string `json:"digest"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Error != "" {
			return errors.New("ollama pull: " + ev.Error)
		}
		if ev.Digest != "" {
			l := layers[ev.Digest]
			if l == nil {
				l = &layer{}
				layers[ev.Digest] = l
			}
			l.done, l.total = ev.Completed, max(ev.Total, l.total)
			var done, total int64
			for _, l := range layers {
				done += l.done
				total += l.total
			}
			if p != nil {
				p(done, total)
			}
		}
		if ev.Status == "success" {
			success = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !success {
		return errors.New("ollama pull ended without success")
	}
	// Give it the plain name, drop the registry-prefixed one (blobs are shared).
	if err := o.call(ctx, http.MethodPost, "/api/copy", map[string]string{"source": pullName, "destination": m.LocalName}, nil, time.Minute); err != nil {
		return err
	}
	_ = o.call(ctx, http.MethodDelete, "/api/delete", map[string]string{"model": pullName}, nil, time.Minute)
	return nil
}

// Load asks Ollama to load the model now and keep it loaded (keep_alive -1).
func (o *Ollama) Load(ctx context.Context, m Model) error {
	return o.call(ctx, http.MethodPost, "/api/generate", map[string]any{"model": m.LocalName, "keep_alive": -1, "stream": false}, nil, 10*time.Minute)
}

func (o *Ollama) Unload(ctx context.Context, m Model) error {
	return o.call(ctx, http.MethodPost, "/api/generate", map[string]any{"model": m.LocalName, "keep_alive": 0, "stream": false}, nil, time.Minute)
}

func (o *Ollama) Remove(ctx context.Context, m Model) error {
	err := o.call(ctx, http.MethodDelete, "/api/delete", map[string]string{"model": m.LocalName}, nil, time.Minute)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}
