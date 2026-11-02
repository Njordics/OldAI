package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aicluster/controller/store"
)

// fakeAI: no embedding model unless vectors are given; Complete returns a
// canned answer.
type fakeAI struct {
	reply  string
	vec    map[string][]float32
	prompt string
}

func (f *fakeAI) Embed(_ context.Context, texts []string) ([][]float32, string, error) {
	if f.vec == nil {
		return nil, "", errors.New("no embedding model")
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := f.vec[t]
		if !ok {
			v = []float32{0, 0, 1}
		}
		out[i] = v
	}
	return out, "embed-test", nil
}

func (f *fakeAI) Complete(_ context.Context, prompt string, _ int) (string, error) {
	f.prompt = prompt
	return f.reply, nil
}

func newService(t *testing.T, ai AI) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Service{Store: st, AI: ai}
}

func TestRecallByKeywords(t *testing.T) {
	s := newService(t, &fakeAI{})
	ctx := context.Background()
	s.Add(ctx, "me", store.MemoryInstruction, "Keep answers short.", "you", false)
	s.Add(ctx, "me", store.MemoryFact, "The user has a border collie called Rex.", "you", false)
	s.Add(ctx, "me", store.MemoryFact, "The user works as a nurse in Leeds.", "you", false)
	if _, err := s.Add(ctx, "me", store.MemoryFact, "the user has a border collie called rex", "learned", false); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Store.ListMemories(ctx, "me"); len(all) != 3 {
		t.Fatalf("duplicate stored: %d memories", len(all))
	}
	note, err := s.Recall(ctx, "me", "What food is best for my collie?")
	if err != nil || note == nil {
		t.Fatal(note, err)
	}
	// Hot facts are a constant reference: all of them go in.
	if !strings.Contains(note.Text, "Keep answers short.") || !strings.Contains(note.Text, "Rex") || !strings.Contains(note.Text, "nurse") {
		t.Fatalf("note:\n%s", note.Text)
	}
	if len(note.IDs) != 3 {
		t.Fatalf("ids %v", note.IDs)
	}
	// Another space knows nothing of it.
	if n, _ := s.Recall(ctx, "other", "collie"); n != nil {
		t.Fatal("memory leaked between spaces")
	}
	// Memory switched off.
	s.Enabled = func(context.Context) bool { return false }
	if n, _ := s.Recall(ctx, "me", "collie"); n != nil {
		t.Fatal("recalled while off")
	}
}

func TestColdNeedsCloserMatch(t *testing.T) {
	s := newService(t, &fakeAI{})
	ctx := context.Background()
	m, _ := s.Add(ctx, "me", store.MemoryFact, "The user's vet is Hillside Vets on Mill Lane.", "you", false)
	old := time.Now().Add(-60 * 24 * time.Hour)
	m.CreatedAt, m.UpdatedAt = old, old
	_ = s.Store.PutMemory(ctx, m)
	s.Sweep(ctx)
	if got, _ := s.Store.GetMemory(ctx, m.ID); got.Tier != store.TierCold {
		t.Fatalf("tier %s", got.Tier)
	}
	if n, _ := s.Recall(ctx, "me", "Is the vet open?"); n != nil {
		t.Fatal("a cold fact came back on one shared word")
	}
	n, _ := s.Recall(ctx, "me", "How do I get to Hillside Vets?")
	if n == nil || !strings.Contains(n.Text, "Mill Lane") {
		t.Fatal("a cold fact didn't come back on a close match")
	}
	s.Used(n.IDs)
	if got, _ := s.Store.GetMemory(ctx, m.ID); got.Tier != store.TierHot || got.Uses != 1 {
		t.Fatalf("after use: %+v", got)
	}
}

func TestRecallByEmbedding(t *testing.T) {
	ai := &fakeAI{vec: map[string][]float32{
		"The user has a border collie called Rex.": {1, 0, 0},
		"The user works as a nurse in Leeds.":      {0, 1, 0},
		"What should I feed my pet?":               {0.9, 0.1, 0},
	}}
	s := newService(t, ai)
	ctx := context.Background()
	s.Add(ctx, "me", store.MemoryFact, "The user has a border collie called Rex.", "you", false)
	s.Add(ctx, "me", store.MemoryFact, "The user works as a nurse in Leeds.", "you", false)
	// Cold, only the fact that matches by meaning comes back.
	all, _ := s.Store.ListMemories(ctx, "me")
	for _, m := range all {
		_ = s.Store.SetMemoryTier(ctx, m.ID, store.TierCold)
	}
	n, _ := s.Recall(ctx, "me", "What should I feed my pet?")
	if n == nil || !strings.Contains(n.Text, "Rex") || strings.Contains(n.Text, "nurse") {
		t.Fatalf("note %+v", n)
	}
}

