package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"aicluster/controller/memory"
)

// Shared memory in the gateway (protocol §18). A chat request from a key
// with a memory space gets that space's instructions and relevant facts as
// a system note; afterwards the user's message is read for new facts.
// Clients can opt a request out with the header X-AICluster-Memory: off.

const memoryHeader = "X-AICluster-Memory"

// SettingMemoryModel names the chat model that reads messages for facts;
// "" picks the smallest installed one.
const SettingMemoryModel = "memory_model"

// lastUserText is the text of the request's last user message.
func lastUserText(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m["role"] != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			return c
		case []any:
			var parts []string
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok && pm["type"] == "text" {
					if t, ok := pm["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			return strings.Join(parts, "\n")
		}
		return ""
	}
	return ""
}

// withMemory adds the space's memory note to a chat request, ahead of any
// system prompt the client sent. It returns the note, or nil.
func (g *Gateway) withMemory(ctx context.Context, r *http.Request, p *Principal, path string, req map[string]any) *memory.Note {
	if g.Memory == nil || p.Memory == "" || path != "/v1/chat/completions" || strings.EqualFold(r.Header.Get(memoryHeader), "off") {
		return nil
	}
	text := lastUserText(req)
	if did := g.Memory.Command(ctx, p.Memory, text); did != "" {
		g.log.Info("memory command", "space", p.Memory, "did", did)
	}
	note, err := g.Memory.Recall(ctx, p.Memory, text)
	if err != nil {
		g.log.Warn("memory recall", "space", p.Memory, "err", err)
		return nil
	}
	if note == nil {
		return nil
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) > 0 {
		if first, ok := msgs[0].(map[string]any); ok && first["role"] == "system" {
			if s, ok := first["content"].(string); ok {
				first["content"] = note.Text + "\n\n" + s
				return note
			}
		}
	}
	req["messages"] = append([]any{map[string]any{"role": "system", "content": note.Text}}, msgs...)
	return note
}

// afterMemory records what the reply used and learns from the message.
func (g *Gateway) afterMemory(p *Principal, req map[string]any, note *memory.Note) {
	if g.Memory == nil || p.Memory == "" {
		return
	}
	if note != nil {
		g.Memory.Used(note.IDs)
	}
	g.Memory.Learn(p.Memory, lastUserText(req))
}

// ---------- memory.AI ----------

// memoryAI lets memory use the cluster's own models.
type memoryAI struct{ g *Gateway }

// MemoryAI is the gateway's implementation of memory.AI.
func (g *Gateway) MemoryAI() memory.AI { return memoryAI{g} }

var errNoModel = errors.New("no suitable model is installed")

func (a memoryAI) Embed(ctx context.Context, texts []string) ([][]float32, string, error) {
	g := a.g
	snap, err := g.snapshotNow(ctx)
	if err != nil {
		return nil, "", err
	}
	var models []string
	for id, caps := range snap.caps {
		if slices.Contains(caps, "embeddings") && len(snap.replicas[id]) > 0 {
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		return nil, "", errNoModel
	}
	sort.Strings(models)
	model := models[0]
	body, _ := json.Marshal(map[string]any{"input": texts})
	out, err := g.internal(ctx, model, "/v1/embeddings", body)
	if err != nil {
		return nil, "", err
	}
	var r struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, "", err
	}
	vs := make([][]float32, len(texts))
	for i, d := range r.Data {
		idx := d.Index
		if idx < 0 || idx >= len(vs) {
			idx = i
		}
		if idx < len(vs) {
			vs[idx] = d.Embedding
		}
	}
	for _, v := range vs {
		if len(v) == 0 {
			return nil, "", fmt.Errorf("%s returned an empty embedding", model)
		}
	}
	return vs, model, nil
}

func (a memoryAI) Complete(ctx context.Context, prompt string, maxTokens int) (string, error) {
	g := a.g
	model, err := g.memoryModel(ctx)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":  maxTokens,
		"temperature": 0,
	})
	out, err := g.internal(ctx, model, "/v1/chat/completions", body)
	if err != nil {
		return "", err
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &r); err != nil || len(r.Choices) == 0 {
		return "", fmt.Errorf("%s gave no answer", model)
	}
	return r.Choices[0].Message.Content, nil
}

// memoryModel is the chat model that reads messages for facts: the one set
// in Settings, else the smallest installed chat model of at least about
// 1 GB (about 1.5B parameters) that doesn't reason out loud first. Smaller
// models copy the prompt's examples instead of reading the message; they
// are used only when nothing bigger is installed.
func (g *Gateway) memoryModel(ctx context.Context) (string, error) {
	snap, err := g.snapshotNow(ctx)
	if err != nil {
		return "", err
	}
	if want, _ := g.core.Store.Setting(ctx, SettingMemoryModel, ""); want != "" {
		if len(snap.replicas[want]) > 0 {
			return want, nil
		}
		return "", fmt.Errorf("the memory model %s isn't ready on any node", want)
	}
	size := map[string]int64{}
	if as, err := g.core.Store.ListAssignments(ctx, ""); err == nil {
		for _, a := range as {
			if s, ok := size[a.ModelID]; !ok || a.Size < s {
				size[a.ModelID] = a.Size
			}
		}
	}
	const capable = 900 << 20
	better := func(a, b string) bool { // is a a better choice than b?
		if b == "" {
			return true
		}
		if (size[a] >= capable) != (size[b] >= capable) {
			return size[a] >= capable
		}
		if size[a] != size[b] {
			return size[a] < size[b]
		}
		return a < b
	}
	var best string
	for id, reps := range snap.replicas {
		caps := snap.caps[id]
		if len(reps) == 0 || !slices.Contains(caps, "chat") || slices.Contains(caps, "reasoning") || slices.Contains(caps, "embeddings") {
			continue
		}
		if better(id, best) {
			best = id
		}
	}
	if best == "" {
		return "", errNoModel
	}
	return best, nil
}

// internal sends one non-streaming request for the cluster's own use,
// taking a slot like any client request.
func (g *Gateway) internal(ctx context.Context, model, path string, body []byte) ([]byte, error) {
	snap, err := g.snapshotNow(ctx)
	if err != nil {
		return nil, err
	}
	reps := snap.replicas[model]
	if len(reps) == 0 {
		return nil, errNoModel
	}
	rep, err := g.acquire(ctx, reps, map[string]bool{}, time.Now().Add(time.Minute))
	if err != nil {
		return nil, err
	}
	if rep == nil {
		return nil, fmt.Errorf("no node serving %s answered", model)
	}
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	req["model"] = rep.LocalName
	out, _ := json.Marshal(req)
	resp, err := g.forward(ctx, rep, path, out)
	if err != nil {
		g.release(rep.NodeID, true)
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	g.release(rep.NodeID, false)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s: %.200s", model, resp.Status, bytes.TrimSpace(data))
	}
	return data, nil
}
