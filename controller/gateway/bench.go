package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"aicluster/common/proto"
	"aicluster/controller/core"
)

// Benchmarks (protocol §17). A benchmark sends a fixed-size prompt to one
// model on one node through the same path real requests take, streams the
// reply and measures how fast the node reads the prompt and writes the
// answer. Targets run one after another so they don't slow each other
// down, and the node counts as busy meanwhile, so the gateway sends real
// requests elsewhere when it can.

// BenchTarget is one model on one node.
type BenchTarget struct {
	ModelID string `json:"model_id"`
	NodeID  string `json:"node_id"`
}

// BenchResult is one target's measurement.
type BenchResult struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	ModelID  string    `json:"model_id"`
	NodeID   string    `json:"node_id"`
	NodeName string    `json:"node_name"`
	// Pool lists the other nodes of a model split across nodes.
	Pool     []string `json:"pool,omitempty"`
	Parallel int      `json:"parallel"`
	// ColdStartS is how long the model took to load and answer a first,
	// tiny request; 0 when it was already in memory.
	ColdStartS float64 `json:"cold_start_s,omitempty"`
	// TTFTms is the time to the first token of the measured request: mostly
	// reading the prompt.
	TTFTms       float64 `json:"ttft_ms"`
	PromptTokens int     `json:"prompt_tokens"`
	PromptTPS    float64 `json:"prompt_tps"` // prompt tokens read per second
	GenTokens    int     `json:"gen_tokens"`
	GenTPS       float64 `json:"gen_tps"` // tokens written per second, per request
	// TotalTPS is tokens written per second by all parallel requests together.
	TotalTPS float64 `json:"total_tps,omitempty"`
	CPUTempC float64 `json:"cpu_temp_c,omitempty"` // right after the run
	Hot      bool    `json:"hot,omitempty"`        // slowing down for heat
	// Approx: the runtime didn't report token counts, so they are estimated.
	Approx bool   `json:"approx,omitempty"`
	Error  string `json:"error,omitempty"`
}

// BenchJob is a run of one or more targets.
type BenchJob struct {
	ID        string        `json:"id"`
	Targets   []BenchTarget `json:"targets"`
	Parallel  int           `json:"parallel"` // 0 = the node's slots
	Done      int           `json:"done"`
	Current   string        `json:"current,omitempty"` // what is running now
	Running   bool          `json:"running"`
	StartedAt time.Time     `json:"started_at"`
	Results   []BenchResult `json:"results"`
}

const (
	benchGenTokens  = 128
	benchTimeout    = 5 * time.Minute // per target, including a cold load
	settingBench    = "benchmarks"
	benchHistoryMax = 200
)

// benchText is about 400 words of plain prose: long enough that reading it
// takes measurable time, short enough for any context window.
var benchText = strings.Repeat("The harbour town woke slowly. Fishing boats came in with the tide, gulls followed them "+
	"over the grey water, and the market filled with crates of fish, bread, apples and rope. An old clockmaker opened "+
	"his shop on the corner, wound every clock on the wall, and waited for the first customer of the day. ", 8)

var ErrBenchRunning = errors.New("a benchmark is already running")

type benchState struct {
	mu  sync.Mutex
	job *BenchJob
}

// BenchJobNow is the running or last job, if any.
func (g *Gateway) BenchJobNow() *BenchJob {
	g.bench.mu.Lock()
	defer g.bench.mu.Unlock()
	if g.bench.job == nil {
		return nil
	}
	j := *g.bench.job
	j.Results = append([]BenchResult(nil), j.Results...)
	return &j
}

// BenchTargets lists what can be benchmarked: every model on every online
// node that serves it.
func (g *Gateway) BenchTargets(ctx context.Context) ([]core.Replica, error) {
	snap, err := g.snapshotNow(ctx)
	if err != nil {
		return nil, err
	}
	var out []core.Replica
	for _, reps := range snap.replicas {
		out = append(out, reps...)
	}
	return out, nil
}

