package runtime

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"time"
)

// The fake runtime's OpenAI-compatible server. Replies are canned but shaped
// exactly like Ollama's, including token streaming, so the gateway and the
// dashboard playground can be exercised without a GPU or real weights.

// startServer listens on loopback; replies name f.Label so tests and humans
// can see which node answered.
func (f *Fake) startServer() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return
	}
	f.url = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", f.chat)
	mux.HandleFunc("POST /v1/completions", f.chat)
	mux.HandleFunc("POST /v1/embeddings", f.embed)
	go func() { _ = http.Serve(ln, mux) }()
}

func (f *Fake) InferenceURL() string { return f.url }

type chatReq struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Prompt   string `json:"prompt"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

func oaiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": "invalid_request_error"}})
}

func (f *Fake) has(model string) bool {
	idx, err := f.index()
	if err != nil {
		return false
	}
	_, ok := idx[model]
	return ok
}

func (f *Fake) chat(w http.ResponseWriter, r *http.Request) {
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oaiError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	ok := f.has(req.Model)
	f.mu.Unlock()
	if !ok {
		oaiError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", req.Model))
		return
	}
	said := req.Prompt
	for _, m := range req.Messages {
		if m.Role == "user" {
			if s, ok := m.Content.(string); ok {
				said = s
			}
		}
	}
	if len(said) > 200 {
		said = said[:200] + "…"
	}
	reply := fmt.Sprintf("This is a stand-in reply from %s on %s (the fake runtime has no real model). You said: \"%s\"", req.Model, f.Label, said)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	isChat := strings.HasSuffix(r.URL.Path, "/chat/completions")

	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		choice := map[string]any{"index": 0, "finish_reason": "stop"}
		obj := "text_completion"
		if isChat {
			choice["message"] = map[string]string{"role": "assistant", "content": reply}
			obj = "chat.completion"
		} else {
			choice["text"] = reply
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": obj, "created": created, "model": req.Model, "choices": []any{choice},
			"usage": map[string]int{"prompt_tokens": len(strings.Fields(said)), "completion_tokens": len(strings.Fields(reply)), "total_tokens": len(strings.Fields(said + " " + reply))},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	words := strings.SplitAfter(reply, " ")
	for i, word := range words {
		if r.Context().Err() != nil {
			return
		}
		var choice map[string]any
		if isChat {
			delta := map[string]string{"content": word}
			if i == 0 {
				delta["role"] = "assistant"
			}
			choice = map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
		} else {
			choice = map[string]any{"index": 0, "text": word, "finish_reason": nil}
		}
		send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{choice}})
		time.Sleep(f.TokenDelay)
	}
	send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func (f *Fake) embed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oaiError(w, http.StatusBadRequest, err.Error())
		return
	}
	f.mu.Lock()
	ok := f.has(req.Model)
	f.mu.Unlock()
	if !ok {
		oaiError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", req.Model))
		return
	}
	var inputs []string
	switch v := req.Input.(type) {
	case string:
		inputs = []string{v}
	case []any:
		for _, x := range v {
			inputs = append(inputs, fmt.Sprint(x))
		}
	}
	var data []any
	for i, in := range inputs {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": fakeVector(in)})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": req.Model, "data": data})
}

// fakeVector is deterministic and unit-length, so cosine similarity works.
func fakeVector(s string) []float64 {
	sum := sha256.Sum256([]byte(s))
	v := make([]float64, 8)
	var n float64
	for i := range v {
		v[i] = float64(int16(binary.BigEndian.Uint16(sum[i*2:]))) / 32768
		n += v[i] * v[i]
	}
	for i := range v {
		v[i] /= math.Sqrt(n)
	}
	return v
}
