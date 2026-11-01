// Command aicluster is the management CLI. It talks to the Controller's
// management API, pinning the cluster CA and authenticating with the admin
// token. On the Controller itself both are read from the data directory.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"aicluster/common/httpx"
	"aicluster/common/pki"
	"aicluster/controller/store"
	"aicluster/controller/web"
)

const usage = `usage: aicluster [global flags] <command> [args]

commands:
  cluster                          show cluster ID, CA fingerprint, endpoints
  nodes [ls]                       list nodes
  nodes show <id>                  show one node (full JSON)
  adopt <id> [--code 123456]       adopt a pending or released node
  release <id> [--keep-models]     release a node back to unadopted
  settings                         show settings
  settings set adoption_mode=auto|approve

<id> may be any unique prefix of a node ID, or a node name.

global flags:
`

type client struct {
	base  string
	token string
	http  *http.Client
}

func main() {
	defDir := envOr("AICLUSTER_DATA_DIR", "/var/lib/aicluster/controller")
	url := flag.String("controller", envOr("AICLUSTER_URL", "https://127.0.0.1:8090"), "management API URL")
	tokenFile := flag.String("token-file", filepath.Join(defDir, "admin.token"), "admin token file (or $AICLUSTER_TOKEN)")
	caFile := flag.String("ca-file", filepath.Join(defDir, "ca", "ca.crt"), "cluster CA certificate")
	asJSON := flag.Bool("json", false, "print JSON")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	c, err := newClient(*url, *tokenFile, *caFile)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	switch args[0] {
	case "cluster":
		var info web.ClusterInfo
		must(c.do(ctx, http.MethodGet, "/api/v1/cluster", nil, &info))
		printJSON(info)
	case "nodes":
		sub := "ls"
		if len(args) > 1 {
			sub = args[1]
		}
		switch sub {
		case "ls", "list":
			nodes := c.nodes(ctx)
			if *asJSON {
				printJSON(nodes)
			} else {
				printNodes(nodes)
			}
		case "show":
			if len(args) < 3 {
				fatal(errors.New("nodes show <id>"))
			}
			var n store.Node
			must(c.do(ctx, http.MethodGet, "/api/v1/nodes/"+c.resolve(ctx, args[2]), nil, &n))
			printJSON(n)
		default:
			fatal(fmt.Errorf("unknown nodes subcommand %q", sub))
		}
	case "adopt":
		fs := flag.NewFlagSet("adopt", flag.ExitOnError)
		code := fs.String("code", "", "pairing code shown on the node's console")
		id := positional(fs, args[1:], "adopt <id> [--code 123456]")
		must(c.do(ctx, http.MethodPost, "/api/v1/nodes/"+c.resolve(ctx, id)+"/adopt", web.AdoptRequest{PairingCode: *code}, nil))
		fmt.Println("enrollment complete; the node shows as adopted after its first heartbeat")
	case "release":
		fs := flag.NewFlagSet("release", flag.ExitOnError)
		keep := fs.Bool("keep-models", false, "keep downloaded models on the node")
		id := positional(fs, args[1:], "release <id> [--keep-models]")
		must(c.do(ctx, http.MethodPost, "/api/v1/nodes/"+c.resolve(ctx, id)+"/release", map[string]bool{"keep_models": *keep}, nil))
		fmt.Println("released")
	case "settings":
		var s web.Settings
		if len(args) >= 3 && args[1] == "set" {
			for _, kv := range args[2:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k != "adoption_mode" {
					fatal(fmt.Errorf("unknown setting %q (supported: adoption_mode)", kv))
				}
				s.AdoptionMode = v
			}
			must(c.do(ctx, http.MethodPut, "/api/v1/settings", s, &s))
		} else {
			must(c.do(ctx, http.MethodGet, "/api/v1/settings", nil, &s))
		}
		printJSON(s)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// positional parses flags that may come before or after the single
// positional argument.
func positional(fs *flag.FlagSet, args []string, use string) string {
	var pos []string
	for len(args) > 0 {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	if len(pos) != 1 {
		fatal(errors.New("usage: aicluster " + use))
	}
	return pos[0]
}

func newClient(url, tokenFile, caFile string) (*client, error) {
	token := os.Getenv("AICLUSTER_TOKEN")
	if token == "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("admin token: %w (set AICLUSTER_TOKEN or --token-file)", err)
		}
		token = strings.TrimSpace(string(b))
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("cluster CA: %w (use --ca-file)", err)
	}
	pool, err := pki.PoolFromPEM(caPEM)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // replaced by CA pin + controller URI SAN
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := pki.VerifyPeer(raw, pool, "", x509.ExtKeyUsageServerAuth)
			if err == nil && pki.ClusterIDFromCert(leaf) == "" {
				err = errors.New("server is not an aicluster controller")
			}
			return err
		},
	}}
	return &client{base: strings.TrimRight(url, "/"), token: token, http: &http.Client{Transport: tr}}, nil
}

func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	return httpx.Do(ctx, c.http, method, c.base+path, in, out, "Authorization", "Bearer "+c.token)
}

func (c *client) nodes(ctx context.Context) []*store.Node {
	var nodes []*store.Node
	must(c.do(ctx, http.MethodGet, "/api/v1/nodes", nil, &nodes))
	return nodes
}

// resolve turns a name or ID prefix into a full node ID.
func (c *client) resolve(ctx context.Context, ref string) string {
	var hits []string
	for _, n := range c.nodes(ctx) {
		if n.NodeID == ref || n.Name == ref {
			return n.NodeID
		}
		if strings.HasPrefix(n.NodeID, ref) {
			hits = append(hits, n.NodeID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0]
	case 0:
		fatal(fmt.Errorf("no node matches %q", ref))
	default:
		fatal(fmt.Errorf("%q matches %d nodes; use a longer prefix", ref, len(hits)))
	}
	return ""
}

func printNodes(nodes []*store.Node) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name+nodes[i].NodeID < nodes[j].Name+nodes[j].NodeID })
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tPHASE\tADDR\tHW\tAGENT\tLAST SEEN\tNOTE")
	for _, n := range nodes {
		note := n.LastError
		if n.PairingRequired && n.State == store.StatePending {
			note = "needs pairing code"
		}
		if n.Returning && n.State != store.StateAdopted {
			note = strings.TrimSpace("returning hardware " + note)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.NodeID[:8], dash(n.Name), n.State, dash(n.Phase), dash(n.Addr), dash(n.HWSummary),
			dash(n.AgentVersion), ago(n.LastSeen), note)
	}
	tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return time.Since(t).Truncate(time.Second).String() + " ago"
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "aicluster:", err)
	os.Exit(1)
}
