package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
)

// HubOutcome is what became of this runner's registration at the hub a
// disconnect left. Three, because what the owner does next differs in each:
// nothing, nothing, or retire the runner at the hub by hand.
type HubOutcome int

const (
	// HubRetired — the hub deregistered the runner at this request.
	HubRetired HubOutcome = iota + 1
	// HubAlreadyRetired — the hub itself answered that it does not know the
	// credential: a disconnect whose local half did not finish, run again,
	// or a registration the hub had dropped on its own. Only the hub's own
	// answer makes this; a credential that could not be read here never
	// does, because a credential nobody could send may be a live one.
	HubAlreadyRetired
	// HubStillRegistered — --force went past a hub that could not be
	// reached, refused, or could not be asked, so the runner is still
	// registered there.
	HubStillRegistered
)

// Disconnection is what Disconnect did.
type Disconnection struct {
	Connection config.Connection
	RunnerID   string
	Hub        HubOutcome
	// Refusal is why the hub was left registered, for HubStillRegistered.
	Refusal error
	// Remaining is how many connections config.toml is left with.
	Remaining int
}

// NotConnectedError is a name config.toml has no connection for.
type NotConnectedError struct {
	Name  string
	Names []string // the connections it has
	File  string
}

func (e *NotConnectedError) Error() string {
	if len(e.Names) == 0 {
		return fmt.Sprintf("%s lists no connection called %q, and none at all", e.File, e.Name)
	}
	return fmt.Sprintf("%s lists no connection called %q — it has %s", e.File, e.Name, strings.Join(e.Names, ", "))
}

