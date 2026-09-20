package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
)

// Daemon is how Disconnect tells a running daemon what is happening, in the
// two stages either side of the hub call. Both are nil when no daemon runs.
type Daemon struct {
	// Beginning stops the connection syncing, before the hub is asked to
	// retire the registration. It destroys nothing.
	Beginning func(context.Context) error
	// Ended closes that connection's sessions, the hub having answered.
	Ended func(context.Context) error
}

// Outcome is what became of the registration at the hub. There are three, and
// they are not two: a hub that had already forgotten this runner and a hub
// that refused one --force went past both leave nothing retired by us, and
// what the owner must do next is opposite in the two cases.
type Outcome string

const (
	// Retired — the hub retired the registration at our asking.
	Retired Outcome = "retired"
	// AlreadyGone — there was no registration to retire: the hub answered
	// 401, 403 or 404, or no credential here could ask it.
	AlreadyGone Outcome = "already_gone"
	// LeftBehind — --force went past a hub that refused, so that hub still
	// holds a live registration for this runner.
	LeftBehind Outcome = "left_behind"
)

// Disconnection is what `yad disconnect` did.
type Disconnection struct {
	Connection config.Connection
	// Outcome is what became of the registration at the hub.
	Outcome Outcome
	// Refused is the hub's refusal that --force went past, for LeftBehind.
	Refused error
	// Remaining is how many connections config.toml is left with.
	Remaining int
}

// standing is what the hub is left holding, for an error that arrives after
// the point of no return. Telling the owner the credential is dead when the
// hub still accepts it is how a live credential is left behind for good;
// telling them it is live when the hub has already forgotten it sends them to
// retire a registration that is not there.
func (d Disconnection) standing() string {
	switch d.Outcome {
	case Retired:
		return "this runner is deregistered at " + d.Connection.URL
	case AlreadyGone:
		return "this runner was already unknown to " + d.Connection.URL
	default:
		return "this runner is still registered at " + d.Connection.URL
	}
}

// andRetire is the step left at the hub, which only LeftBehind has.
func (d Disconnection) andRetire() string {
	if d.Outcome == LeftBehind {
		return ", and retire this runner at " + d.Connection.URL + " — the credential still works there"
	}
	return "; it is no longer accepted anywhere"
}

// Disconnect retires this runner's registration with one hub and forgets it:
// the hub deregisters it — marking every run it still holds lost — and then
// the credential and the connection's entry in config.toml go.
//
// Nothing is removed until the hub has answered, and a hub that cannot be
// reached keeps both: removing a credential cannot be undone from here, a hub
// that is down for a minute is the likelier case, and a retry costs nothing.
// force goes ahead anyway, for a hub that is gone for good. A hub that answers
// 401, 403 or 404 has already forgotten this runner, so there is no credential
// left to retire and the removal goes ahead without force.
//
// The daemon is told in two stages, around the hub call. Beginning stops the
// connection syncing *before* the hub retires the registration, so the sync
// that would take the 401 its own deregistration causes never happens; it
// destroys nothing, so a hub that then refuses leaves the connection stopped
// until `yad daemon restart` and nothing worse. Ended closes that connection's
// sessions once the hub has answered, which is when their workdirs may go. A
// failure in either is a note and never fatal — by the second, the
// registration is gone, and a credential kept for a hub that has forgotten it
// is a secret nobody will ever clean up.
//
// The credential goes before the config entry. A deregistered credential is
// worthless, so one orphaned by a failure in between costs nothing, while a
// live credential with no connection naming it is a secret nobody would find.
func Disconnect(ctx context.Context, p config.Paths, name string, force bool, d Daemon) (Disconnection, []string, error) {
	var notes []string
	cfg, err := config.Load(p)
	if err != nil {
		return Disconnection{}, notes, err
	}
	i := slices.IndexFunc(cfg.Connections, func(c config.Connection) bool { return c.Name == name })
	if i < 0 {
		var names []string
		for _, c := range cfg.Connections {
			names = append(names, c.Name)
		}
		if len(names) == 0 {
			return Disconnection{}, notes, fmt.Errorf("this runner has no connection called %q, and none at all — `yad connect <url> --token …` adds one", name)
		}
		return Disconnection{}, notes, fmt.Errorf("this runner has no connection called %q — it has %s (%s lists them)", name, strings.Join(names, ", "), p.ConfigFile())
	}
	// Nothing here reaches the hub until the credential is read, so the
	// starting point is the hub having no registration of ours to retire.
	out := Disconnection{Connection: cfg.Connections[i], Outcome: AlreadyGone}

	// Before the hub, so nothing this runner does next is read as a failure.
	if d.Beginning != nil {
		if err := d.Beginning(ctx); err != nil {
			notes = append(notes, "the running daemon was not told to stop this connection ("+err.Error()+"); it stops at its next sync, when the hub refuses the credential — `yad daemon restart` ends it now")
		}
	}

	switch cred, err := p.Credential(name); {
	case err != nil:
		// Unreadable or gone: there is no way to retire the registration from
		// here, and refusing to go on would leave the owner with a connection
		// they cannot remove. The note says what is left to do at the hub.
		notes = append(notes, fmt.Sprintf("the credential could not be read (%v), so %s was not told — retire this runner at the hub itself", err, out.Connection.URL))
	default:
		id, err := p.RunnerID()
		if err != nil {
			return out, notes, err
		}
		client, err := hubclient.New(out.Connection.URL, cred)
		if err != nil {
			return out, notes, err
		}
		switch err := client.Deregister(ctx, id, "the owner ran `yad disconnect`"); {
		case err == nil:
			out.Outcome = Retired
		case alreadyGone(err):
			out.Outcome = AlreadyGone
			notes = append(notes, fmt.Sprintf("%s had already forgotten this runner (%v); there was nothing to retire", out.Connection.URL, err))
		case force:
			out.Outcome, out.Refused = LeftBehind, err
		default:
			return out, notes, fmt.Errorf("%s would not retire this runner (%w) — nothing was removed and the connection is stopped until `yad daemon restart`, so run `yad disconnect %s` again when the hub answers, or `yad disconnect %s --force` to remove the credential and the connection anyway and retire this runner at the hub by hand", out.Connection.URL, err, name, name)
		}
	}

	if d.Ended != nil {
		if err := d.Ended(ctx); err != nil {
			notes = append(notes, "the running daemon did not close this hub's sessions here ("+err.Error()+"); `yad sessions` lists them and `yad sessions close` closes one")
		}
	}
	if err := p.DeleteCredential(name); err != nil {
		return out, notes, fmt.Errorf("%s, but its credential could not be removed: %w — delete the file by hand%s", out.standing(), err, out.andRetire())
	}
	next := cfg
	next.Connections = slices.Delete(slices.Clone(cfg.Connections), i, i+1)
	if err := config.Save(p, next); err != nil {
		return out, notes, fmt.Errorf("%s and its credential is gone, but %s could not be written: %w — remove the [[connection]] entry named %q by hand%s", out.standing(), p.ConfigFile(), err, name, out.andRetire())
	}
	out.Remaining = len(next.Connections)
	return out, notes, nil
}

// alreadyGone is a hub that has no registration to retire: it does not know
// this runner, or will not take the credential it was offered. A credential
// the hub refuses cannot be made any deader by keeping it.
func alreadyGone(err error) bool {
	var se *hubclient.StatusError
	return errors.As(err, &se) &&
		(se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden || se.Status == http.StatusNotFound)
}
