package web

import (
	"sync"
	"time"
)

// loginLimiter makes guessing the 8-character login code hopeless:
//   - per address: 5 free attempts, then a pause that doubles with every
//     further failure (1 min, 2 min, ... up to 1 h); a success clears it.
//   - overall: at most 30 failed code attempts per 10 minutes from all
//     addresses together, so many addresses can't share the work.
//
// At 30 tries per 10 minutes, 2^40 codes take ~70,000 years to cover half.
// The full admin token (256 bits) skips the overall cap, so a flood of
// guesses can't lock the admin out.
type loginLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	byAddr  map[string]*addrFails
	recent  []time.Time // failed code attempts, all addresses
	free    int
	window  time.Duration
	global  int
	maxWait time.Duration
}

type addrFails struct {
	fails int
	until time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{now: time.Now, byAddr: map[string]*addrFails{}, free: 5,
		window: 10 * time.Minute, global: 30, maxWait: time.Hour}
}

// allow reports how long addr must wait before trying again (0 = go ahead).
// code says whether the attempt is a login code (subject to the overall cap).
func (l *loginLimiter) allow(addr string, code bool) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if a := l.byAddr[addr]; a != nil && now.Before(a.until) {
		return a.until.Sub(now)
	}
	if code {
		l.prune(now)
		if len(l.recent) >= l.global {
			return l.recent[0].Add(l.window).Sub(now)
		}
	}
	return 0
}

func (l *loginLimiter) failed(addr string, code bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a := l.byAddr[addr]
	if a == nil {
		a = &addrFails{}
		l.byAddr[addr] = a
	}
	a.fails++
	if over := a.fails - l.free; over >= 0 { // the 5th failure starts the pause
		wait := time.Minute << min(over, 10)
		a.until = now.Add(min(wait, l.maxWait))
	}
	if code {
		l.prune(now)
		l.recent = append(l.recent, now)
	}
	if len(l.byAddr) > 10000 { // don't grow without bound
		for k, v := range l.byAddr {
			if now.After(v.until) {
				delete(l.byAddr, k)
			}
		}
	}
}

func (l *loginLimiter) succeeded(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byAddr, addr)
}

func (l *loginLimiter) prune(now time.Time) {
	i := 0
	for i < len(l.recent) && now.Sub(l.recent[i]) >= l.window {
		i++
	}
	l.recent = l.recent[i:]
}
