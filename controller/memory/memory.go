// Package memory gives every model on the cluster a shared, lasting memory
// of the people using it (protocol §18).
//
// Memories belong to a space (usually one person), which API keys select.
// They are standing instructions ("keep answers short"), always included,
// and facts ("Has a dog called Rex."), included when relevant. Facts used
// within 30 days are hot; older ones go cold and are only brought in when a
// question clearly matches. Facts come from "remember …" in a chat, the
// dashboard, or a small model that reads each message for lasting facts.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"aicluster/controller/store"
)

// AI is what memory needs from the cluster's models. The gateway provides
// it; either call may return an error when no suitable model is installed.
type AI interface {
	// Embed returns one vector per text and the model that made them.
	Embed(ctx context.Context, texts []string) ([][]float32, string, error)
	// Complete answers a single prompt with a small, fast model.
	Complete(ctx context.Context, prompt string, maxTokens int) (string, error)
}

// Tuning.
const (
	hotFor       = 30 * 24 * time.Hour // unused this long → cold
	maxFacts     = 20                  // facts in one reply's memory note
	maxNoteChars = 1600                // about 400 tokens
	maxFactChars = 200
	maxLearnText = 2000
	minLearnText = 15
	// Similarity a fact needs to be included, by embedding or by keywords.
	hotCos, coldCos = 0.35, 0.55
	hotWords        = 1
	coldWords       = 2
	dupCos          = 0.92
)

// Service is the cluster's memory.
type Service struct {
	Store *store.Store
	AI    AI
	Log   *slog.Logger
	// Enabled reports the Settings switch; memory is off when it says so.
	Enabled func(ctx context.Context) bool

	mu       sync.Mutex
	learning map[string]bool // spaces with an extraction running
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Service) on(ctx context.Context) bool { return s.Enabled == nil || s.Enabled(ctx) }

func newID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Add stores a memory, embedding it when an embedding model is installed.
// A fact that repeats one already known isn't added twice.
func (s *Service) Add(ctx context.Context, space, kind, text, source string, pinned bool) (*store.Memory, error) {
	text = clean(text)
	if text == "" {
		return nil, fmt.Errorf("the memory is empty")
	}
	if kind != store.MemoryInstruction {
		kind = store.MemoryFact
	}
	now := time.Now().UTC()
	m := &store.Memory{ID: newID(), Space: space, Kind: kind, Text: text, Pinned: pinned, Tier: store.TierHot,
		Source: source, CreatedAt: now, UpdatedAt: now}
	if vs, model, err := s.embed(ctx, []string{text}); err == nil {
		m.Embedding, m.EmbedModel = vs[0], model
	}
	existing, err := s.Store.ListMemories(ctx, space)
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		if e.Kind == kind && (normal(e.Text) == normal(text) || cosine(e.Embedding, m.Embedding, e.EmbedModel, m.EmbedModel) >= dupCos) {
			return e, nil
		}
	}
	return m, s.Store.PutMemory(ctx, m)
}

// Update changes a memory's text, kind or pin.
func (s *Service) Update(ctx context.Context, m *store.Memory) error {
	m.Text = clean(m.Text)
	if m.Text == "" {
		return fmt.Errorf("the memory is empty")
	}
	m.UpdatedAt = time.Now().UTC()
	if vs, model, err := s.embed(ctx, []string{m.Text}); err == nil {
		m.Embedding, m.EmbedModel = vs[0], model
	}
	return s.Store.PutMemory(ctx, m)
}

func (s *Service) embed(ctx context.Context, texts []string) ([][]float32, string, error) {
	if s.AI == nil {
		return nil, "", fmt.Errorf("no models")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	vs, model, err := s.AI.Embed(ctx, texts)
	if err == nil && len(vs) != len(texts) {
		err = fmt.Errorf("the embedding model returned %d vectors for %d texts", len(vs), len(texts))
	}
	return vs, model, err
}

func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, "-•* ")
	if len(s) > maxFactChars*2 {
		s = s[:maxFactChars*2]
	}
	return s
}

func normal(s string) string {
	return strings.TrimRight(strings.ToLower(clean(s)), ".!")
}

// Run keeps tiers and embeddings up to date: shortly after start, then
// hourly.
func (s *Service) Run(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		s.Sweep(ctx)
		wait = time.Hour
	}
}

// ---------- Recall ----------

// Note is what a reply gets from memory: a system note and the memories in it.
type Note struct {
	Text string
	IDs  []string
}