// StartBench runs targets in the background. parallel is the number of
// requests at once; 0 means as many as each node serves.
func (g *Gateway) StartBench(targets []BenchTarget, parallel int) (*BenchJob, error) {
	if len(targets) == 0 {
		return nil, errors.New("pick at least one model on a node")
	}
	g.bench.mu.Lock()
	if g.bench.job != nil && g.bench.job.Running {
		g.bench.mu.Unlock()
		return nil, ErrBenchRunning
	}
	job := &BenchJob{ID: newID(), Targets: targets, Parallel: parallel, Running: true, StartedAt: time.Now().UTC()}
	g.bench.job = job
	g.bench.mu.Unlock()
	go g.runBench(job)
	return g.BenchJobNow(), nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (g *Gateway) runBench(job *BenchJob) {
	for i, t := range job.Targets {
		g.bench.mu.Lock()
		job.Current = t.ModelID + " on " + g.nodeName(t.NodeID)
		g.bench.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), benchTimeout)
		res := g.benchOne(ctx, t, job.Parallel)
		cancel()
		g.log.Info("benchmark", "model", res.ModelID, "node", res.NodeName, "parallel", res.Parallel,
			"prompt_tps", int(res.PromptTPS), "gen_tps", fmt.Sprintf("%.1f", res.GenTPS), "err", res.Error)
		g.saveBench(res)
		g.bench.mu.Lock()
		job.Results = append(job.Results, res)
		job.Done = i + 1
		g.bench.mu.Unlock()
	}
	g.bench.mu.Lock()
	job.Running, job.Current = false, ""
	g.bench.mu.Unlock()
}

func (g *Gateway) nodeName(id string) string {
	if n, err := g.core.Store.GetNode(context.Background(), id); err == nil {
		return n.Name
	}
	return id
}

func (g *Gateway) benchOne(ctx context.Context, t BenchTarget, parallel int) BenchResult {
	res := BenchResult{ID: newID(), At: time.Now().UTC(), ModelID: t.ModelID, NodeID: t.NodeID, NodeName: g.nodeName(t.NodeID)}
	// A fresh view: the model may have just loaded or moved.
	g.mu.Lock()
	g.snap = nil
	g.mu.Unlock()
	snap, err := g.snapshotNow(ctx)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	var rep *core.Replica
	for i := range snap.replicas[t.ModelID] {
		if r := &snap.replicas[t.ModelID][i]; r.NodeID == t.NodeID {
			rep = r
		}
	}
	if rep == nil {
		res.Error = "the model isn't ready on this node, or the node is offline"
		return res
	}
	res.Pool = rep.Pool
	if parallel <= 0 {
		parallel = max(rep.Slots, 1)
	}
	res.Parallel = parallel

	// Count the benchmark as load, so real requests prefer other nodes.
	g.mu.Lock()
	g.inflight[rep.NodeID] += parallel
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inflight[rep.NodeID] -= parallel
		g.mu.Unlock()
	}()

	if !rep.Loaded {
		start := time.Now()
		if _, err := g.benchRequest(ctx, rep, "Say OK.", 1); err != nil {
			res.Error = "loading the model: " + err.Error()
			return res
		}
		res.ColdStartS = time.Since(start).Seconds()
	}

	runs := make([]streamStats, parallel)
	errs := make([]error, parallel)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A fresh prefix each time, so no prompt cache skips the reading.
			prompt := "Run " + newID() + ". " + benchText + "\nContinue this story for several paragraphs."
			runs[i], errs[i] = g.benchRequest(ctx, rep, prompt, benchGenTokens)
		}()
	}
	wg.Wait()
	var first, last time.Time
	var promptTPS, genTPS, ttft float64
	ok := 0
	for i, s := range runs {
		if errs[i] != nil {
			res.Error = errs[i].Error()
			continue
		}
		ok++
		ttft += s.ttft.Seconds() * 1000
		if s.ttft > 0 {
			promptTPS += float64(s.promptTokens) / s.ttft.Seconds()
		}
		if d := s.last.Sub(s.firstToken); d > 0 && s.genTokens > 1 {
			genTPS += float64(s.genTokens-1) / d.Seconds()
		}
		res.PromptTokens, res.GenTokens = s.promptTokens, res.GenTokens+s.genTokens
		res.Approx = res.Approx || s.approx
		if first.IsZero() || s.firstToken.Before(first) {
			first = s.firstToken
		}
		if s.last.After(last) {
			last = s.last
		}
	}
	if ok == 0 {
		return res
	}
	res.Error = ""
	res.TTFTms, res.PromptTPS, res.GenTPS = ttft/float64(ok), promptTPS/float64(ok), genTPS/float64(ok)
	if parallel > 1 && last.After(first) {
		res.TotalTPS = float64(res.GenTokens) / last.Sub(first).Seconds()
	}
	res.GenTokens /= ok
	if n, err := g.core.Store.GetNode(ctx, rep.NodeID); err == nil {
		var h proto.Health
		if json.Unmarshal(n.Health, &h) == nil && h.Thermal != nil {
			res.CPUTempC, res.Hot = h.Thermal.CPUTempC, h.Thermal.Throttled
		}
	}
	return res
}

