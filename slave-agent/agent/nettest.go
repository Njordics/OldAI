package agent

import (
	"crypto/rand"
	"io"
	"net/http"
	"strconv"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/linkinfo"
	"aicluster/common/proto"
	"aicluster/slave-agent/netcfg"
)

// linkTo reads the link of the interface that reaches the Controller.
func linkTo(ad *adopted) *proto.Link {
	if ad == nil || len(ad.Controller.Endpoints) == 0 {
		return nil
	}
	o, err := netcfg.Route(endpointHost(ad.Controller.Endpoints[0]))
	if err != nil || o == nil {
		return nil
	}
	return linkinfo.Read(o.Iface)
}

// Network speed test (protocol §16): the Controller downloads from and
// uploads to the agent over the node API's own mTLS connection, so the
// result includes everything a model download goes through.

const maxNetTestBytes = 1 << 30

// handleNetTestSend streams ?bytes= of random data to the Controller.
func (a *Agent) handleNetTestSend(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
	if err != nil || n <= 0 || n > maxNetTestBytes {
		http.Error(w, "bytes must be between 1 and 1 GiB", http.StatusBadRequest)
		return
	}
	buf := make([]byte, 1<<20)
	_, _ = rand.Read(buf)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	for n > 0 {
		k := min(n, int64(len(buf)))
		if _, err := w.Write(buf[:k]); err != nil {
			return
		}
		n -= k
	}
}

// handleNetTestReceive reads and drops the request body, and says how long
// that took on this side.
func (a *Agent) handleNetTestReceive(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	n, err := io.Copy(io.Discard, io.LimitReader(r.Body, maxNetTestBytes+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"bytes": n, "seconds": time.Since(start).Seconds()})
}
