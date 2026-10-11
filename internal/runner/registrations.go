package runner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store"
)

// adoptRegistrations makes each configured connection's name the registration
// config.toml says it is, moving whatever an earlier registration left under
// the name to a retired name first (decision 0078). It runs once, at the
// daemon's start: a connection `yad connect` adds while a daemon runs is not
// synced until the next one, so no loop can be writing under a name while it
// is moved.
//
// What it returns are the connections it could not settle. Those are not
// synced: their hub would be sent, or refused over, what another hub left.
func adoptRegistrations(ctx context.Context, st *store.Store, p config.Paths, conns []config.Connection, log *slog.Logger) map[string]error {
	failed := map[string]error{}
	now := time.Now()
	for _, c := range conns {
		r, err := st.AdoptRegistration(ctx, c.Name, c.RegisteredAt, now)
		if err != nil {
			failed[c.Name] = fmt.Errorf("what an earlier registration under this name left here could not be moved aside (%w), and this hub's ids would meet it — `%s` tries again", err, p.Command("daemon", "restart"))
			continue
		}
		if r.As != "" {
			// The rows are history, and the collector ends whatever of them
			// is not ended yet: no loop will ever serve the retired name.
			log.Info("a new registration took this connection's name: what the earlier one left is kept under a retired name",
				"connection", c.Name, "retired_as", r.As, "sessions", r.Sessions, "runs", r.Runs)
		}
	}
	return failed
}
