package discovery

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

// Announcer publishes one mDNS service instance and lets the caller update
// its TXT records as the agent's state changes.
type Announcer struct {
	srv *zeroconf.Server
}

func Announce(instance, service string, port int, txt map[string]string) (*Announcer, error) {
	srv, err := zeroconf.Register(instance, service, "local.", port, encodeTXT(txt), nil)
	if err != nil {
		return nil, err
	}
	return &Announcer{srv: srv}, nil
}

func (a *Announcer) Update(txt map[string]string) { a.srv.SetText(encodeTXT(txt)) }
func (a *Announcer) Close()                       { a.srv.Shutdown() }

// MDNSEntry is a browse result reduced to what the cluster needs.
type MDNSEntry struct {
	Instance string
	Port     int
	IPs      []net.IP
	TXT      map[string]string
}

// browseRound bounds each zeroconf browse. zeroconf reports an instance only
// once per browse, so restarting is how TXT changes (e.g. state) are re-seen.
const browseRound = 30 * time.Second

// Browse calls fn for every instance of service seen until ctx ends.
func Browse(ctx context.Context, service string, log *slog.Logger, fn func(MDNSEntry)) {
	for ctx.Err() == nil {
		round, cancel := context.WithTimeout(ctx, browseRound)
		// zeroconf closes entries when a browse that started successfully
		// ends; fn must therefore be safe to call from several goroutines.
		entries := make(chan *zeroconf.ServiceEntry, 16)
		go func() {
			for e := range entries {
				fn(MDNSEntry{Instance: e.Instance, Port: e.Port, IPs: e.AddrIPv4, TXT: decodeTXT(e.Text)})
			}
		}()
		if err := zeroconf.Browse(round, service, "local.", entries); err != nil {
			log.Warn("mdns browse failed", "service", service, "err", err)
			<-round.Done()
		}
		cancel()
	}
}

func encodeTXT(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func decodeTXT(txt []string) map[string]string {
	m := make(map[string]string, len(txt))
	for _, kv := range txt {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}
