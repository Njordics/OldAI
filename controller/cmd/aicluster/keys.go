package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"aicluster/controller/store"
	"aicluster/controller/web"
)

func keysCmd(ctx context.Context, c *client, args []string, asJSON bool) {
	sub := "ls"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "ls", "list":
		var keys []store.APIKey
		must(c.do(ctx, http.MethodGet, "/api/v1/keys", nil, &keys))
		if asJSON {
			printJSON(keys)
			return
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tKEY\tMODELS\tRPM\tLAST USED")
		for _, k := range keys {
			models := "all"
			if len(k.Models) > 0 {
				models = strings.Join(k.Models, ",")
			}
			fmt.Fprintf(tw, "%s\t%s\taic_%s…\t%s\t%d\t%s\n", k.ID[:10], k.Name, k.Prefix, models, k.RPM, ago(k.LastUsedAt))
		}
		tw.Flush()
	case "create":
		fs := flag.NewFlagSet("keys create", flag.ExitOnError)
		models := fs.String("models", "", "comma-separated models this key may use (default: all)")
		rpm := fs.Int("rpm", 0, "requests per minute (0 = unlimited)")
		name := positional(fs, args, "keys create <name> [--models a,b] [--rpm N]")
		req := web.CreateKeyRequest{Name: name, RPM: *rpm}
		if *models != "" {
			req.Models = strings.Split(*models, ",")
		}
		var k web.CreatedKey
		must(c.do(ctx, http.MethodPost, "/api/v1/keys", req, &k))
		fmt.Fprintln(os.Stderr, "Copy this key now; it is not shown again:")
		fmt.Println(k.Key)
	case "rm", "revoke":
		if len(args) != 1 {
			fatal(errors.New("usage: aicluster keys rm <name|id>"))
		}
		var keys []store.APIKey
		must(c.do(ctx, http.MethodGet, "/api/v1/keys", nil, &keys))
		var hit []store.APIKey
		for _, k := range keys {
			if k.ID == args[0] || k.Name == args[0] || strings.HasPrefix(k.ID, args[0]) {
				hit = append(hit, k)
			}
		}
		if len(hit) != 1 {
			fatal(fmt.Errorf("%q matches %d keys", args[0], len(hit)))
		}
		must(c.do(ctx, http.MethodDelete, "/api/v1/keys/"+hit[0].ID, nil, nil))
		fmt.Println("revoked", hit[0].Name)
	default:
		fatal(fmt.Errorf("unknown keys subcommand %q", sub))
	}
}
