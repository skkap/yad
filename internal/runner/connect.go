package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
)

// Connect registers this runner with the hub at hubURL, exchanging the
// one-time registration token for a runner credential, and records the
// connection: the credential in credentials/<name> at 0600, the connection in
// config.toml. The token is used for the one request and kept nowhere.
//
// Connecting again under the same name and URL registers again, with a token
// the hub issued for this runner, which gives the runner a fresh credential —
// the recovery for a lost or leaked one.
// Connect returns any notes the owner should see that are not failures —
// today, that the account states could not be read, so the document
// registered reports every configured account free. They are returned rather
// than logged because this runs as a CLI command with no logger, and swallowed
// they would leave an owner debugging refused runs with no thread to pull.
func Connect(ctx context.Context, p config.Paths, hubURL, token, name string) (config.Connection, v1.RegisterResponse, []string, error) {
	var notes []string
	var none v1.RegisterResponse
	token = strings.TrimSpace(token)
	if token == "" {
		return config.Connection{}, none, notes, errors.New("no registration token — create one at the hub (`yad hub token create`, or its Add runner) and pass it with --token")
	}
	if err := config.CheckHubURL(hubURL); err != nil {
		return config.Connection{}, none, notes, err
	}
	if name == "" {
		name = NameFromURL(hubURL)
	}
	cfg, err := config.Load(p)
	if err != nil {
		return config.Connection{}, none, notes, err
	}
	conn := config.Connection{Name: name, URL: hubURL}
	// Every message below names the hub. CheckHubURL has refused userinfo,
	// but a query or fragment is where a signed URL keeps its signature.
	shown := config.RedactURL(hubURL)
	existing := -1
	for i, c := range cfg.Connections {
		switch {
		case c.Name == name && c.URL != hubURL:
			return conn, none, notes, fmt.Errorf("connection %q already points at %s — pick another --name for this hub", name, config.RedactURL(c.URL))
		case c.Name != name && c.URL == hubURL:
			// The command is for pasting, and one carrying "?redacted" in place
			// of the owner's query would register against a different URL, so
			// it names the URL rather than quoting it.
			again := shown
			if shown != hubURL {
				again = "<the same URL>"
			}
			return conn, none, notes, fmt.Errorf("this runner is already connected to %s as %q — a runner has one connection per hub; to register it again, run `%s`",
				shown, c.Name, config.YadCommand(p.Profile, "connect", again, "--name", c.Name, "--token", "<new token>"))
		case c.Name == name:
			existing, conn = i, c
		}
	}
	// Checked before the token is spent: a config that cannot be saved would
	// otherwise burn the token and leave the hub with a runner nobody can use.
	next := cfg
	next.Connections = append([]config.Connection(nil), cfg.Connections...)
	if existing < 0 {
		next.Connections = append(next.Connections, conn)
	}
	if err := next.Validate(); err != nil {
		return conn, none, notes, err
	}

	if err := p.Ensure(); err != nil {
		return conn, none, notes, err
	}
	id, err := p.RunnerID()
	if err != nil {
		return conn, none, notes, err
	}
	client, err := hubclient.New(hubURL, "")
	if err != nil {
		return conn, none, notes, err
	}
	// A runner registers with the accounts the owner configured, whatever
	// state they are in: a harness whose accounts all need login is still a
	// runner a hub should know about.
	// A state database one migration behind — this binary newer than the
	// daemon that owns it — must not stop a registration. cmd_daemon takes the
	// same failure the same way, and capability.Build falls back to reporting
	// the owner's configured labels as free.
	accounts, err := account.Read(ctx, p, cfg)
	if err != nil {
		// Not fatal: a state database this binary cannot read — one migration
		// behind, because the daemon has not restarted — must not stop a
		// registration, and capability.Build falls back to the owner's
		// configured labels. But the error carries the way out, so it is
		// handed back rather than dropped.
		notes = append(notes, "account states could not be read, so every configured account is registered as free: "+err.Error())
		accounts = nil
	}
	res, err := client.Register(ctx, token, v1.RegisterRequest{Capabilities: capability.Build(ctx, id, cfg, accounts)})
	if err != nil {
		return conn, none, notes, fmt.Errorf("register with %s: %w", shown, err)
	}
	if res.RunnerCredential == "" {
		return conn, none, notes, fmt.Errorf("%s answered register without a runner credential — it is not a working YAD hub", shown)
	}
	if err := p.SaveCredential(name, res.RunnerCredential); err != nil {
		return conn, none, notes, fmt.Errorf("the hub registered this runner but the credential could not be saved (%w) — fix the config directory and connect again with a new token", err)
	}
	if existing < 0 {
		if err := config.Save(p, next); err != nil {
			// A credential with no connection naming it is a secret nobody
			// will use or clean up.
			return conn, none, notes, errors.Join(fmt.Errorf("save %s: %w — connect again with a new token", p.ConfigFile(), err), p.DeleteCredential(name))
		}
	}
	return conn, res, notes, nil
}

var notName = regexp.MustCompile(`[^a-z0-9_-]+`)

// NameFromURL derives a connection name from the hub's host, so
// `yad connect https://zumino.cc/api/yad/v1` needs no --name.
func NameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "hub"
	}
	name := strings.Trim(notName.ReplaceAllString(strings.ToLower(u.Hostname()), "-"), "-_")
	if len(name) > 64 {
		name = strings.TrimRight(name[:64], "-_")
	}
	if name == "" {
		return "hub"
	}
	return name
}