type streamStats struct {
	ttft         time.Duration // request sent → first token
	firstToken   time.Time
	last         time.Time
	promptTokens int
	genTokens    int
	approx       bool
}

// benchRequest streams one chat completion and times it.
func (g *Gateway) benchRequest(ctx context.Context, rep *core.Replica, prompt string, maxTokens int) (streamStats, error) {
	var s streamStats
	body, _ := json.Marshal(map[string]any{
		"model":          rep.LocalName,
		"messages":       []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":     maxTokens,
		"temperature":    0,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	})
	start := time.Now()
	resp, err := g.forward(ctx, rep, "/v1/chat/completions", body)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return s, fmt.Errorf("the node answered %s: %.200s", resp.Status, strings.TrimSpace(b.String()))
	}
	chunks := 0
	br := bufio.NewReaderSize(resp.Body, 16<<10)
	for {
		line, err := br.ReadBytes('\n')
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			data = bytes.TrimSpace(data)
			if bytes.Equal(data, []byte("[DONE]")) {
				break
			}
			var ev struct {
				Choices []struct {
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
						Reasoning        string `json:"reasoning"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &ev) == nil {
				now := time.Now()
				for _, c := range ev.Choices {
					if c.Delta.Content != "" || c.Delta.ReasoningContent != "" || c.Delta.Reasoning != "" {
						if chunks == 0 {
							s.firstToken, s.ttft = now, now.Sub(start)
						}
						chunks++
						s.last = now
					}
				}
				if ev.Usage != nil {
					s.promptTokens, s.genTokens = ev.Usage.PromptTokens, ev.Usage.CompletionTokens
				}
			}
		}
		if err != nil {
			break
		}
	}
	if chunks == 0 {
		return s, errors.New("the model returned no text")
	}
	if s.genTokens == 0 {
		// No usage report: count streamed chunks, and words for the prompt.
		s.genTokens, s.promptTokens, s.approx = chunks, len(strings.Fields(prompt))*4/3, true
	}
	return s, nil
}

// saveBench keeps the newest results in the store.
func (g *Gateway) saveBench(r BenchResult) {
	ctx := context.Background()
	all := g.BenchHistory(ctx)
	all = append([]BenchResult{r}, all...)
	if len(all) > benchHistoryMax {
		all = all[:benchHistoryMax]
	}
	b, _ := json.Marshal(all)
	if err := g.core.Store.SetSetting(ctx, settingBench, string(b)); err != nil {
		g.log.Error("save benchmark", "err", err)
	}
}

// BenchHistory is every saved result, newest first.
func (g *Gateway) BenchHistory(ctx context.Context) []BenchResult {
	raw, _ := g.core.Store.Setting(ctx, settingBench, "")
	var all []BenchResult
	_ = json.Unmarshal([]byte(raw), &all)
	return all
}

// ClearBench forgets every saved result.
func (g *Gateway) ClearBench(ctx context.Context) error {
	return g.core.Store.SetSetting(ctx, settingBench, "")
}