// Disconnect retires this runner at one hub and removes the connection here:
// the hub deregisters it — the runs it held there end lost, its sessions
// there close and the runs queued in them end (decision 0069, DEV-77) — and
// then the connection goes from config.toml and its credential file is
// deleted, in that order. What a running daemon holds of it is the caller's
// to ask about: the CLI never writes the state database (decision 0043).
//
// The hub comes first, and nothing here is removed until it has answered:
// a credential removed here can never ask the hub again, a hub down for a
// minute is likelier than one gone for good, and a retry costs nothing.
// force goes on anyway, for a hub that is gone or will never agree, and the
// runner stays registered there. A hub answering that it does not know the
// credential has nothing left to retire, and the removal goes on without
// force; that is how running this again after a half-finished removal
// finishes it.
//
// config.toml goes before the credential file. A failure between the two
// leaves a credential no connection names, which the next `yad disconnect`
// of that name deletes; the other order would leave a connection with no
// credential, which cannot ask the hub whether it is retired.
func Disconnect(ctx context.Context, p config.Paths, name string, force bool, reason string) (Disconnection, error) {
	var out Disconnection
	if err := config.ValidName(name); err != nil {
		return out, fmt.Errorf("connection name: %w", err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		return out, err
	}
	i := slices.IndexFunc(cfg.Connections, func(c config.Connection) bool { return c.Name == name })
	if i < 0 {
		nc := &NotConnectedError{Name: name, File: p.ConfigFile()}
		for _, c := range cfg.Connections {
			nc.Names = append(nc.Names, c.Name)
		}
		return out, nc
	}
	out.Connection = cfg.Connections[i]
	shown := config.RedactURL(out.Connection.URL)
	again := p.Command("disconnect", name)
	forced := p.Command("disconnect", name, "--force")

	// Each way the hub cannot be asked is refused alike: nothing has been
	// retired, and the owner decides whether to go on without the hub.
	cannotAsk := func(why error) error {
		if force {
			out.Hub, out.Refusal = HubStillRegistered, why
			return nil
		}
		return fmt.Errorf("%s was not asked to retire this runner, because %w. Nothing was retired or removed — fix that and run `%s` again, or `%s` removes the connection here anyway and leaves the runner registered at %s, to retire there",
			shown, why, again, forced, shown)
	}
	id, ok, err := p.KnownRunnerID()
	if err == nil && !ok {
		err = errors.New("this profile has no runner id")
	}
	var client *hubclient.Client
	if err != nil {
		err = cannotAsk(fmt.Errorf("the runner's id could not be read (%w)", err))
	} else if cred, cerr := p.Credential(name); cerr != nil {
		err = cannotAsk(fmt.Errorf("its credential could not be read (%w)", cerr))
	} else if client, cerr = hubclient.New(out.Connection.URL, cred); cerr != nil {
		err = cannotAsk(fmt.Errorf("its URL is not one a runner calls (%w)", cerr))
	}
	if err != nil {
		return out, err
	}
	out.RunnerID = id
	if client != nil {
		switch derr := client.Deregister(ctx, id, reason); {
		case derr == nil:
			out.Hub = HubRetired
		case hubForgot(derr):
			out.Hub = HubAlreadyRetired
		case force:
			out.Hub, out.Refusal = HubStillRegistered, derr
		default:
			var se *hubclient.StatusError
			if errors.As(derr, &se) {
				return out, fmt.Errorf("%s refused to retire this runner (%w). Nothing was retired or removed — run `%s` again once the hub takes it, or `%s` removes the connection here anyway and leaves the runner registered at %s, to retire there",
					shown, derr, again, forced, shown)
			}
			return out, fmt.Errorf("%s could not be reached to retire this runner (%w). Nothing was retired or removed — run `%s` again when it answers, or `%s` removes the connection here anyway and leaves the runner registered at %s, to retire there",
				shown, derr, again, forced, shown)
		}
	}

	// The same command, --force and all: a retry without it would stop at the
	// hub it went past.
	rerun := again
	if force {
		rerun = forced
	}
	now, err := config.Update(ctx, p, func(c *config.Config) (bool, error) {
		n := len(c.Connections)
		c.Connections = slices.DeleteFunc(c.Connections, func(k config.Connection) bool { return k.Name == name })
		return len(c.Connections) != n, nil
	})
	if err != nil {
		return out, fmt.Errorf("%s, but %s could not be written (%w), so nothing here was removed — run `%s` again: %s",
			out.standing(shown), p.ConfigFile(), err, rerun, out.retry())
	}
	out.Remaining = len(now.Connections)
	if err := p.DeleteCredential(name); err != nil {
		return out, fmt.Errorf("%s, and %q is out of %s, but its credential file %s could not be deleted (%w) — run `%s` again, or delete the file",
			out.standing(shown), name, p.ConfigFile(), p.CredentialFile(name), err, rerun)
	}
	return out, nil
}

// hubForgot is the hub saying, in the protocol's own envelope, that it does
// not know this credential. A bare 401 is not that — a proxy in front of the
// hub can answer one — and neither is a 403, which yad hub gives a credential
// that is live but belongs to another runner id.
func hubForgot(err error) bool {
	var se *hubclient.StatusError
	if !errors.As(err, &se) || se.Protocol == nil {
		return false
	}
	switch se.Protocol.Code {
	case v1.CodeRunnerRevoked:
		return true
	case v1.CodeUnauthorized:
		return se.Status == http.StatusUnauthorized
	}
	return false
}

// standing is where the hub stands, for a message about a failure after it
// answered.
func (d Disconnection) standing(shown string) string {
	switch d.Hub {
	case HubRetired:
		return fmt.Sprintf("%s has retired this runner (%s)", shown, d.RunnerID)
	case HubAlreadyRetired:
		return fmt.Sprintf("%s had already retired this runner (%s)", shown, d.RunnerID)
	}
	return fmt.Sprintf("%s still has this runner registered — retire it there", shown)
}

// retry is what running the same disconnect again will find at the hub.
func (d Disconnection) retry() string {
	if d.Hub == HubStillRegistered {
		return "the hub is asked again, and --force goes past it again"
	}
	return "the hub will answer that it no longer knows the credential, and the removal here follows"
}
