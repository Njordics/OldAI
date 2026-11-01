package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aicluster/common/proto"
	"aicluster/controller/core"
	"aicluster/controller/gateway"
	"aicluster/controller/store"
)

type gw struct {
	t   *testing.T
	url string
}

func startGateway(t *testing.T, c *ctrl, cfg gateway.Config) *gw {
	srv := httptest.NewServer(gateway.New(c.Core, cfg, logger("gateway")).Handler())
	t.Cleanup(srv.Close)
	return &gw{t: t, url: srv.URL}
}

func (g *gw) do(key, method, path, body string) (*http.Response, string) {
	g.t.Helper()
	req, _ := http.NewRequest(method, g.url+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func errCode(body string) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	return e.Error.Code
}

// installEverywhere puts model on each slave and waits until all are loaded.
func installOn(t *testing.T, c *ctrl, model string, slaves ...*slave) {
	t.Helper()
	var ids []string
	for _, s := range slaves {
		ids = append(ids, s.NodeID())
	}
	as, err := c.Install(context.Background(), core.InstallRequest{ModelID: model, Nodes: ids})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		c.waitModel(t, a.ID, proto.ModelLoaded)
	}
}

const chatBody = `{"model":"qwen2.5:0.5b","messages":[{"role":"user","content":"hello cluster"}]}`

