// Package provision is the Go client for the Python provisioner
// (controller/provisioner): catalog reads and fit/placement questions.
package provision

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"aicluster/common/httpx"
)

type Quant struct {
	Quant   string `json:"quant"`
	Runtime string `json:"runtime"`
	Size    int64  `json:"size"`
	Ref     string `json:"ref,omitempty"`
}

type Variant struct {
	ID         string         `json:"id"`
	ParamsB    float64        `json:"params_b"`
	CtxDefault int            `json:"ctx_default"`
	Arch       map[string]int `json:"arch,omitempty"`
	Quants     []Quant        `json:"quants"`
}

type Family struct {
	ID           string            `json:"id"`
	Family       string            `json:"family"`
	Description  string            `json:"description"`
	License      map[string]any    `json:"license"`
	Capabilities []string          `json:"capabilities"`
	Priority     int               `json:"priority"`
	Aliases      map[string]string `json:"aliases"`
	Variants     []Variant         `json:"variants"`
}

type Catalog struct {
	Version  string   `json:"version"`
	Error    string   `json:"error,omitempty"`
	Families []Family `json:"families"`
}

// Variant looks up a model by id.
func (c *Catalog) Variant(id string) (*Family, *Variant) {
	for i := range c.Families {
		for j := range c.Families[i].Variants {
			if c.Families[i].Variants[j].ID == id {
				return &c.Families[i], &c.Families[i].Variants[j]
			}
		}
	}
	return nil, nil
}

type GPU struct {
	VRAM int64 `json:"vram"`
}

type Committed struct {
	ID         string `json:"id"`
	VRAM       int64  `json:"vram"`
	RAM        int64  `json:"ram"`
	Residency  string `json:"residency"`
	Size       int64  `json:"size"`
	Downloaded bool   `json:"downloaded"`
}

type FitNode struct {
	NodeID    string      `json:"node_id"`
	GPUs      []GPU       `json:"gpus"`
	RAMTotal  int64       `json:"ram_total"`
	DiskFree  int64       `json:"disk_free"`
	Runtimes  []string    `json:"runtimes"`
	Committed []Committed `json:"committed"`
}

type FitRequest struct {
	Nodes   []FitNode `json:"nodes"`
	Models  []string  `json:"models,omitempty"`
	Quant   string    `json:"quant,omitempty"`
	Exclude []string  `json:"exclude,omitempty"`
}

type FitOption struct {
	Quant     string `json:"quant"`
	Runtime   string `json:"runtime"`
	Ref       string `json:"ref"`
	Size      int64  `json:"size"`
	Need      int64  `json:"need"`
	Fits      bool   `json:"fits"`
	Placement string `json:"placement"`
	VRAM      int64  `json:"vram"`
	RAM       int64  `json:"ram"`
	Reason    string `json:"reason"`
}

type FitResult struct {
	ModelID   string      `json:"model_id"`
	NodeID    string      `json:"node_id"`
	Fits      bool        `json:"fits"`
	Quant     string      `json:"quant,omitempty"`
	Runtime   string      `json:"runtime,omitempty"`
	Ref       string      `json:"ref,omitempty"`
	Size      int64       `json:"size,omitempty"`
	Need      int64       `json:"need,omitempty"`
	Placement string      `json:"placement,omitempty"`
	VRAM      int64       `json:"vram,omitempty"`
	RAM       int64       `json:"ram,omitempty"`
	Reason    string      `json:"reason"`
	Options   []FitOption `json:"options"`
}

type FitResponse struct {
	CatalogVersion string      `json:"catalog_version"`
	Results        []FitResult `json:"results"`
}

type Client struct {
	http *http.Client
	base string

	mu     sync.Mutex
	cat    *Catalog
	catAt  time.Time
	catTTL time.Duration
}

// New accepts "unix:/run/aicluster/provisioner.sock" or "tcp:127.0.0.1:7460".
func New(addr string) (*Client, error) {
	tr := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	switch {
	case strings.HasPrefix(addr, "unix:"):
		path := strings.TrimPrefix(addr, "unix:")
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	case strings.HasPrefix(addr, "tcp:"):
	default:
		return nil, fmt.Errorf("provisioner address must start with unix: or tcp: (got %q)", addr)
	}
	host := "provisioner"
	if h, ok := strings.CutPrefix(addr, "tcp:"); ok {
		host = h
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 15 * time.Second}, base: "http://" + host, catTTL: 5 * time.Second}, nil
}

// Catalog returns the catalog, cached briefly (the dashboard polls it).
func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	c.mu.Lock()
	if c.cat != nil && time.Since(c.catAt) < c.catTTL {
		cat := c.cat
		c.mu.Unlock()
		return cat, nil
	}
	c.mu.Unlock()
	var cat Catalog
	if err := httpx.Do(ctx, c.http, http.MethodGet, c.base+"/catalog", nil, &cat); err != nil {
		return nil, fmt.Errorf("provisioner: %w", err)
	}
	c.mu.Lock()
	c.cat, c.catAt = &cat, time.Now()
	c.mu.Unlock()
	return &cat, nil
}

func (c *Client) Fit(ctx context.Context, req FitRequest) (*FitResponse, error) {
	var resp FitResponse
	if err := httpx.Do(ctx, c.http, http.MethodPost, c.base+"/fit", req, &resp); err != nil {
		return nil, fmt.Errorf("provisioner: %w", err)
	}
	return &resp, nil
}
