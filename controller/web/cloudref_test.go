package web

import (
	"encoding/json"
	"testing"
	"time"
)

// The Benchmark page's cloud figures ship with the dashboard; a broken edit
// would quietly hide them.
func TestCloudReference(t *testing.T) {
	b, err := uiFiles.ReadFile("ui/cloud-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Src    string `json:"source"`
		Link   string `json:"url"`
		Date   string `json:"as_of"`
		Models []struct {
			Service string  `json:"service"`
			Model   string  `json:"model"`
			GenTPS  float64 `json:"gen_tps"`
			TTFT    float64 `json:"ttft_s"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatal(err)
	}
	if ref.Src == "" || ref.Link == "" {
		t.Fatal("source missing")
	}
	if _, err := time.Parse("2006-01-02", ref.Date); err != nil {
		t.Fatalf("as_of: %v", err)
	}
	services := map[string]bool{}
	for _, m := range ref.Models {
		if m.Service == "" || m.Model == "" || m.GenTPS <= 0 || m.TTFT <= 0 {
			t.Fatalf("bad entry %+v", m)
		}
		services[m.Service] = true
	}
	for _, s := range []string{"ChatGPT", "Claude", "Grok"} {
		if !services[s] {
			t.Errorf("no %s model", s)
		}
	}
}
