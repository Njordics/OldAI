package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aicluster/controller/core"
	"aicluster/controller/gateway"
	"aicluster/controller/memory"
	"aicluster/controller/store"
)

// Memory reaches chats through the gateway: "remember …" is stored, the
// space's instructions and relevant facts go in front of the request, and
// keys without memory, or requests that opt out, get none.
func TestMemoryInChats(t *testing.T) {
	requireProvisioner(t)
	c := startController(t, core.ModeAuto)
	s := startSlave(t, c)
	adoptAll(t, c, s)
	installOn(t, c, "qwen2.5:0.5b", s)
	ctx := context.Background()

	gw := gateway.New(c.Core, gateway.Config{}, logger("gateway"))
	mem := &memory.Service{Store: c.Store, AI: gw.MemoryAI(), Enabled: c.MemoryEnabled, Log: logger("memory")}
	gw.Memory = mem
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	k, withMem, _ := c.Store.CreateAPIKey(ctx, "phone", nil, 0)
	_ = c.Store.SetAPIKeyMemory(ctx, k.ID, "me")
	_, noMem, _ := c.Store.CreateAPIKey(ctx, "automation", nil, 0)
	chat := func(key, text string, header ...string) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"model": "qwen2.5:0.5b", "messages": []map[string]string{{"role": "user", "content": text}}})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if len(header) == 2 {
			req.Header.Set(header[0], header[1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("chat: %s", resp.Status)
		}
		return string(fake(s).LastChat())
	}

	chat(withMem, "Remember that my dog is called Rex")
	all, _ := c.Store.ListMemories(ctx, "me")
	if len(all) != 1 || !strings.Contains(all[0].Text, "Rex") {
		t.Fatalf("memories %+v", all)
	}
	if _, err := mem.Add(ctx, "me", store.MemoryInstruction, "Answer in French.", "you", false); err != nil {
		t.Fatal(err)
	}
	sent := chat(withMem, "What should I feed my dog Rex tonight?")
	if !strings.Contains(sent, "dog is called Rex") || !strings.Contains(sent, "Answer in French.") || !strings.Contains(sent, `"role":"system"`) {
		t.Fatalf("no memory in the request: %s", sent)
	}
	if sent := chat(withMem, "What should I feed my dog Rex tonight?", "X-AICluster-Memory", "off"); strings.Contains(sent, "Rex.") || strings.Contains(sent, "French") {
		t.Fatalf("memory despite opting out: %s", sent)
	}
	if sent := chat(noMem, "What should I feed my dog Rex tonight?"); strings.Contains(sent, "French") {
		t.Fatalf("memory for a key without any: %s", sent)
	}
	waitFor(t, 5*time.Second, "the reply to count as using the memories", func() bool {
		m, _ := c.Store.ListMemories(ctx, "me")
		return len(m) == 2 && m[0].Uses > 0 && m[1].Uses > 0
	})
}

// The Controller backs memory up to a node, restores it by itself when its
// own copy goes missing, and only offers a backup from another cluster.
func TestMemoryBackupAndRestore(t *testing.T) {
	c := startController(t, core.ModeAuto, func(cfg *core.Config) { cfg.MemoryBackupEvery = 200 * time.Millisecond })
	s := startSlave(t, c)
	adoptAll(t, c, s)
	ctx := context.Background()
	mem := &memory.Service{Store: c.Store}
	mem.Add(ctx, "me", store.MemoryFact, "The user has a border collie called Rex.", "you", false)
	mem.Add(ctx, "me", store.MemoryInstruction, "Keep answers short.", "you", false)
	gen, _ := c.Store.MemoryGeneration(ctx)

	waitFor(t, 20*time.Second, "a backup on the node", func() bool {
		b := c.MemoryBackup(ctx)
		return b.NodeID == s.NodeID() && b.Generation == gen && b.Count == 2 && b.Error == ""
	})

	// The Controller loses its memory (a database restored from an old copy).
	if err := c.Store.ReplaceMemories(ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the memory to be restored", func() bool {
		all, _ := c.Store.ListMemories(ctx, "me")
		n := c.MemoryNoticeNow(ctx)
		return len(all) == 2 && n != nil && n.Kind == "restored" && n.Before == 0 && n.NodeID == s.NodeID()
	})
	if g, _ := c.Store.MemoryGeneration(ctx); g != gen {
		t.Fatalf("generation after restore %d, want %d", g, gen)
	}
	c.DismissMemoryNotice(ctx)
	if c.MemoryNoticeNow(ctx) != nil {
		t.Fatal("notice not dismissed")
	}

	// A newer backup from another cluster (after reinstalling a Controller):
	// offered, not restored by itself.
	writeForeignBackup(t, filepath.Join(s.cfg.DataDir, "memory-backup"))
	if err := c.Store.ReplaceMemories(ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the foreign backup to be offered", func() bool {
		n := c.MemoryNoticeNow(ctx)
		return n != nil && n.Kind == "found" && n.ClusterID == "other-cluster" && n.Count == 1
	})
	if all, _ := c.Store.ListMemories(ctx, ""); len(all) != 0 {
		t.Fatal("a foreign backup was restored without asking")
	}
	n, err := c.RestoreMemoryFrom(ctx, s.NodeID())
	if err != nil || n.Kind != "restored" {
		t.Fatalf("restore: %+v %v", n, err)
	}
	all, _ := c.Store.ListMemories(ctx, "")
	if len(all) != 1 || all[0].Text != "The user has a cat called Miso." {
		t.Fatalf("restored %+v", all)
	}
	if g, _ := c.Store.MemoryGeneration(ctx); g <= 99 {
		t.Fatalf("generation %d should continue past the backup's 99", g)
	}
}

func writeForeignBackup(t *testing.T, dir string) {
	t.Helper()
	now := time.Now().UTC().Add(time.Hour)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_ = json.NewEncoder(zw).Encode(map[string]any{"v": 1, "cluster_id": "other-cluster", "generation": 99, "saved_at": now,
		"memories": []map[string]any{{"id": "x1", "space": "me", "kind": "fact", "text": "The user has a cat called Miso.",
			"tier": "hot", "created_at": now, "updated_at": now}}})
	_ = zw.Close()
	sum := sha256.Sum256(buf.Bytes())
	name := "memory-foreign.json.gz"
	info, _ := json.Marshal(map[string]any{"file": name, "cluster_id": "other-cluster", "generation": 99, "count": 1,
		"sha256": hex.EncodeToString(sum[:]), "size": buf.Len(), "saved_at": now})
	_ = os.MkdirAll(dir, 0o700)
	if os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600) != nil || os.WriteFile(filepath.Join(dir, name+".info"), info, 0o600) != nil {
		t.Fatal("write foreign backup")
	}
}
