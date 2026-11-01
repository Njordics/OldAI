// Package discovery implements the LAN announcement channels from
// docs/protocol.md §2: mDNS (primary) and a UDP broadcast beacon (fallback).
// Both are unauthenticated hints; trust is established only by the TLS
// handshake that follows.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"time"

	"aicluster/common/proto"
)

const maxBeaconSize = 1200

// BeaconSender broadcasts Beacon datagrams.
type BeaconSender struct {
	conn *net.UDPConn
	// Targets overrides the destinations (host:port). When empty the sender
	// uses 255.255.255.255 plus every interface's directed broadcast.
	Targets []string
	Port    int
}

func NewBeaconSender(port int, targets []string) (*BeaconSender, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	return &BeaconSender{conn: conn, Targets: targets, Port: port}, nil
}

func (s *BeaconSender) Close() error { return s.conn.Close() }

// Send marshals b and writes it to every target. It returns the last error
// only if every target failed.
func (s *BeaconSender) Send(b proto.Beacon) error {
	b.V = proto.Version
	b.TS = time.Now().UTC()
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if len(data) > maxBeaconSize {
		return errors.New("discovery: beacon too large")
	}
	var lastErr error
	sent := 0
	for _, t := range s.targets() {
		addr, err := net.ResolveUDPAddr("udp4", t)
		if err != nil {
			lastErr = err
			continue
		}
		if _, err := s.conn.WriteToUDP(data, addr); err != nil {
			lastErr = err
			continue
		}
		sent++
	}
	if sent == 0 && lastErr != nil {
		return lastErr
	}
	return nil
}

func (s *BeaconSender) targets() []string {
	if len(s.Targets) > 0 {
		return s.Targets
	}
	port := strconv.Itoa(s.Port)
	out := []string{net.JoinHostPort("255.255.255.255", port)}
	for _, bc := range DirectedBroadcasts() {
		out = append(out, net.JoinHostPort(bc.String(), port))
	}
	return out
}

// DirectedBroadcasts returns the IPv4 broadcast address of every up,
// non-loopback interface.
func DirectedBroadcasts() []net.IP {
	var out []net.IP
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := ipn.IP.To4()
			mask := net.IP(ipn.Mask).To4()
			if mask == nil {
				continue
			}
			bc := make(net.IP, 4)
			for i := range bc {
				bc[i] = ip[i] | ^mask[i]
			}
			out = append(out, bc)
		}
	}
	return out
}

// LocalIPv4s returns the IPv4 addresses of every up, non-loopback interface.
func LocalIPv4s() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

// ListenBeacons reads beacons on addr (e.g. ":47800") until ctx ends and
// calls fn for each well-formed one. from is the datagram's source address,
// which is more trustworthy than the self-reported Addrs.
func ListenBeacons(ctx context.Context, addr string, log *slog.Logger, fn func(b proto.Beacon, from *net.UDPAddr)) error {
	laddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("beacon read failed", "err", err)
			continue
		}
		var b proto.Beacon
		if err := json.Unmarshal(buf[:n], &b); err != nil || b.V != proto.Version {
			log.Debug("ignoring malformed beacon", "from", from, "err", err)
			continue
		}
		fn(b, from)
	}
}

// Jitter returns d ±20 %.
func Jitter(d time.Duration) time.Duration {
	j := time.Duration(time.Now().UnixNano() % int64(d/5*2+1))
	return d - d/5 + j
}