// Recall picks a space's instructions and the facts relevant to query.
// Pinned memories and instructions come first in a stable order, so the
// runtime can reuse its reading of them from one message to the next.
func (s *Service) Recall(ctx context.Context, space, query string) (*Note, error) {
	if space == "" || !s.on(ctx) {
		return nil, nil
	}
	all, err := s.Store.ListMemories(ctx, space)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	var qv []float32
	var qmodel string
	needEmbed := false
	for _, m := range all {
		needEmbed = needEmbed || (m.Kind == store.MemoryFact && !m.Pinned)
	}
	if needEmbed && strings.TrimSpace(query) != "" {
		if vs, model, err := s.embed(ctx, []string{query}); err == nil {
			qv, qmodel = vs[0], model
		}
	}
	qwords := words(query)

	// Hot facts are a constant reference: all of them go in, up to the size
	// limit. Cold facts go in only on a clear match. The most relevant come
	// first when there are too many for the note.
	var always, facts []*store.Memory
	score := map[string]float64{}
	for _, m := range all {
		if m.Kind == store.MemoryInstruction || m.Pinned {
			always = append(always, m)
			continue
		}
		hot := m.Tier != store.TierCold
		sc, match := 0.0, false
		if c := cosine(qv, m.Embedding, qmodel, m.EmbedModel); c > -1 {
			sc, match = c, (hot && c >= hotCos) || c >= coldCos
		} else {
			// No embeddings: shared keywords, scaled to compare with cosines.
			n := overlap(qwords, words(m.Text))
			sc, match = float64(n)/4, (hot && n >= hotWords) || n >= coldWords
		}
		if !match && !hot {
			continue
		}
		if match {
			sc++ // matching facts before the rest of the hot ones
		}
		score[m.ID] = sc
		facts = append(facts, m)
	}
	sort.SliceStable(facts, func(i, j int) bool {
		if score[facts[i].ID] != score[facts[j].ID] {
			return score[facts[i].ID] > score[facts[j].ID]
		}
		return facts[i].LastUsedAt.After(facts[j].LastUsedAt)
	})
	// Keep what fits, then put it in a stable order (by age, not score).
	budget := maxNoteChars - 300
	for _, m := range always {
		budget -= len(m.Text) + 3
	}
	kept := facts[:0]
	for _, m := range facts {
		if len(kept) >= maxFacts || budget-len(m.Text)-3 < 0 {
			continue
		}
		budget -= len(m.Text) + 3
		kept = append(kept, m)
	}
	facts = kept
	sort.SliceStable(facts, func(i, j int) bool { return facts[i].CreatedAt.Before(facts[j].CreatedAt) })

	var instr, known []*store.Memory
	for _, m := range append(always, facts...) {
		if m.Kind == store.MemoryInstruction {
			instr = append(instr, m)
		} else {
			known = append(known, m)
		}
	}
	if len(instr)+len(known) == 0 {
		return nil, nil
	}
	var b strings.Builder
	note := &Note{}
	b.WriteString("You remember the following about the user you are talking to, from earlier conversations with them. " +
		"Use it naturally whenever it is relevant, including when they ask about these things. Don't list it unprompted.")
	add := func(head string, ms []*store.Memory) {
		if len(ms) == 0 {
			return
		}
		b.WriteString("\n\n" + head)
		for _, m := range ms {
			line := "\n- " + m.Text
			if b.Len()+len(line) > maxNoteChars {
				return
			}
			b.WriteString(line)
			note.IDs = append(note.IDs, m.ID)
		}
	}
	add("The user's instructions:", instr)
	add("What you remember about the user:", known)
	note.Text = b.String()
	return note, nil
}

// Used records that the memories went into a reply, which keeps them hot.
func (s *Service) Used(ids []string) {
	if len(ids) == 0 {
		return
	}
	if err := s.Store.TouchMemories(context.Background(), time.Now().UTC(), ids...); err != nil {
		s.log().Warn("memory: record use", "err", err)
	}
}