func TestGatewayChatAndAuth(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s1, s2 := startSlave(t, c), startSlave(t, c)
	c.waitState(t, s1.NodeID(), store.StateAdopted, adoptTimeout)
	c.waitState(t, s2.NodeID(), store.StateAdopted, adoptTimeout)
	installOn(t, c, "qwen2.5:0.5b", s1, s2)
	installOn(t, c, "all-minilm:22m", s2)
	g := startGateway(t, c, gateway.Config{})
	ctx := context.Background()
	_, key, err := c.Store.CreateAPIKey(ctx, "test", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Auth.
	if r, b := g.do("", "POST", "/v1/chat/completions", chatBody); r.StatusCode != 401 || errCode(b) != "missing_api_key" {
		t.Fatalf("no key: %d %s", r.StatusCode, b)
	}
	if r, b := g.do("aic_wrongwrongwrong", "POST", "/v1/chat/completions", chatBody); r.StatusCode != 401 || errCode(b) != "invalid_api_key" {
		t.Fatalf("bad key: %d %s", r.StatusCode, b)
	}

	// Models.
	r, b := g.do(key, "GET", "/v1/models", "")
	var list struct {
		Data []gateway.ModelInfo `json:"data"`
	}
	_ = json.Unmarshal([]byte(b), &list)
	if r.StatusCode != 200 || len(list.Data) != 2 || list.Data[1].ID != "qwen2.5:0.5b" || list.Data[1].AICluster.Replicas != 2 {
		t.Fatalf("models: %d %s", r.StatusCode, b)
	}

	// Chat, whole answer: model name is the cluster's, not the runtime's.
	r, b = g.do(key, "POST", "/v1/chat/completions", chatBody)
	var chat struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal([]byte(b), &chat)
	if r.StatusCode != 200 || chat.Model != "qwen2.5:0.5b" || !strings.Contains(chat.Choices[0].Message.Content, `You said: "hello cluster"`) {
		t.Fatalf("chat: %d %s", r.StatusCode, b)
	}
	if r.Header.Get("X-AICluster-Node") == "" {
		t.Fatal("no X-AICluster-Node header")
	}

	// Streaming.
	r, b = g.do(key, "POST", "/v1/chat/completions", `{"model":"qwen2.5:0.5b","stream":true,"messages":[{"role":"user","content":"stream please"}]}`)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") || !strings.HasSuffix(strings.TrimSpace(b), "data: [DONE]") {
		t.Fatalf("stream: %d %q", r.StatusCode, b)
	}
	var text strings.Builder
	for _, line := range strings.Split(b, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var ch struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal([]byte(data), &ch)
		if len(ch.Choices) > 0 {
			text.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if strings.Contains(b, `"model":"qwen2.5:0.5b-`) || !strings.Contains(b, `"model":"qwen2.5:0.5b"`) {
		t.Fatalf("stream leaks the runtime's model name: %q", b[:200])
	}
	if !strings.Contains(text.String(), `You said: "stream please"`) {
		t.Fatalf("streamed text = %q", text.String())
	}

	// Embeddings.
	r, b = g.do(key, "POST", "/v1/embeddings", `{"model":"all-minilm:22m","input":["a","b"]}`)
	var emb struct {
		Data []struct{ Embedding []float64 } `json:"data"`
	}
	_ = json.Unmarshal([]byte(b), &emb)
	if r.StatusCode != 200 || len(emb.Data) != 2 || len(emb.Data[0].Embedding) == 0 {
		t.Fatalf("embeddings: %d %s", r.StatusCode, b)
	}

	// Errors.
	if r, b := g.do(key, "POST", "/v1/chat/completions", `{"model":"llama3.1:70b","messages":[]}`); r.StatusCode != 404 || errCode(b) != "model_not_found" {
		t.Fatalf("missing model: %d %s", r.StatusCode, b)
	}
	if r, b := g.do(key, "POST", "/v1/chat/completions", `{"messages":[]}`); r.StatusCode != 400 || errCode(b) != "missing_model" {
		t.Fatalf("no model: %d %s", r.StatusCode, b)
	}

	// A key limited to one model.
	_, limited, _ := c.Store.CreateAPIKey(ctx, "embed-only", []string{"all-minilm:22m"}, 0)
	if r, b := g.do(limited, "POST", "/v1/chat/completions", chatBody); r.StatusCode != 403 || errCode(b) != "model_not_allowed" {
		t.Fatalf("limited key: %d %s", r.StatusCode, b)
	}
	if _, b := g.do(limited, "GET", "/v1/models", ""); strings.Contains(b, "qwen") {
		t.Fatalf("limited key sees other models: %s", b)
	}

	// Rate limit.
	_, slow, _ := c.Store.CreateAPIKey(ctx, "slow", nil, 2)
	for i := 0; i < 2; i++ {
		if r, _ := g.do(slow, "GET", "/v1/models", ""); r.StatusCode != 200 {
			t.Fatalf("within limit: %d", r.StatusCode)
		}
	}
	if r, b := g.do(slow, "GET", "/v1/models", ""); r.StatusCode != 429 || errCode(b) != "rate_limit_exceeded" {
		t.Fatalf("over limit: %d %s", r.StatusCode, b)
	}

	// Revoked key stops working.
	keys, _ := c.Store.ListAPIKeys(ctx)
	for _, k := range keys {
		if k.Name == "test" {
			_ = c.Store.DeleteAPIKey(ctx, k.ID)
		}
	}
	if r, _ := g.do(key, "GET", "/v1/models", ""); r.StatusCode != 401 {
		t.Fatalf("revoked key: %d", r.StatusCode)
	}
}

// Concurrent requests spread over both nodes; when one node dies, requests
// fail over to the other before the node is even marked offline.
func TestGatewayBalanceAndFailover(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s1, s2 := startSlave(t, c, withSlots(2)), startSlave(t, c, withSlots(2))
	c.waitState(t, s1.NodeID(), store.StateAdopted, adoptTimeout)
	c.waitState(t, s2.NodeID(), store.StateAdopted, adoptTimeout)
	installOn(t, c, "qwen2.5:0.5b", s1, s2)
	g := startGateway(t, c, gateway.Config{})
	_, key, _ := c.Store.CreateAPIKey(context.Background(), "t", nil, 0)

	streamBody := `{"model":"qwen2.5:0.5b","stream":true,"messages":[{"role":"user","content":"x"}]}`
	var mu sync.Mutex
	nodes := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := g.do(key, "POST", "/v1/chat/completions", streamBody)
			mu.Lock()
			nodes[r.Header.Get("X-AICluster-Node")]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(nodes) != 2 {
		t.Fatalf("4 concurrent requests on 2x2 slots used nodes %v", nodes)
	}

	s1.stop() // connection refused from now on; still "adopted" for a few seconds
	for i := 0; i < 4; i++ {
		r, b := g.do(key, "POST", "/v1/chat/completions", chatBody)
		if r.StatusCode != 200 {
			t.Fatalf("request %d after node loss: %d %s", i, r.StatusCode, b)
		}
	}
	c.waitState(t, s1.NodeID(), store.StateOffline, offlineTimeout)
	if r, b := g.do(key, "GET", "/v1/models", ""); !strings.Contains(b, `"replicas":1`) {
		t.Fatalf("offline node still listed: %d %s", r.StatusCode, b)
	}
}

// With every slot busy, requests queue; past the queue limit they get 429.
func TestGatewayQueue(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c, withSlots(1))
	c.waitState(t, s.NodeID(), store.StateAdopted, adoptTimeout)
	installOn(t, c, "qwen2.5:0.5b", s)
	g := startGateway(t, c, gateway.Config{MaxQueue: 1, QueueTimeout: 10 * time.Second})
	_, key, _ := c.Store.CreateAPIKey(context.Background(), "t", nil, 0)

	// Hold the only slot with a streaming request we read slowly.
	req, _ := http.NewRequest("POST", g.url+"/v1/chat/completions",
		strings.NewReader(`{"model":"qwen2.5:0.5b","stream":true,"messages":[{"role":"user","content":"hold"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	hold, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(hold.Body)
	_, _ = br.ReadString('\n') // first token arrived: the slot is taken

	queued := make(chan int, 1)
	go func() {
		r, _ := g.do(key, "POST", "/v1/chat/completions", chatBody)
		queued <- r.StatusCode
	}()
	time.Sleep(300 * time.Millisecond) // let it enter the queue
	if r, b := g.do(key, "POST", "/v1/chat/completions", chatBody); r.StatusCode != 429 || errCode(b) != "queue_full" {
		t.Fatalf("over queue: %d %s", r.StatusCode, b)
	}
	_, _ = io.Copy(io.Discard, br)
	hold.Body.Close()
	select {
	case code := <-queued:
		if code != 200 {
			t.Fatalf("queued request finished with %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("queued request never ran")
	}
}
