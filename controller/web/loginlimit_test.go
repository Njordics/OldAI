package web

import (
	"strconv"
	"testing"
	"time"
)

func TestLoginLimiterPerAddress(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	for i := range 5 {
		if w := l.allow("a", true); w != 0 {
			t.Fatalf("attempt %d throttled: %v", i+1, w)
		}
		l.failed("a", true)
	}
	if w := l.allow("a", true); w != time.Minute {
		t.Fatalf("after 5 failures wait = %v, want 1m", w)
	}
	if w := l.allow("b", true); w != 0 {
		t.Fatal("another address was throttled")
	}
	now = now.Add(time.Minute)
	l.failed("a", true) // 6th failure: 2 minutes
	if w := l.allow("a", true); w != 2*time.Minute {
		t.Fatalf("wait = %v, want 2m", w)
	}
	for range 20 {
		l.failed("a", false)
	}
	if w := l.allow("a", false); w != time.Hour {
		t.Fatalf("wait capped at %v, want 1h", w)
	}
	l.succeeded("a")
	if w := l.allow("a", true); w != 0 {
		t.Fatal("success didn't clear the address")
	}
}

func TestLoginLimiterOverallCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	// 30 failures from 30 addresses: each stays under its own limit.
	for i := range 30 {
		l.failed("10.0.0."+strconv.Itoa(i), true)
		now = now.Add(time.Second)
	}
	if w := l.allow("10.0.1.1", true); w == 0 {
		t.Fatal("login codes still accepted past the overall cap")
	}
	if w := l.allow("10.0.1.1", false); w != 0 {
		t.Fatal("the full token was blocked by the overall cap (admin lock-out)")
	}
	now = now.Add(10 * time.Minute)
	if w := l.allow("10.0.1.1", true); w != 0 {
		t.Fatalf("cap didn't expire: %v", w)
	}
}
