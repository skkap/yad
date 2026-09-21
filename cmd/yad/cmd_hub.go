package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/skkap/yad/protocol/hubapi"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hub/store"
)

// cmdHub is `yad hub`: the standalone hub. It listens — it is a server, and the
// no-port rule (decision 0004) binds runners, not hubs.
func cmdHub(ctx context.Context, g global, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: yad hub serve | token create | admin-token create|list|revoke | submit | watch <run> | cancel <run> | interrupt <run> | steer <run> <text> | runners [runner] | drain <runner> | close-session <session>")
	}
	switch args[0] {
	case "serve":
		return cmdHubServe(ctx, g, args[1:], stdout)
	case "token":
		return cmdHubToken(ctx, g, args[1:], stdout, stderr)
	case "admin-token":
		return cmdHubAdminToken(ctx, g, args[1:], stdout, stderr)
	case "submit":
		return cmdHubSubmit(ctx, g, args[1:], stdout, stderr)
	case "watch":
		return cmdHubWatch(ctx, g, args[1:], stdout, stderr)
	case "cancel", "interrupt", "steer":
		return cmdHubControl(ctx, g, args[0], args[1:], stdout)
	case "runners":
		return cmdHubRunners(ctx, g, args[1:], stdout)
	case "drain":
		return cmdHubDrain(ctx, g, args[1:], stdout)
	case "close-session":
		return cmdHubCloseSession(ctx, g, args[1:], stdout)
	default:
		return fmt.Errorf("unknown hub subcommand %q — use serve, token, admin-token, submit, watch, cancel, interrupt, steer, runners, drain or close-session", args[0])
	}
}

func cmdHubServe(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("hub serve", flag.ContinueOnError)
	listen := fs.String("listen", defaultHubListen, "address to serve the protocol and the service API on")
	dbFile := fs.String("db", g.paths.HubDB(), "the hub's database")
	minVersion := fs.String("min-version", "", "refuse runners older than this yad version, e.g. 0.4.0 (default: take any version)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Checked before the listener: a floor the hub cannot read would refuse
	// nothing, and the operator would never learn their flag was ignored.
	if err := hub.ValidateMinVersion(*minVersion); err != nil {
		return err
	}
	s, err := store.Open(ctx, *dbFile)
	if err != nil {
		return err
	}
	defer s.Close()
	h := hub.New(hub.Options{Store: s, MinVersion: *minVersion})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w — pick another address with --listen", *listen, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(w, "yad hub serving protocol v1 at http://%s%s — register a runner with a token from `yad hub token create`\n", ln.Addr(), hub.BasePath)
	fmt.Fprintf(w, "service API at http://%s%s — submit runs with `yad hub submit` and an admin token from `yad hub admin-token create`\n", ln.Addr(), hubapi.BasePath)
	if *minVersion != "" {
		fmt.Fprintf(w, "runners older than yad %s are refused at register and at sync, with the next action `yad upgrade`\n", *minVersion)
		// A refused runner stops syncing, so whatever it holds stops renewing
		// too. An operator raising the floor on a working fleet is entitled to
		// hear that before the sweep records those runs lost.
		fmt.Fprintln(w, "a runner refused mid-run stops syncing, so the runs it holds lose their leases and are recorded lost — drain it first (`yad hub drain <runner>`) to raise the floor without that")
	}

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

// defaultHubListen is loopback: exposing a hub is a deliberate act, usually
// behind a tailnet or a TLS-terminating proxy.
const defaultHubListen = "127.0.0.1:7878"

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

// cmdHubAdminToken manages the admin tokens the service API accepts. They are
// created on the hub's machine, against its database, like registration
// tokens; unlike those, they are long-lived, so by default the token goes
// straight into a 0600 file and is never shown.
func cmdHubAdminToken(ctx context.Context, g global, args []string, stdout, stderr io.Writer) error {
	const usage = "usage: yad hub admin-token create [--name cli] [--out file|-] | list | revoke <name>"
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("hub admin-token "+args[0], flag.ContinueOnError)
	dbFile := fs.String("db", g.paths.HubDB(), "the hub's database — the one `yad hub serve` uses")
	name := fs.String("name", "cli", "what to call the token, to tell it apart and revoke it")
	out := fs.String("out", g.paths.HubAdminToken(), "file to save the token in (0600); - prints it once to stdout, for a service's secret store")
	// The subcommand is judged before its arguments are: how many positionals
	// are right depends on which one it is, so a mistyped subcommand checked
	// afterwards is reported as a bad argument, and the complaint points at
	// the one part of the command line that was fine.
	want := 0
	switch args[0] {
	case "revoke":
		want = 1 // the token it names
	case "create", "list":
	default:
		return fmt.Errorf("unknown admin-token subcommand %q — %s", args[0], usage)
	}
	pos, err := positional(fs, args[1:], want, usage)
	if err != nil {
		return err
	}
	open := func() (*store.Store, error) { return store.Open(ctx, *dbFile) }
	// A follow-up command acts on the database this one did, or it revokes a
	// token in some other hub.
	again := func(sub ...string) string {
		argv := append([]string{"hub", "admin-token"}, sub...)
		if *dbFile != g.paths.HubDB() {
			argv = append(argv, "--db", *dbFile)
		}
		return config.YadCommand(g.paths.Profile, argv...)
	}
	switch args[0] {
	case "create":
		// Checked before the token exists: a token issued and then not
		// saved is a live secret nobody holds.
		if *out != "-" {
			if _, err := os.Stat(*out); err == nil {
				return fmt.Errorf("%s already holds an admin token — revoke that one (`%s`, then revoke), delete the file, and create again; or pass --out elsewhere", *out, again("list"))
			}
		}
		s, err := open()
		if err != nil {
			return err
		}
		defer s.Close()
		tok, err := hub.IssueAdminToken(ctx, s, *name, time.Now())
		if err != nil {
			return err
		}
		if *out == "-" {
			fmt.Fprintln(stdout, tok)
			fmt.Fprintf(stderr, "admin token %q — shown this once; store it as the service's secret, never in a command line\n", *name)
			return nil
		}
		if err := config.WriteSecret(*out, tok); err != nil {
			return errors.Join(fmt.Errorf("the token was created but not saved to %s — revoke it with `%s`", *out, again("revoke", *name)), err)
		}
		fmt.Fprintf(stdout, "admin token %q saved to %s (0600) — `yad hub submit` and `yad hub watch` read it from there\n", *name, *out)
		return nil
	case "list":
		s, err := open()
		if err != nil {
			return err
		}
		defer s.Close()
		toks, err := s.ListAdminTokens(ctx)
		if err != nil {
			return err
		}
		if len(toks) == 0 {
			fmt.Fprintln(stdout, "no admin tokens — `yad hub admin-token create` makes one")
		}
		for _, t := range toks {
			fmt.Fprintf(stdout, "%s\tcreated %s\n", t.Name, time.UnixMilli(t.CreatedAt).Local().Format(time.DateTime))
		}
		return nil
	case "revoke":
		s, err := open()
		if err != nil {
			return err
		}
		defer s.Close()
		if err := hub.RevokeAdminToken(ctx, s, pos[0]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "admin token %q revoked — it stops working on its next request\n", pos[0])
		return nil
	}
	return nil
}