// Sweep moves facts unused for 30 days to cold, and embeds memories that
// have no vector yet (or one from another model).
func (s *Service) Sweep(ctx context.Context) {
	all, err := s.Store.ListMemories(ctx, "")
	if err != nil {
		return
	}
	now := time.Now()
	var missing []*store.Memory
	for _, m := range all {
		last := m.LastUsedAt
		if m.CreatedAt.After(last) {
			last = m.CreatedAt
		}
		tier := store.TierHot
		if m.Kind == store.MemoryFact && !m.Pinned && now.Sub(last) > hotFor {
			tier = store.TierCold
		}
		if tier != m.Tier {
			_ = s.Store.SetMemoryTier(ctx, m.ID, tier)
		}
		if m.Kind == store.MemoryFact {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return
	}
	// One probe tells which model embeds now; only stale vectors are redone.
	_, model, err := s.embed(ctx, []string{"probe"})
	if err != nil {
		return
	}
	var redo []*store.Memory
	for _, m := range missing {
		if len(m.Embedding) == 0 || m.EmbedModel != model {
			redo = append(redo, m)
		}
	}
	for i := 0; i < len(redo); i += 32 {
		batch := redo[i:min(i+32, len(redo))]
		texts := make([]string, len(batch))
		for j, m := range batch {
			texts[j] = m.Text
		}
		vs, model, err := s.embed(ctx, texts)
		if err != nil {
			return
		}
		for j, m := range batch {
			_ = s.Store.SetMemoryEmbedding(ctx, m.ID, model, vs[j])
		}
	}
}

func cosine(a, b []float32, ma, mb string) float64 {
	if len(a) == 0 || len(a) != len(b) || ma != mb {
		return -2 // not comparable
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}']+`)

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and or but if of to in on at for with from by about as is are was were be been being
		i me my mine you your yours he him his she her it its we us our they them their this that these those what which who whom
		do does did done have has had will would can could should shall may might must not no yes so than then there here when
		where why how all any some more most very just also too only own same such into over under again once up down out off
		tell know want like get got make made go going one two please thanks thank hi hello hey okay ok im i'm it's dont don't`) {
		stop[w] = true
	}
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		w = strings.Trim(w, "'")
		if len(w) < 3 || stop[w] {
			continue
		}
		out[strings.TrimSuffix(w, "s")] = true
	}
	return out
}

func overlap(a, b map[string]bool) int {
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return n
}

// ---------- Learning ----------

var (
	rememberRe = regexp.MustCompile(`(?i)^\s*(?:please\s+)?remember(?:\s+that)?[:,]?\s+(.{3,})$`)
	forgetRe   = regexp.MustCompile(`(?i)^\s*(?:please\s+)?forget(?:\s+(?:that|about))?[:,]?\s+(.{3,})$`)
)

// Command handles "remember …" and "forget …" at once, before the model
// answers. It reports what it did, for the log.
func (s *Service) Command(ctx context.Context, space, text string) string {
	if space == "" || !s.on(ctx) {
		return ""
	}
	text = strings.TrimSpace(text)
	if m := rememberRe.FindStringSubmatch(text); m != nil {
		if _, err := s.Add(ctx, space, store.MemoryFact, firstPerson(m[1]), "you", false); err == nil {
			return "remembered"
		}
		return ""
	}
	if m := forgetRe.FindStringSubmatch(text); m != nil {
		n := s.forget(ctx, space, m[1])
		if n > 0 {
			return fmt.Sprintf("forgot %d", n)
		}
	}
	return ""
}

// firstPerson keeps the user's own words, framed so a model reads them as
// coming from the user ("my dog" stays the user's dog, not the model's).
func firstPerson(s string) string {
	s = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(s), ".!"))
	return "The user asked you to remember: " + s + "."
}

func (s *Service) forget(ctx context.Context, space, what string) int {
	all, err := s.Store.ListMemories(ctx, space)
	if err != nil {
		return 0
	}
	var qv []float32
	var qmodel string
	if vs, model, err := s.embed(ctx, []string{what}); err == nil {
		qv, qmodel = vs[0], model
	}
	qw := words(what)
	var ids []string
	for _, m := range all {
		if m.Kind != store.MemoryFact {
			continue
		}
		c := cosine(qv, m.Embedding, qmodel, m.EmbedModel)
		if c >= 0.6 || (c == -2 && len(qw) > 0 && overlap(qw, words(m.Text)) >= max(1, len(qw)*2/3)) {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return 0
	}
	if err := s.Store.DeleteMemories(ctx, ids...); err != nil {
		return 0
	}
	return len(ids)
}

// Learn reads a user's message for lasting facts in the background. One
// message per space is read at a time; others are skipped while it runs.
func (s *Service) Learn(space, text string) {
	text = strings.TrimSpace(text)
	if space == "" || s.AI == nil || len(text) < minLearnText || strings.Contains(text, "```") ||
		rememberRe.MatchString(text) || forgetRe.MatchString(text) {
		return
	}
	if len(text) > maxLearnText {
		text = text[:maxLearnText]
	}
	s.mu.Lock()
	if s.learning == nil {
		s.learning = map[string]bool{}
	}
	if s.learning[space] {
		s.mu.Unlock()
		return
	}
	s.learning[space] = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.learning, space)
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if !s.on(ctx) {
			return
		}
		if err := s.learn(ctx, space, text); err != nil {
			s.log().Debug("memory: learning skipped", "space", space, "err", err)
		}
	}()
}