// When there are more hot facts than fit, the matching ones win.
func TestHotFactsFillTheNote(t *testing.T) {
	s := newService(t, &fakeAI{})
	ctx := context.Background()
	for i := range 40 {
		s.Add(ctx, "me", store.MemoryFact, fmt.Sprintf("The user once visited museum number %d in some town far away.", i), "you", false)
	}
	s.Add(ctx, "me", store.MemoryFact, "The user's sister Priya is a dentist.", "you", false)
	n, _ := s.Recall(ctx, "me", "Should I ask my sister Priya about my tooth?")
	if n == nil || !strings.Contains(n.Text, "Priya") || len(n.Text) > maxNoteChars || len(n.IDs) > maxFacts {
		t.Fatalf("%d ids, %d chars, priya=%v", len(n.IDs), len(n.Text), strings.Contains(n.Text, "Priya"))
	}
}

func TestCommands(t *testing.T) {
	s := newService(t, &fakeAI{})
	ctx := context.Background()
	if did := s.Command(ctx, "me", "Remember that my dog is called Rex"); did != "remembered" {
		t.Fatalf("remember: %q", did)
	}
	all, _ := s.Store.ListMemories(ctx, "me")
	if len(all) != 1 || all[0].Text != "The user asked you to remember: my dog is called Rex." || all[0].Source != "you" {
		t.Fatalf("stored %+v", all)
	}
	if did := s.Command(ctx, "me", "What's the weather?"); did != "" {
		t.Fatalf("a question was a command: %q", did)
	}
	if did := s.Command(ctx, "me", "please forget about my dog Rex"); did != "forgot 1" {
		t.Fatalf("forget: %q", did)
	}
	if all, _ := s.Store.ListMemories(ctx, "me"); len(all) != 0 {
		t.Fatal("not forgotten")
	}
}

func TestLearn(t *testing.T) {
	ai := &fakeAI{reply: `Sure! {"add": ["The user has a cat called Miso."], "update": [{"n": 1, "text": "The user lives in York."}], "remove": []}`}
	s := newService(t, ai)
	ctx := context.Background()
	s.Add(ctx, "me", store.MemoryFact, "The user lives in Leeds.", "learned", false)
	if err := s.learn(ctx, "me", "We moved to York last month, and our cat Miso loves the new garden."); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ai.prompt, "1. The user lives in Leeds.") {
		t.Fatalf("prompt didn't list known facts:\n%s", ai.prompt)
	}
	var texts []string
	all, _ := s.Store.ListMemories(ctx, "me")
	for _, m := range all {
		texts = append(texts, m.Text)
	}
	got := strings.Join(texts, " | ")
	if !strings.Contains(got, "York") || strings.Contains(got, "Leeds") || !strings.Contains(got, "Miso") {
		t.Fatalf("memories: %s", got)
	}
	// Placeholders, the prompt's examples and facts the message doesn't
	// support are dropped.
	ai.reply = `{"add": ["...", "The user is learning Spanish.", "The user has three cats.", "The user drives a red van to work."], "update": [], "remove": []}`
	before, _ := s.Store.ListMemories(ctx, "me")
	if err := s.learn(ctx, "me", "I drive my old red van to work every morning."); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Store.ListMemories(ctx, "me")
	var now []string
	for _, m := range after {
		now = append(now, m.Text)
	}
	// Facts saved in the same second list in no particular order.
	if len(after) != len(before)+1 || !strings.Contains(strings.Join(now, " | "), "red van") {
		t.Fatalf("after filtering: %q", now)
	}
	// A reply that isn't JSON changes nothing.
	ai.reply = "I don't think there's anything to remember."
	if err := s.learn(ctx, "me", "Hello there, how are you today?"); err == nil {
		t.Fatal("accepted a non-JSON reply")
	}
	// The model may not remove what the user asked to remember.
	s.Command(ctx, "me", "remember that I'm allergic to penicillin")
	ai.reply = `{"add": [], "update": [], "remove": [1, 2, 3]}`
	_ = s.learn(ctx, "me", "allergic penicillin York Miso cat")
	all, _ = s.Store.ListMemories(ctx, "me")
	kept := false
	for _, m := range all {
		kept = kept || strings.Contains(m.Text, "penicillin")
	}
	if !kept {
		t.Fatal("learning removed a memory the user asked for")
	}
}
