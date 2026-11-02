package core

import (
	"strings"
	"testing"
)

func TestLoginCode(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c := NewLoginCode()
		if len(c) != 8 || strings.Trim(c, loginAlphabet) != "" {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 199 {
		t.Fatalf("only %d distinct codes in 200", len(seen))
	}
	if FormatLoginCode("K7QM4XPA") != "K7QM-4XPA" {
		t.Fatal(FormatLoginCode("K7QM4XPA"))
	}
	for typed, want := range map[string]string{
		"K7QM-4XPA":  "K7QM4XPA",
		"k7qm 4xpa":  "K7QM4XPA",
		" k7qm4xpa ": "K7QM4XPA",
		"oIlU-0000":  "011V0000", // look-alikes read as what the screen shows
	} {
		if got := NormalizeLoginCode(typed); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", typed, got, want)
		}
	}
}
