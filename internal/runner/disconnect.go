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

// Disconnection is what `yad disconnect` did.
type Disconnection struct {
	Connection config.Connection
	// Deregistered is whether the hub retired this runner's registration at
	// our asking. False without an error means there was nothing to retire:
	// the hub had already forgotten this runner, or its credential could not
	// be read here.
	Deregistered bool
	// Forced is the hub's refusal that --force went past.
	Forced error
	// Remaining is how many connections config.toml is left with.
	Remaining int
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
// stopped, when set, is called once the hub has answered and before anything
// is removed: `yad disconnect` asks the daemon there to stop the connection
// and close its sessions. Its failure is a note and never fatal — by then the
// registration is gone, and a credential kept for a hub that has forgotten it
// is a secret nobody will ever clean up.
//
// The credential goes before the config entry. A deregistered credential is
// worthless, so one orphaned by a failure in between costs nothing, while a
// live credential with no connection naming it is a secret nobody would find.
func Disconnect(ctx context.Context, p config.Paths, name string, force bool, stopped func(context.Context) error) (Disconnection, []string, error) {
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
	out := Disconnection{Connection: cfg.Connections[i]}

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
			out.Deregistered = true
		case alreadyGone(err):
			notes = append(notes, fmt.Sprintf("%s had already forgotten this runner (%v); there was nothing to retire", out.Connection.URL, err))
		case force:
			out.Forced = err
		default:
			return out, notes, fmt.Errorf("%s would not retire this runner (%w) — nothing was removed, so run `yad disconnect %s` again when the hub answers, or `yad disconnect %s --force` to remove the credential and the connection anyway and retire this runner at the hub by hand", out.Connection.URL, err, name, name)
		}
	}

	if stopped != nil {
		if err := stopped(ctx); err != nil {
			notes = append(notes, "the running daemon was not told to stop this connection ("+err.Error()+"); it stops at its next sync, when the hub refuses the credential — `yad daemon restart` ends it now")
		}
	}
	if err := p.DeleteCredential(name); err != nil {
		return out, notes, fmt.Errorf("this runner is deregistered at %s but its credential could not be removed: %w — delete the file by hand; it is no longer accepted anywhere", out.Connection.URL, err)
	}
	next := cfg
	next.Connections = slices.Delete(slices.Clone(cfg.Connections), i, i+1)
	if err := config.Save(p, next); err != nil {
		return out, notes, fmt.Errorf("this runner is deregistered at %s and its credential is gone, but %s could not be written: %w — remove the [[connection]] entry named %q by hand", out.Connection.URL, p.ConfigFile(), err, name)
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