const learnPrompt = `You keep a short memory of lasting facts about a user, to personalise future answers.

Facts already known:
%s
The user just wrote:
"""
%s
"""

Reply with JSON only. For example, if the user wrote "I'm learning Spanish for a trip to Peru" and fact 1 said they live in Madrid but they now write that they moved to Lima, the reply would be:
{"add": ["The user is learning Spanish.", "The user is planning a trip to Peru."], "update": [{"n": 1, "text": "The user lives in Lima."}], "remove": []}

Rules:
- Only lasting facts about the user that the message above states: their life, family, pets, work, projects, preferences, plans. Not questions, requests, passing remarks, or general knowledge.
- Each fact is one short sentence about "the user".
- Use "update" when a known fact has changed, "remove" when the user says it is no longer true.
- If the message states nothing lasting about the user, reply {"add": [], "update": [], "remove": []}.`

// exampleFacts are the prompt's examples, which a model must not return as
// facts about this user.
var exampleFacts = map[string]bool{
	"the user is learning spanish": true, "the user is planning a trip to peru": true, "the user lives in lima": true,
}

type learnReply struct {
	Add    []string `json:"add"`
	Update []struct {
		N    int    `json:"n"`
		Text string `json:"text"`
	} `json:"update"`
	Remove []int `json:"remove"`
}

func (s *Service) learn(ctx context.Context, space, text string) error {
	all, err := s.Store.ListMemories(ctx, space)
	if err != nil {
		return err
	}
	// Show the model the facts most likely to change.
	var facts []*store.Memory
	qw := words(text)
	for _, m := range all {
		if m.Kind == store.MemoryFact {
			facts = append(facts, m)
		}
	}
	sort.SliceStable(facts, func(i, j int) bool {
		return overlap(qw, words(facts[i].Text)) > overlap(qw, words(facts[j].Text))
	})
	if len(facts) > 15 {
		facts = facts[:15]
	}
	var known strings.Builder
	for i, m := range facts {
		fmt.Fprintf(&known, "%d. %s\n", i+1, m.Text)
	}
	if known.Len() == 0 {
		known.WriteString("(none)\n")
	}
	out, err := s.AI.Complete(ctx, fmt.Sprintf(learnPrompt, known.String(), text), 300)
	if err != nil {
		return err
	}
	var r learnReply
	start, end := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if start < 0 || end <= start || json.Unmarshal([]byte(out[start:end+1]), &r) != nil {
		return fmt.Errorf("the model's reply wasn't JSON: %.100s", out)
	}
	// Small models copy examples and placeholders, or invent things: a fact
	// must share a meaningful word with what the user wrote.
	said := words(text)
	grounded := func(f string) bool {
		f = clean(f)
		if len(f) < 8 || len(f) > maxFactChars || exampleFacts[normal(f)] {
			return false
		}
		w := words(f)
		delete(w, "user")
		return overlap(w, said) > 0
	}
	changed := 0
	for _, n := range r.Remove {
		if n >= 1 && n <= len(facts) && facts[n-1].Source != "you" {
			if s.Store.DeleteMemories(ctx, facts[n-1].ID) == nil {
				changed++
			}
		}
	}
	for _, u := range r.Update {
		if u.N >= 1 && u.N <= len(facts) && grounded(u.Text) {
			m := *facts[u.N-1]
			m.Text, m.Source = u.Text, "learned"
			if s.Update(ctx, &m) == nil {
				changed++
			}
		}
	}
	for i, t := range r.Add {
		if i >= 5 {
			break
		}
		if !grounded(t) {
			continue
		}
		if _, err := s.Add(ctx, space, store.MemoryFact, t, "learned", false); err == nil {
			changed++
		}
	}
	if changed > 0 {
		s.log().Info("memory: learned from a message", "space", space, "changes", changed)
	}
	return nil
}
