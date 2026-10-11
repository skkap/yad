package capability

import (
	"fmt"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	v1 "github.com/skkap/yad/protocol/v1"
)

// SilentHomes warns on each drivable harness about every home of it whose
// runs have gone silent — an account's, or the harness's own login when the
// owner gave it no accounts — and returns the harnesses left with no home
// that can take a run (decision 0077). A warning, not an error: an error
// makes a runner refuse the run outright, and this is a cool-down that ends
// by itself. The health report's ready and silent_until are what stop a hub
// sending work, and the claim, which reads them, leaves an offer for the hub
// rather than refusing it.
//
// command builds the yad commands the warnings give: the profile's own for
// `yad doctor`, and for the document, which reaches every hub, one that names
// no path (DEV-67).
func SilentHomes(found []harness.Detected, cfg config.Config, accounts []account.Account, silences account.Silences, now time.Time, command func(args ...string) string) map[string]bool {
	var notReady map[string]bool
	mark := func(id string) {
		if notReady == nil {
			notReady = map[string]bool{}
		}
		notReady[id] = true
	}
	restart := "once the cause is fixed, `" + command("daemon", "restart") + "` offers it again at once"
	for i, d := range found {
		if !d.Ready() {
			continue
		}
		mine := account.For(accounts, d.ID)
		// The owner's list decides which home runs use, not the states read:
		// a harness with accounts never runs on its own login, read or not.
		if len(cfg.Harness[d.ID].Accounts) == 0 {
			s, ok := silences.Default(d.ID, now)
			if !ok {
				continue
			}
			mark(d.ID)
			w := fmt.Sprintf("not ready: its own login has said nothing in its last %d runs — each stopped by the first-output timeout before its first event, ending harness_silent — so no run is started on it until %s, when it is tried again; %s",
				s.Runs, s.Until.UTC().Format(time.RFC3339), restart)
			// Only for a harness that can have accounts: the advice is
			// refused as printed for one that cannot.
			if account.HomeVar(d.ID) != "" {
				w += ". The one cause known is another " + d.ID + " running from the same home (DEV-170); an account is a home nobody else uses: `" +
					command("account", "add", d.ID, "<label>") + "`"
			}
			found[i].Warnings = append(found[i].Warnings, w)
			continue
		}
		usable, silent := false, false
		for _, a := range mine {
			if a.SilentUntil != nil {
				silent = true
				found[i].Warnings = append(found[i].Warnings, fmt.Sprintf("account %q has said nothing in its last %d runs — each stopped by the first-output timeout before its first event, ending harness_silent — so no run is started on it until %s, when it is tried again; %s",
					a.Label, a.SilentRuns, a.SilentUntil.UTC().Format(time.RFC3339), restart))
				continue
			}
			if a.State == v1.AccountFree {
				usable = true
			}
		}
		// Only when silence is part of why: a harness whose accounts all
		// need login or are limited is already said to be by other checks.
		if !usable && silent {
			mark(d.ID)
		}
	}
	return notReady
}
