package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"aicluster/common/discovery"
	"aicluster/common/httpx"
	"aicluster/common/pki"
	"aicluster/common/proto"
	"aicluster/slave-agent/inventory"
)

const (
	observedRefresh = 60 * time.Second
	requestTimeout  = 10 * time.Second
)

// controllerClient returns an mTLS client that presents the node cert and
// accepts only this cluster's Controller.
func controllerClient(ad *adopted) *http.Client {
	pool, want := ad.caPool, pki.ControllerURI(ad.ClusterID)
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS13,
				Certificates:       []tls.Certificate{ad.cert},
				InsecureSkipVerify: true, // replaced by VerifyPeerCertificate: CA pin + URI SAN, not hostname
				VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
					_, err := pki.VerifyPeer(raw, pool, want, x509.ExtKeyUsageServerAuth)
					return err
				},
			},
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	var (
		client   *http.Client
		clientOf *adopted
	)
	for {
		a.mu.RLock()
		ad := a.adopted
		interval := proto.DefaultHeartbeatInterval
		if ad != nil && ad.HeartbeatIntervalS > 0 {
			interval = time.Duration(ad.HeartbeatIntervalS) * time.Second
		}
		a.mu.RUnlock()

		if ad != nil {
			if clientOf != ad {
				client, clientOf = controllerClient(ad), ad
			}
			a.heartbeatOnce(ctx, client, ad)
		}

		// ±10 % jitter.
		wait := interval - interval/10 + time.Duration(time.Now().UnixNano()%int64(interval/5+1))
		select {
		case <-ctx.Done():
			return
		case <-a.wakeHB:
		case <-time.After(wait):
		}
	}
}

func (a *Agent) heartbeatOnce(ctx context.Context, client *http.Client, ad *adopted) {
	a.mu.Lock()
	a.seq++
	hb := proto.Heartbeat{
		V:      proto.Version,
		NodeID: a.nodeID,
		Seq:    a.seq,
		BootID: inventory.BootID(),
		TS:     time.Now().UTC(),
		Addrs:  discovery.LocalIPv4s(),
		Health: inventory.Health(a.cfg.ModelDir),
	}
	hb.Health.Inference = proto.InferenceLoad{InFlight: int(a.infer.inFlight.Load()), Slots: a.cfg.InferenceSlots}
	hb.Health.Thermal, hb.Health.GPUs = a.latestThermal()
	hb.Health.Link = linkTo(ad)
	nodeID := a.nodeID
	lastObs, lastSent := a.lastObs, a.lastObsSent
	a.mu.Unlock()
	// Observed state rides along when it changed (including download
	// progress) and at least every observedRefresh.
	obs := a.observed()
	obsJSON := observedJSON(obs)
	sendObs := obsJSON != lastObs || time.Since(lastSent) > observedRefresh
	if sendObs {
		hb.Observed = obs
	}

	var resp proto.HeartbeatResp
	ep, err := a.tryEndpoints(ad, func(base string) error {
		return httpx.Do(ctx, client, http.MethodPost, base+"/v1/nodes/"+nodeID+"/heartbeat", hb, &resp)
	})
	if err != nil {
		if httpx.Code(err) == proto.ErrRevoked {
			// The Controller released us while we were away (§5.5).
			if rerr := a.release("certificate revoked by controller", false, true); rerr != nil {
				a.log.Error("release after revocation failed", "err", rerr)
			}
			return
		}
		a.log.Warn("heartbeat failed", "err", err)
		return
	}
	a.noteHeartbeatOK()
	if sendObs {
		a.mu.Lock()
		a.lastObsSent, a.lastObs = time.Now(), obsJSON
		a.mu.Unlock()
	}
	a.applyHeartbeatResp(ctx, client, ad, ep, resp)
}

