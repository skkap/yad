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
)

// cmdHub is `yad hub`: the standalone hub. It listens — it is a server, and the
// no-port rule (decision 0004) binds runners, not hubs.
func cmdHub(ctx context.Context, args []string, w io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yad hub serve [--listen addr]")
	}
	switch args[0] {
	case "serve":
	case "submit", "watch", "token":
		return fmt.Errorf("`yad hub %s` arrives in epic E2 (Zumino yad/dev) — see ARCHITECTURE.md §9", args[0])
	default:
		return fmt.Errorf("unknown hub subcommand %q — use serve", args[0])
	}
	fs := flag.NewFlagSet("hub serve", flag.ContinueOnError)
	// Loopback by default: exposing a hub is a deliberate act, usually behind a
	// tailnet or a TLS-terminating proxy.
	listen := fs.String("listen", "127.0.0.1:7878", "address to serve the protocol on")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w — pick another address with --listen", *listen, err)
	}
	srv := &http.Server{Handler: hub.New(), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(w, "yad hub serving protocol v1 at http://%s%s (operations answer not_implemented until epic E2)\n", ln.Addr(), hub.BasePath)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
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
	}
}
