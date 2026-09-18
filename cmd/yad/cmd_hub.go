package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hub/store"
)

// cmdHub is `yad hub`: the standalone hub. It listens — it is a server, and the
// no-port rule (decision 0004) binds runners, not hubs.
func cmdHub(ctx context.Context, g global, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yad hub serve [--listen addr] | yad hub token create [--ttl 1h] [--runner id]")
	}
	switch args[0] {
	case "serve":
		return cmdHubServe(ctx, g, args[1:], stdout)
	case "token":
		return cmdHubToken(ctx, g, args[1:], stdout, stderr)
	case "submit", "watch":
		return fmt.Errorf("`yad hub %s` arrives in epic E2 (Zumino yad/dev) — see ARCHITECTURE.md §9", args[0])
	default:
		return fmt.Errorf("unknown hub subcommand %q — use serve or token", args[0])
	}
}

func cmdHubServe(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("hub serve", flag.ContinueOnError)
	// Loopback by default: exposing a hub is a deliberate act, usually behind a
	// tailnet or a TLS-terminating proxy.
	listen := fs.String("listen", "127.0.0.1:7878", "address to serve the protocol on")
	dbFile := fs.String("db", g.paths.HubDB(), "the hub's database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := store.Open(ctx, *dbFile)
	if err != nil {
		return err
	}
	defer s.Close()
	h := hub.New(hub.Options{Store: s})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w — pick another address with --listen", *listen, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(w, "yad hub serving protocol v1 at http://%s%s — register a runner with a token from `yad hub token create`\n", ln.Addr(), hub.BasePath)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	// Syncs sweep too, but a hub whose last runner went away gets no syncs,
	// and that runner's runs must still be marked lost.
	sweep := time.NewTicker(h.SweepEvery())
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shut)
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-sweep.C:
			if err := h.Sweep(ctx); err != nil && ctx.Err() == nil {
				fmt.Fprintf(w, "%s sweep failed: %v\n", time.Now().Format(time.TimeOnly), err)
			}
		}
	}
}

// cmdHubToken prints a one-time registration token. The token goes to stdout
// alone, so it can be piped; everything said about it goes to stderr.
func cmdHubToken(ctx context.Context, g global, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return fmt.Errorf("usage: yad hub token create [--ttl 1h] [--runner id]")
	}
	fs := flag.NewFlagSet("hub token create", flag.ContinueOnError)
	ttl := fs.Duration("ttl", hub.DefaultTokenTTL, "how long the token can be used, at most "+hub.MaxTokenTTL.String())
	dbFile := fs.String("db", g.paths.HubDB(), "the hub's database — the one `yad hub serve` uses")
	forRunner := fs.String("runner", "", "re-register this existing runner id, replacing its credential (default: a token for a new runner)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := store.Open(ctx, *dbFile)
	if err != nil {
		return err
	}
	defer s.Close()
	tok, exp, err := hub.IssueRegistrationToken(ctx, s, *ttl, time.Now(), *forRunner)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, tok)
	fmt.Fprintf(stderr, "registers one runner, once, until %s — on the runner: yad connect <hub url> --token -  (and paste it)\n", exp.Local().Format(time.DateTime))
	return nil
}