// tryEndpoints calls fn against each known Controller endpoint until one
// succeeds, and moves the working one to the front.
func (a *Agent) tryEndpoints(ad *adopted, fn func(base string) error) (string, error) {
	a.mu.RLock()
	eps := slices.Clone(ad.Controller.Endpoints)
	a.mu.RUnlock()
	var lastErr error
	for i, ep := range eps {
		err := fn(ep)
		if err == nil {
			if i > 0 {
				a.mu.Lock()
				ad.Controller.Endpoints = append([]string{ep}, slices.DeleteFunc(slices.Clone(ad.Controller.Endpoints), func(s string) bool { return s == ep })...)
				a.mu.Unlock()
			}
			return ep, nil
		}
		// An API error means we reached the Controller; don't try the others.
		if httpx.Code(err) != "" {
			return ep, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errNoEndpoints
	}
	return "", lastErr
}

type agentError string

func (e agentError) Error() string { return string(e) }

const errNoEndpoints = agentError("no controller endpoints known")

func (a *Agent) applyHeartbeatResp(ctx context.Context, client *http.Client, ad *adopted, ep string, resp proto.HeartbeatResp) {
	a.mu.Lock()
	changed := false
	if len(resp.ControllerEndpoints) > 0 {
		// The Controller's list, but the endpoint that just answered stays
		// first: on the Slave network the home addresses only time out.
		eps := slices.Clone(resp.ControllerEndpoints)
		if i := slices.Index(eps, ep); i > 0 {
			eps = append([]string{ep}, slices.Delete(eps, i, i+1)...)
		}
		if !slices.Equal(eps, ad.Controller.Endpoints) {
			ad.Controller.Endpoints = eps
			changed = true
		}
	}
	if resp.HeartbeatIntervalS > 0 && resp.HeartbeatIntervalS != ad.HeartbeatIntervalS {
		ad.HeartbeatIntervalS = resp.HeartbeatIntervalS
		changed = true
	}
	if changed && a.adopted == ad {
		if err := a.writeJSON(fileAdoption, ad.adoption, 0o644); err != nil {
			a.log.Error("persist adoption", "err", err)
		}
	}
	behind := resp.DesiredGeneration > a.desired.Generation
	leaf := ad.leaf
	nodeID := a.nodeID
	a.mu.Unlock()

	if behind {
		var ds proto.DesiredState
		if err := httpx.Do(ctx, client, http.MethodGet, ep+"/v1/nodes/"+nodeID+"/desired-state", nil, &ds); err != nil {
			a.log.Warn("pull desired state failed", "err", err)
		} else {
			a.applyDesired(ds)
		}
	}

	life := leaf.NotAfter.Sub(leaf.NotBefore)
	if resp.CertRenew || time.Now().After(leaf.NotBefore.Add(life*2/3)) {
		if err := a.renew(ctx, client, ep, ad); err != nil {
			a.log.Warn("certificate renewal failed", "err", err)
		}
	}
}

// renew rotates the node key and certificate (§5.4).
func (a *Agent) renew(ctx context.Context, client *http.Client, ep string, ad *adopted) error {
	keyPEM, csrPEM, err := pki.NewNodeKeyAndCSR(a.NodeID())
	if err != nil {
		return err
	}
	var resp proto.RenewResponse
	if err := httpx.Do(ctx, client, http.MethodPost, ep+"/v1/nodes/"+a.NodeID()+"/renew",
		proto.RenewRequest{V: proto.Version, CSRPEM: string(csrPEM)}, &resp); err != nil {
		return err
	}
	if _, err := pki.VerifyPeer([][]byte{pemDER([]byte(resp.NodeCertPEM))}, ad.caPool, pki.NodeURI(a.NodeID()), x509.ExtKeyUsageClientAuth); err != nil {
		return err
	}
	cert, err := tls.X509KeyPair([]byte(resp.NodeCertPEM), keyPEM)
	if err != nil {
		return err
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.adopted != ad {
		return nil // released meanwhile
	}
	if err := a.writeNodeIdentity(keyPEM, []byte(resp.NodeCertPEM)); err != nil {
		return err
	}
	next := *ad
	next.cert, next.leaf = cert, leaf
	a.adopted = &next
	a.log.Info("certificate renewed", "not_after", leaf.NotAfter)
	nudge(a.stateC)
	return nil
}

// controllerLocator learns new Controller addresses (after a DHCP change)
// from controller beacons and mDNS. Only entries for our own cluster_id are
// used, and the address is still authenticated by the CA pin on connect.
func (a *Agent) controllerLocator(ctx context.Context) {
	add := func(ip string, port int, clusterID string) {
		a.mu.Lock()
		defer a.mu.Unlock()
		ad := a.adopted
		if ad == nil || clusterID != ad.ClusterID || ip == "" {
			return
		}
		ep := "https://" + net.JoinHostPort(ip, strconv.Itoa(port))
		if !slices.Contains(ad.Controller.Endpoints, ep) {
			ad.Controller.Endpoints = append(ad.Controller.Endpoints, ep)
			a.log.Info("learned controller endpoint", "endpoint", ep)
		}
	}
	if a.cfg.MDNS {
		go discovery.Browse(ctx, proto.ControllerMDNSService, a.log, func(e discovery.MDNSEntry) {
			for _, ip := range e.IPs {
				add(ip.String(), e.Port, e.TXT["cl"])
			}
		})
	}
	if a.cfg.ControllerBeaconListen != "" {
		err := discovery.ListenBeacons(ctx, a.cfg.ControllerBeaconListen, a.log, func(b proto.Beacon, from *net.UDPAddr) {
			if b.Kind == proto.BeaconKindController {
				add(from.IP.String(), b.Port, b.ClusterID)
			}
		})
		if err != nil {
			a.log.Warn("controller beacon listener disabled", "err", err)
		}
	}
	<-ctx.Done()
}
